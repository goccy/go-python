// Package internal holds the machinery under the public go-python API: the
// generated wasm2go binding (python.go), the per-instance bridge transport,
// the handle-release queue, the context-cancellation watchdog, and the
// callback dispatch the Python -> Go function bridge rides on. Nothing here
// is application API — the public package wraps a *Python from this package
// and exposes only what users should call.
package internal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	wasm2go "github.com/goccy/pythonwasm2go"
	"github.com/goccy/pythonwasm2go/base"
)

// Method IDs of the python bridge service (service 0), in the order the proto
// declares them (alphabetical over the py.h exports). ONE added export
// renumbers every later id — methodids_test.go checks this table against the
// regenerated python.go, so a py.h change that is not mirrored here fails the
// build's tests instead of dispatching to the wrong export.
const (
	midAsyncExcAddr         = 0
	midCall                 = 1
	midCallMethod           = 2
	midClose                = 3
	midContains             = 4
	midDelItem              = 5
	midDictGet              = 6
	midDictItems            = 7
	midDictKeys             = 8
	midDictValues           = 9
	midEval                 = 10
	midEvalBreakerAddr      = 11
	midGetAttr              = 12
	midGetItem              = 13
	midImport               = 14
	midIsInstance           = 15
	midIter                 = 16
	midKeyboardInterruptObj = 17
	midLen                  = 18
	midListAppend           = 19
	midNew                  = 20
	midNewDict              = 21
	midNewFrozenSet         = 22
	midNewFunction          = 23
	midNewList              = 24
	midNewSet               = 25
	midNewTuple             = 26
	midRelease              = 27
	midRepr                 = 28
	midSetAdd               = 29
	midSetDiscard           = 30
	midSetGoDispatcher      = 31
	midSetAttr              = 32
	midSetItem              = 33
	midStr                  = 34
)

// generatedMethodIDs maps each generated per-export wrapper in python.go to
// the id this package dispatches it under; methodids_test.go verifies it
// against the generated source.
var generatedMethodIDs = map[string]int32{
	"PyAsyncExcAddr":         midAsyncExcAddr,
	"PyCall":                 midCall,
	"PyCallMethod":           midCallMethod,
	"PyClose":                midClose,
	"PyContains":             midContains,
	"PyDelitem":              midDelItem,
	"PyDictGet":              midDictGet,
	"PyDictItems":            midDictItems,
	"PyDictKeys":             midDictKeys,
	"PyDictValues":           midDictValues,
	"PyEval":                 midEval,
	"PyEvalBreakerAddr":      midEvalBreakerAddr,
	"PyGetattr":              midGetAttr,
	"PyGetitem":              midGetItem,
	"PyImport":               midImport,
	"PyIsinstance":           midIsInstance,
	"PyIter":                 midIter,
	"PyKeyboardInterruptObj": midKeyboardInterruptObj,
	"PyLen":                  midLen,
	"PyListAppend":           midListAppend,
	"PyNew":                  midNew,
	"PyNewDict":              midNewDict,
	"PyNewFrozenset":         midNewFrozenSet,
	"PyNewFunction":          midNewFunction,
	"PyNewList":              midNewList,
	"PyNewSet":               midNewSet,
	"PyNewTuple":             midNewTuple,
	"PyRelease":              midRelease,
	"PyRepr":                 midRepr,
	"PySetAdd":               midSetAdd,
	"PySetDiscard":           midSetDiscard,
	"PySetGoDispatcher":      midSetGoDispatcher,
	"PySetattr":              midSetAttr,
	"PySetitem":              midSetItem,
	"PyStr":                  midStr,
}

// InstanceOptions is the resolved per-instance configuration. The public package
// maps its Config here after applying defaults (the FS arrives non-nil).
type InstanceOptions struct {
	StdlibDir string
	Env       []string
	FS        base.FS
	Dial      func(network, host, ip string, port int) bool
	Resolve   func(host string) bool
	Exec      func(path string, argv []string) bool
	Stdin     io.Reader
	Stdout    io.Writer
	Stderr    io.Writer

	MaxMemoryBytes     int
	MemoryReserveBytes int
}

// ExitError is an uncaught SystemExit the bridge caught cleanly: the
// interpreter unwound back to the call frame and stays usable.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("python: exit(%d)", e.Code) }

// ErrClosed is returned by every entry point once Close has run.
var ErrClosed = errors.New("python: instance is closed")

// pyAsyncExceptionBit is CPython's _PY_ASYNC_EXCEPTION_BIT in the
// eval-breaker word.
const pyAsyncExceptionBit = 8

// Python is one isolated CPython interpreter instance's machinery: the wasm
// module, the interpreter handle, the interrupt watchdog, the handle-release
// queue, and the callback dispatch.
type Python struct {
	m    *Module
	wasi *base.WasiStubs
	h    uint64

	// Interrupt plumbing, resolved once at New so a context cancellation can
	// fire without taking the instance lock a running call holds: the
	// linear-memory offsets of the main thread state's async_exc slot and
	// eval_breaker word, and the (immortal) KeyboardInterrupt class object.
	asyncExcAddr    uint32
	breakerAddr     uint32
	kbdInterruptObj uint32

	// UserHandler receives every callback: the Python->Go function bridge,
	// keyed by the function id. The public package sets it once at
	// construction, before any dispatcher registration.
	UserHandler func(methodID int32, req []byte) ([]byte, error)

	dispMu        sync.Mutex
	dispatcherSet bool

	// closed flips when Close runs; every entry point checks it so a
	// straggler — including a handle release racing a Close — errors out
	// instead of calling into a finalized runtime.
	closed atomic.Bool
	// pendingRel collects handle ids whose host wrappers were collected by
	// the Go GC. Finalizers ONLY append here (a pure host-side operation —
	// invoking the guest from the finalizer goroutine could block behind a
	// long Eval or hit a closed instance); the queue drains through one
	// batched guest call at the start of the next user-initiated op.
	relMu      sync.Mutex
	pendingRel []uint64
}

// Closed reports whether Close has run.
func (p *Python) Closed() bool { return p.closed.Load() }

// QueueRelease records a handle whose host wrapper is gone. Safe from any
// goroutine, including the runtime's finalizer goroutine.
func (p *Python) QueueRelease(id uint64) {
	p.relMu.Lock()
	p.pendingRel = append(p.pendingRel, id)
	p.relMu.Unlock()
}

// drainReleases releases every queued handle in one guest call. Runs on
// user-initiated entry points only. Best-effort: a failure re-queues nothing
// (the registry dies with the instance anyway).
func (p *Python) drainReleases() {
	p.relMu.Lock()
	ids := p.pendingRel
	p.pendingRel = nil
	p.relMu.Unlock()
	if len(ids) == 0 || p.closed.Load() {
		return
	}
	packed := make([]byte, 0, len(ids)*8)
	for _, id := range ids {
		packed = binary.LittleEndian.AppendUint64(packed, id)
	}
	buf := pbAppendUint64(nil, 1, p.h)
	buf = appendBuffer(buf, 2, packed)
	_, _ = p.m.invoke(0, midRelease, buf, wasm2go.Inv_0_27)
}

// New boots a fresh instance from opts.
func New(opts InstanceOptions) (p *Python, err error) {
	m := &Module{}
	wasi := buildWASI(opts)
	p = &Python{m: m, wasi: wasi}

	if err := p.boot(opts, wasi); err != nil {
		return nil, err
	}

	// Resolve the interrupt addresses up front so a context cancellation can
	// fire without calling into a busy instance.
	ae, err := p.addrOp(midAsyncExcAddr, wasm2go.Inv_0_0)
	if err != nil {
		return nil, fmt.Errorf("resolve async_exc address: %w", err)
	}
	be, err := p.addrOp(midEvalBreakerAddr, wasm2go.Inv_0_11)
	if err != nil {
		return nil, fmt.Errorf("resolve eval_breaker address: %w", err)
	}
	ko, err := p.addrOp(midKeyboardInterruptObj, wasm2go.Inv_0_17)
	if err != nil {
		return nil, fmt.Errorf("resolve KeyboardInterrupt object: %w", err)
	}
	var addrErr error
	base.AccessMemory(m.g, func(mem []byte) {
		// Both words live in CPython's runtime data, far below the initial
		// memory size; validate anyway so a surprising address surfaces here
		// as an error rather than as a wild write in the watchdog.
		for _, a := range [...]uint32{ae, be} {
			if uint64(a)+4 > uint64(len(mem)) {
				addrErr = fmt.Errorf("interrupt address %#x out of range (memory is %d bytes)", a, len(mem))
				return
			}
		}
	})
	if addrErr != nil {
		return nil, addrErr
	}
	p.asyncExcAddr, p.breakerAddr, p.kbdInterruptObj = ae, be, ko
	return p, nil
}

// buildWASI assembles the per-instance WASI host from the options. Sandbox
// by default: no host environment, no host stdio.
func buildWASI(opts InstanceOptions) *base.WasiStubs {
	wasi := base.DefaultWASI()
	wasi.SetEnv(opts.Env)
	if opts.FS != nil {
		wasi.SetFS(opts.FS)
	}
	// The capability hooks are FAIL-CLOSED: the runtime allows when a hook
	// is unset, so a nil hook installs an explicit deny-all here — the zero
	// configuration grants nothing.
	dial := opts.Dial
	if dial == nil {
		dial = func(string, string, string, int) bool { return false }
	}
	wasi.SetDialHook(dial)
	resolve := opts.Resolve
	if resolve == nil {
		resolve = func(string) bool { return false }
	}
	wasi.SetResolveHook(resolve)
	exec := opts.Exec
	if exec == nil {
		exec = func(string, []string) bool { return false }
	}
	wasi.SetExecHook(exec)
	// Sandbox stdio by default: an unset stream does NOT fall through to the
	// host process stdio. Stdin defaults to empty (immediate EOF), stdout and
	// stderr to discard. (print() output during Eval is still captured into
	// the eval envelope by the bridge; these back the raw guest fds 0/1/2.)
	if opts.Stdin != nil {
		wasi.SetStdin(opts.Stdin)
	} else {
		wasi.SetStdin(bytes.NewReader(nil))
	}
	if opts.Stdout != nil {
		wasi.SetStdout(opts.Stdout)
	} else {
		wasi.SetStdout(io.Discard)
	}
	if opts.Stderr != nil {
		wasi.SetStderr(opts.Stderr)
	} else {
		wasi.SetStderr(io.Discard)
	}
	return wasi
}

// boot brings the instance's wasm module up on a private linear memory: run
// the reactor initializer and py_new.
func (p *Python) boot(opts InstanceOptions, wasi *base.WasiStubs) (err error) {
	if opts.MemoryReserveBytes > 0 {
		p.m.g = wasm2go.NewWithWASIReserve(wasi, envStubs{m: p.m}, wasmifyStubs{m: p.m}, opts.MemoryReserveBytes)
	} else {
		p.m.g = wasm2go.NewWithWASI(wasi, envStubs{m: p.m}, wasmifyStubs{m: p.m})
	}
	// Cap linear-memory growth so a runaway allocation raises MemoryError in
	// the guest instead of growing the host process unbounded. Round down to
	// a wasm page; ignore values below the module's initial memory.
	if opts.MaxMemoryBytes > 0 {
		const wasmPage = 65536
		max := uint64(opts.MaxMemoryBytes) / wasmPage * wasmPage
		if max >= uint64(len(wasm2go.Memory(p.m.g))) {
			wasm2go.SetMaxMemory(p.m.g, max)
		}
	}

	// Run the reactor _initialize + wasmify init under a recover so a
	// static-initializer trap surfaces as an error.
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("instance init panicked: %v", r)
			}
		}()
		wasm2go.Initialize(p.m.g)
		_ = wasm2go.WasmInit(p.m.g)
	}()
	if err != nil {
		return err
	}

	h, err := p.pyNew(opts.StdlibDir)
	if err != nil {
		return fmt.Errorf("py_new: %w", err)
	}
	if h == 0 {
		return fmt.Errorf("py_new returned 0 (interpreter init failed; check StdlibDir=%q)", opts.StdlibDir)
	}
	p.h = h
	return nil
}

func (p *Python) pyNew(stdlibDir string) (uint64, error) {
	buf := pbAppendString(nil, 1, stdlibDir)
	resp, err := p.m.invoke(0, midNew, buf, wasm2go.Inv_0_20)
	if err != nil {
		return 0, err
	}
	if e := pbExtractError(resp); e != nil {
		return 0, e
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint64), nil
}

// addrOp runs one of the interrupt-address getters (h -> uint32).
func (p *Python) addrOp(mid int32, inv invoker) (uint32, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	resp, err := p.m.invoke(0, mid, buf, inv)
	if err != nil {
		return 0, err
	}
	if e := pbExtractError(resp); e != nil {
		return 0, e
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint32), nil
}

// invoker matches the generated per-method entry points (wasm2go.Inv_0_N).
type invoker = func(*base.Module, wptr, wptr) (int64, error)

// appendBuffer appends a (const char *, uint32_t len) parameter pair: the
// bytes at field f and their length at field f+1, which is how the generated
// bridge lays out the two C parameters.
func appendBuffer(buf []byte, f uint32, data []byte) []byte {
	buf = pbAppendBytes(buf, f, data)
	return pbAppendUint64(buf, f+1, uint64(len(data)))
}

// appendName appends a (const char *, uint32_t len) identifier pair.
func appendName(buf []byte, f uint32, name string) []byte {
	buf = pbAppendString(buf, f, name)
	return pbAppendUint64(buf, f+1, uint64(len(name)))
}

// valueOp sends one bridge operation and returns the raw response envelope
// bytes (the typed-value protocol documented in python-wasm's py.h; the
// public package decodes them). interrupted reports whether the ctx watchdog
// fired while the guest ran — a raised envelope then means the interruption,
// not a user-level exception.
func (p *Python) valueOp(ctx context.Context, mid int32, inv invoker, buf []byte) (resp []byte, interrupted bool, err error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if p.closed.Load() {
		return nil, false, ErrClosed
	}
	p.drainReleases()

	disarm := p.armInterrupt(ctx)
	out, invokeErr := p.m.invoke(0, mid, buf, inv)
	interrupted = disarm()

	if invokeErr != nil {
		return nil, interrupted, invokeErr
	}
	if e := pbExtractError(out); e != nil {
		return nil, interrupted, e
	}
	return readScalarAtField(out, 1, (*pbReader).readBytes), interrupted, nil
}

// EvalOp evaluates src in __main__ and returns the raw eval envelope.
func (p *Python) EvalOp(ctx context.Context, src string) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendString(buf, 2, src)
	return p.valueOp(ctx, midEval, wasm2go.Inv_0_10, buf)
}

// ImportOp imports the named module.
func (p *Python) ImportOp(ctx context.Context, name string) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = appendName(buf, 2, name)
	return p.valueOp(ctx, midImport, wasm2go.Inv_0_14, buf)
}

// CallOp calls the object behind callable with an encoded node list and
// kwargs list.
func (p *Python) CallOp(ctx context.Context, callable uint64, args, kwargs []byte) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, callable)
	buf = appendBuffer(buf, 3, args)
	buf = appendBuffer(buf, 5, kwargs)
	return p.valueOp(ctx, midCall, wasm2go.Inv_0_1, buf)
}

// CallMethodOp calls obj.name(*args, **kwargs).
func (p *Python) CallMethodOp(ctx context.Context, obj uint64, name string, args, kwargs []byte) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = appendName(buf, 3, name)
	buf = appendBuffer(buf, 5, args)
	buf = appendBuffer(buf, 7, kwargs)
	return p.valueOp(ctx, midCallMethod, wasm2go.Inv_0_2, buf)
}

// GetAttrOp is getattr(obj, name).
func (p *Python) GetAttrOp(ctx context.Context, obj uint64, name string) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = appendName(buf, 3, name)
	return p.valueOp(ctx, midGetAttr, wasm2go.Inv_0_12, buf)
}

// SetAttrOp is setattr(obj, name, val) with val an encoded node.
func (p *Python) SetAttrOp(ctx context.Context, obj uint64, name string, val []byte) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = appendName(buf, 3, name)
	buf = appendBuffer(buf, 5, val)
	return p.valueOp(ctx, midSetAttr, wasm2go.Inv_0_32, buf)
}

// GetItemOp is obj[key] with key an encoded node.
func (p *Python) GetItemOp(ctx context.Context, obj uint64, key []byte) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = appendBuffer(buf, 3, key)
	return p.valueOp(ctx, midGetItem, wasm2go.Inv_0_13, buf)
}

// SetItemOp is obj[key] = val.
func (p *Python) SetItemOp(ctx context.Context, obj uint64, key, val []byte) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = appendBuffer(buf, 3, key)
	buf = appendBuffer(buf, 5, val)
	return p.valueOp(ctx, midSetItem, wasm2go.Inv_0_33, buf)
}

// DelItemOp is del obj[key].
func (p *Python) DelItemOp(ctx context.Context, obj uint64, key []byte) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = appendBuffer(buf, 3, key)
	return p.valueOp(ctx, midDelItem, wasm2go.Inv_0_5, buf)
}

// objOp is the shared shape of the (h, obj) operations.
func (p *Python) objOp(ctx context.Context, mid int32, inv invoker, obj uint64) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	return p.valueOp(ctx, mid, inv, buf)
}

// LenOp is len(obj).
func (p *Python) LenOp(ctx context.Context, obj uint64) ([]byte, bool, error) {
	return p.objOp(ctx, midLen, wasm2go.Inv_0_18, obj)
}

// StrOp is str(obj).
func (p *Python) StrOp(ctx context.Context, obj uint64) ([]byte, bool, error) {
	return p.objOp(ctx, midStr, wasm2go.Inv_0_34, obj)
}

// ReprOp is repr(obj).
func (p *Python) ReprOp(ctx context.Context, obj uint64) ([]byte, bool, error) {
	return p.objOp(ctx, midRepr, wasm2go.Inv_0_28, obj)
}

// IterOp is list(obj).
func (p *Python) IterOp(ctx context.Context, obj uint64) ([]byte, bool, error) {
	return p.objOp(ctx, midIter, wasm2go.Inv_0_16, obj)
}

// DictKeysOp is list(obj.keys()).
func (p *Python) DictKeysOp(ctx context.Context, obj uint64) ([]byte, bool, error) {
	return p.objOp(ctx, midDictKeys, wasm2go.Inv_0_8, obj)
}

// DictValuesOp is list(obj.values()).
func (p *Python) DictValuesOp(ctx context.Context, obj uint64) ([]byte, bool, error) {
	return p.objOp(ctx, midDictValues, wasm2go.Inv_0_9, obj)
}

// DictItemsOp is list(obj.items()), flattened.
func (p *Python) DictItemsOp(ctx context.Context, obj uint64) ([]byte, bool, error) {
	return p.objOp(ctx, midDictItems, wasm2go.Inv_0_7, obj)
}

// keyOp is the shared shape of the (h, obj, key-node) operations.
func (p *Python) keyOp(ctx context.Context, mid int32, inv invoker, obj uint64, key []byte) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = appendBuffer(buf, 3, key)
	return p.valueOp(ctx, mid, inv, buf)
}

// ContainsOp is key in obj.
func (p *Python) ContainsOp(ctx context.Context, obj uint64, key []byte) ([]byte, bool, error) {
	return p.keyOp(ctx, midContains, wasm2go.Inv_0_4, obj, key)
}

// DictGetOp is obj[key] with the KeyError folded into an exists flag.
func (p *Python) DictGetOp(ctx context.Context, obj uint64, key []byte) ([]byte, bool, error) {
	return p.keyOp(ctx, midDictGet, wasm2go.Inv_0_6, obj, key)
}

// ListAppendOp appends each element of an encoded node list to a list.
func (p *Python) ListAppendOp(ctx context.Context, obj uint64, elems []byte) ([]byte, bool, error) {
	return p.keyOp(ctx, midListAppend, wasm2go.Inv_0_19, obj, elems)
}

// SetAddOp is obj.add(val).
func (p *Python) SetAddOp(ctx context.Context, obj uint64, val []byte) ([]byte, bool, error) {
	return p.keyOp(ctx, midSetAdd, wasm2go.Inv_0_29, obj, val)
}

// SetDiscardOp is obj.discard(val).
func (p *Python) SetDiscardOp(ctx context.Context, obj uint64, val []byte) ([]byte, bool, error) {
	return p.keyOp(ctx, midSetDiscard, wasm2go.Inv_0_30, obj, val)
}

// IsInstanceOp is isinstance(obj, cls).
func (p *Python) IsInstanceOp(ctx context.Context, obj, cls uint64) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = pbAppendUint64(buf, 2, obj)
	buf = pbAppendUint64(buf, 3, cls)
	return p.valueOp(ctx, midIsInstance, wasm2go.Inv_0_15, buf)
}

// newOp is the shared shape of the aggregate constructors (h, node list).
func (p *Python) newOp(ctx context.Context, mid int32, inv invoker, elems []byte) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = appendBuffer(buf, 2, elems)
	return p.valueOp(ctx, mid, inv, buf)
}

// NewListOp builds a list from an encoded node list.
func (p *Python) NewListOp(ctx context.Context, elems []byte) ([]byte, bool, error) {
	return p.newOp(ctx, midNewList, wasm2go.Inv_0_24, elems)
}

// NewTupleOp builds a tuple from an encoded node list.
func (p *Python) NewTupleOp(ctx context.Context, elems []byte) ([]byte, bool, error) {
	return p.newOp(ctx, midNewTuple, wasm2go.Inv_0_26, elems)
}

// NewSetOp builds a set from an encoded node list.
func (p *Python) NewSetOp(ctx context.Context, elems []byte) ([]byte, bool, error) {
	return p.newOp(ctx, midNewSet, wasm2go.Inv_0_25, elems)
}

// NewFrozenSetOp builds a frozenset from an encoded node list.
func (p *Python) NewFrozenSetOp(ctx context.Context, elems []byte) ([]byte, bool, error) {
	return p.newOp(ctx, midNewFrozenSet, wasm2go.Inv_0_22, elems)
}

// NewDictOp builds a dict from an encoded flat key/value node list.
func (p *Python) NewDictOp(ctx context.Context, items []byte) ([]byte, bool, error) {
	return p.newOp(ctx, midNewDict, wasm2go.Inv_0_21, items)
}

// NewFunctionOp materialises the host function bound under funcID as a
// builtin function named name.
func (p *Python) NewFunctionOp(ctx context.Context, name string, funcID int32) ([]byte, bool, error) {
	buf := pbAppendUint64(nil, 1, p.h)
	buf = appendName(buf, 2, name)
	buf = pbAppendInt32(buf, 4, funcID)
	return p.valueOp(ctx, midNewFunction, wasm2go.Inv_0_23, buf)
}

// armInterrupt starts the cancellation watchdog for ctx. It first STAGES the
// pending async exception — the KeyboardInterrupt class pointer is written
// into the thread state's async_exc slot, sequenced before the guest call
// starts, so it is visible to the guest long before the flag can be — and
// on ctx cancellation the watchdog trips the eval-breaker bit with a plain
// store into linear memory: no call into the (busy) instance. CPython tests
// the breaker on every bytecode back-edge and raises the staged exception.
//
// The returned disarm function MUST be called exactly once, after the
// guarded invoke returns; it reports whether the watchdog fired and clears
// the staging either way (the call may have finished before the guest
// consumed the interrupt, and a lingering flag would poison the next call).
// Receiving after close(stop) is deterministic: the watchdog sends exactly
// one value, and a true arrives strictly after its store.
func (p *Python) armInterrupt(ctx context.Context) (disarm func() bool) {
	done := ctx.Done()
	if done == nil {
		return func() bool { return false }
	}
	base.AccessMemory(p.m.g, func(mem []byte) {
		binary.LittleEndian.PutUint32(mem[p.asyncExcAddr:], p.kbdInterruptObj)
	})
	stop := make(chan struct{})
	fired := make(chan bool, 1)
	go func() {
		select {
		case <-done:
			p.fireInterrupt()
			fired <- true
		case <-stop:
			fired <- false
		}
	}()
	return func() bool {
		close(stop)
		f := <-fired
		p.clearInterrupt(f)
		return f
	}
}

// fireInterrupt sets _PY_ASYNC_EXCEPTION_BIT in the eval-breaker word (the
// exception object itself was staged by armInterrupt). It does not take the
// instance lock — that is the point: it runs concurrently with a busy call.
//
// The write happens inside base.AccessMemory, which holds the same lock the
// runtime's memory.grow takes to mutate the memory slice header or relocate
// its backing array, so the bit lands in the array the guest observes. The
// breaker word is read-modify-written by the guest with plain single-word
// accesses — that unsynchronised word is the eval-breaker protocol CPython
// defines, and the guest re-asserts its own bits on every pass, so the |=
// below is exactly as strong as CPython's native cross-thread signalling.
func (p *Python) fireInterrupt() {
	base.AccessMemory(p.m.g, func(mem []byte) {
		v := binary.LittleEndian.Uint32(mem[p.breakerAddr:])
		binary.LittleEndian.PutUint32(mem[p.breakerAddr:], v|pyAsyncExceptionBit)
	})
}

// clearInterrupt removes the staged exception and, when the watchdog fired,
// the breaker bit. Called after a call whose ctx was cancellable: if the
// guest consumed the interrupt both are already clear (CPython resets them
// when it raises), and if it finished first they would otherwise linger into
// the next call.
func (p *Python) clearInterrupt(fired bool) {
	base.AccessMemory(p.m.g, func(mem []byte) {
		binary.LittleEndian.PutUint32(mem[p.asyncExcAddr:], 0)
		if fired {
			v := binary.LittleEndian.Uint32(mem[p.breakerAddr:])
			binary.LittleEndian.PutUint32(mem[p.breakerAddr:], v&^pyAsyncExceptionBit)
		}
	})
}

// Close finalizes the instance. It must not be used afterward: every later
// op errors, and outstanding handles become inert. Idempotent.
func (p *Python) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	buf := pbAppendUint64(nil, 1, p.h)
	resp, err := p.m.invoke(0, midClose, buf, wasm2go.Inv_0_3)
	if err == nil {
		err = pbExtractError(resp)
	}
	return err
}

// WasiExitCode reports whether err is the guest terminating itself — a
// caught SystemExit (*ExitError) or a raw wasi proc_exit — and returns the
// exit status.
func WasiExitCode(err error) (int, bool) {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code, true
	}
	var we *base.WasiExitError
	if errors.As(err, &we) {
		return int(we.Code), true
	}
	return 0, false
}
