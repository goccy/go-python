package python

// Interpreter is a hand-written, multi-interpreter API layered on top of the
// generated single-global binding in cpython.go. Each Interpreter owns its own
// wasm module (independent linear memory + WASI host) and one CPython runtime,
// so several interpreters run concurrently and in isolation. It also exposes
// the WASI sandbox controls (preopen dir, env, filesystem/network policy
// hooks) and the interrupt primitive.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	wasm2go "github.com/goccy/pythonwasm2go"
	"github.com/goccy/pythonwasm2go/base"
)

// EvalResult is the decoded form of the JSON document py_eval returns.
type EvalResult struct {
	Ok     bool   `json:"ok"`
	Repr   string `json:"repr"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error"`
}

// Config configures a new Interpreter's sandbox. The zero value is a usable
// (but unsandboxed-filesystem) interpreter: no host env is leaked, but the
// preopen defaults to the host "/" until PreopenDir is set.
type Config struct {
	// StdlibDir is the directory holding the Python standard library (the
	// Lib/ tree). It becomes the sole module search path. Required for any
	// non-trivial import (encodings is needed even for startup).
	StdlibDir string
	// PreopenDir scopes the guest filesystem root ("/") to this host
	// directory. Empty leaves the host "/" visible (no scoping).
	PreopenDir string
	// Env is the environment the guest sees. nil means an empty
	// environment — the host process os.Environ() is NOT leaked.
	Env []string
	// FSAccess, when non-nil, is a per-open/create/unlink whitelist. It
	// receives the guest path (relative to the preopen) and whether the
	// access is a write; returning false denies it (the guest sees a
	// PermissionError / OSError EACCES).
	FSAccess func(path string, write bool) bool
	// NetAccess, when non-nil, gates the socket accept/recv/send surface.
	// op is "accept"/"recv"/"send"; returning false denies it.
	NetAccess func(op string) bool
	// Dial, when non-nil, is the OUTBOUND-connection whitelist. It is called
	// with ("tcp", dotted-quad IP, port) before each connect; returning false
	// denies the connection (the guest sees a connect error). When nil, all
	// outbound connections are allowed.
	Dial func(network, ip string, port int) bool
	// Resolve, when non-nil, is the name-resolution whitelist. It is called
	// with the host being resolved before each lookup; returning false denies
	// it (the guest sees a name-resolution error). This is where a hostname
	// policy such as "block example.com" is enforced. When nil, all lookups
	// are allowed.
	Resolve func(host string) bool
	// Stdin, when non-nil, backs the guest's fd 0 (sys.stdin / input()).
	// Defaults to an empty stream (the host process stdin is NOT used).
	Stdin io.Reader
	// Stdout, when non-nil, receives the guest's fd 1 writes (os-level
	// stdout, e.g. os.write(1, ...)). Note: Python print() output is also
	// captured into EvalResult.Stdout by the bridge; Stdout here is the
	// raw fd sink, used for streaming and by the CLI front-end.
	Stdout io.Writer
	// Stderr, when non-nil, receives the guest's fd 2 writes.
	Stderr io.Writer
	// MaxMemoryBytes, when > 0, caps this interpreter's wasm linear memory.
	// A guest allocation that would grow memory past this limit fails
	// (memory.grow returns -1 -> the C allocator sees ENOMEM -> Python raises
	// MemoryError) instead of growing the host process unbounded. Rounded
	// down to a multiple of the 64 KiB wasm page size; values below the
	// module's initial memory are ignored.
	MaxMemoryBytes int
	// MemoryReserveBytes, when > 0, is the initial linear-memory slice
	// capacity reserved for this interpreter. CPython's wasm linear memory
	// grows during boot; if the initial slice has no spare capacity the first
	// grow reallocates and copies the whole linear memory, which makes the
	// (otherwise untouched, zero) C-stack region resident and inflates RSS.
	// Reserving capacity up front makes those grows zero-copy reslices, so a
	// freshly-booted interpreter's resident memory drops dramatically. The
	// reservation is virtual address space, not resident memory — untouched
	// pages stay non-resident — so a generous value is cheap. It is clamped up
	// to the module's minimum memory size. When 0, a default headroom is used
	// (NewWithWASI), which already covers a normal boot.
	MemoryReserveBytes int
	// Exec, when non-nil, is the subprocess whitelist. It is called before
	// every process spawn (subprocess.run/Popen via posix_spawn) with the
	// executable path and full argv; returning false denies the spawn (the
	// guest sees a PermissionError). Spawning runs a HOST binary, so a sandbox
	// that allows subprocess should set this to a strict allow-list. When nil,
	// all spawns are permitted (subject to host-subprocess being built in).
	Exec func(path string, argv []string) bool
	// FS, when non-nil, is the filesystem backend this interpreter sees as its
	// entire guest filesystem ("/"). Every file operation (open/read/write/
	// stat/mkdir/readdir/...) is routed to it, so giving two interpreters
	// separate FS values isolates them completely — one cannot see another's
	// files. Use NewStdlibMemFS() for an in-memory FS pre-loaded with the
	// standard library. When FS is set, PreopenDir is ignored and StdlibDir
	// defaults to "/" (the FS root). When nil, the default os-backed filesystem
	// (optionally scoped by PreopenDir) is used.
	FS FS
}

// Interpreter is one isolated CPython interpreter.
type Interpreter struct {
	m    *Module
	wasi *base.WasiStubs
	h    uint64
}

// NewInterpreter builds a fresh wasm instance, applies the sandbox config,
// initializes the CPython runtime, and returns a ready interpreter.
func NewInterpreter(cfg Config) (inst *Interpreter, err error) {
	// Resolve the standard library location:
	//   - custom FS backend: the stdlib must live inside it; default the search
	//     path to its root ("/"). (Use NewStdlibMemFS to pre-load it.)
	//   - no FS and no StdlibDir: extract the embedded stdlib to a temp dir on
	//     the default os filesystem.
	if cfg.FS != nil {
		if cfg.StdlibDir == "" {
			cfg.StdlibDir = "/"
		}
	} else if cfg.StdlibDir == "" {
		dir, sErr := ExtractStdlib()
		if sErr != nil {
			return nil, fmt.Errorf("extract embedded stdlib: %w", sErr)
		}
		cfg.StdlibDir = dir
	}

	m := &Module{}
	wasi := base.DefaultWASI()
	// Sandbox by default: do not leak the host environment.
	wasi.SetEnv(cfg.Env)
	if cfg.FS != nil {
		wasi.SetFS(cfg.FS)
	} else if cfg.PreopenDir != "" {
		wasi.SetPreopenDir(cfg.PreopenDir)
	}
	if cfg.FSAccess != nil {
		wasi.SetFSAccessHook(cfg.FSAccess)
	}
	if cfg.NetAccess != nil {
		wasi.SetNetAccessHook(cfg.NetAccess)
	}
	if cfg.Dial != nil {
		wasi.SetDialHook(cfg.Dial)
	}
	if cfg.Resolve != nil {
		wasi.SetResolveHook(cfg.Resolve)
	}
	if cfg.Exec != nil {
		wasi.SetExecHook(cfg.Exec)
	}
	// Sandbox stdio by default: an unset stream does NOT fall through to the
	// host process stdio. Stdin defaults to empty (immediate EOF), stdout and
	// stderr to discard. (Python-level print() is still captured into
	// EvalResult by the bridge; these back the raw guest fds 0/1/2.)
	if cfg.Stdin != nil {
		wasi.SetStdin(cfg.Stdin)
	} else {
		wasi.SetStdin(bytes.NewReader(nil))
	}
	if cfg.Stdout != nil {
		wasi.SetStdout(cfg.Stdout)
	} else {
		wasi.SetStdout(io.Discard)
	}
	if cfg.Stderr != nil {
		wasi.SetStderr(cfg.Stderr)
	} else {
		wasi.SetStderr(io.Discard)
	}
	if cfg.MemoryReserveBytes > 0 {
		m.g = wasm2go.NewWithWASIReserve(wasi, envStubs{m: m}, cfg.MemoryReserveBytes)
	} else {
		m.g = wasm2go.NewWithWASI(wasi, envStubs{m: m})
	}
	// Cap linear-memory growth so a runaway allocation raises MemoryError in
	// the guest instead of growing the host process unbounded. Round down to a
	// wasm page; ignore values below the module's initial memory.
	if cfg.MaxMemoryBytes > 0 {
		const wasmPage = 65536
		max := uint64(cfg.MaxMemoryBytes) / wasmPage * wasmPage
		if max >= uint64(len(wasm2go.Memory(m.g))) {
			m.g.MaxMem = max
		}
	}

	// Mirror initModule: run the reactor _initialize + wasmify init under a
	// recover so a C++ static-initializer trap surfaces as an error.
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("instance init panicked: %v", r)
			}
		}()
		wasm2go.Initialize(m.g)
		_ = wasm2go.WasmInit(m.g)
	}()
	if err != nil {
		return nil, err
	}

	inst = &Interpreter{m: m, wasi: wasi}
	h, err := inst.pyNew(cfg.StdlibDir)
	if err != nil {
		return nil, fmt.Errorf("py_new: %w", err)
	}
	if h == 0 {
		return nil, fmt.Errorf("py_new returned 0 (interpreter init failed; check StdlibDir=%q)", cfg.StdlibDir)
	}
	inst.h = h
	return inst, nil
}

// Eval compiles and runs src in this interpreter's persistent globals and
// returns the structured result. A Python-level exception is reported via
// EvalResult.Ok=false / .Error, not as a Go error; a Go error indicates a
// host/transport failure (a wasm trap, encoding problem, ...).
func (i *Interpreter) Eval(src string) (EvalResult, error) {
	var buf []byte
	buf = pbAppendUint64(buf, 1, i.h)
	buf = pbAppendString(buf, 2, src)
	resp, err := i.m.invoke(0, 2, buf, wasm2go.Inv_0_2)
	if err != nil {
		return EvalResult{}, err
	}
	if e := pbExtractError(resp); e != nil {
		return EvalResult{}, e
	}
	js := readScalarAtField(resp, 1, (*pbReader).readString)
	var r EvalResult
	if err := json.Unmarshal([]byte(js), &r); err != nil {
		return EvalResult{}, fmt.Errorf("decode eval result %q: %w", js, err)
	}
	return r, nil
}

// Close finalizes the interpreter. The Interpreter must not be used afterward.
func (i *Interpreter) Close() error {
	var buf []byte
	buf = pbAppendUint64(buf, 1, i.h)
	resp, err := i.m.invoke(0, 1, buf, wasm2go.Inv_0_1)
	if err != nil {
		return err
	}
	return pbExtractError(resp)
}

// scalarCall is the shared shape for the three address/obj accessors.
func (i *Interpreter) scalarCall(mid int32, call func(*base.Module, int32, int32) (int64, error)) (uint32, error) {
	var buf []byte
	buf = pbAppendUint64(buf, 1, i.h)
	resp, err := i.m.invoke(0, mid, buf, call)
	if err != nil {
		return 0, err
	}
	if e := pbExtractError(resp); e != nil {
		return 0, e
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint32), nil
}

func (i *Interpreter) pyNew(stdlibDir string) (uint64, error) {
	var buf []byte
	buf = pbAppendString(buf, 1, stdlibDir)
	resp, err := i.m.invoke(0, 5, buf, wasm2go.Inv_0_5)
	if err != nil {
		return 0, err
	}
	if e := pbExtractError(resp); e != nil {
		return 0, e
	}
	return readScalarAtField(resp, 1, (*pbReader).readUint64), nil
}

// Interrupt raises KeyboardInterrupt in a running evaluation WITHOUT
// executing any wasm/C code on this instance. It writes the async-exception
// object and sets the eval-breaker bit directly in linear memory, mirroring
// PyThreadState_SetAsyncExc. Safe to call from another goroutine while Eval
// is running a long/infinite loop — CPython checks the eval-breaker on every
// bytecode back-edge and raises at the next iteration.
//
// The three addresses are fetched here via normal calls, so callers that want
// to interrupt a loop must either call Interrupt from a separate goroutine
// (this call cannot acquire the per-instance lock while Eval holds it — see
// PrepareInterrupt) or pre-fetch them with PrepareInterrupt before starting
// the loop.
func (i *Interpreter) Interrupt() error {
	ip, err := i.PrepareInterrupt()
	if err != nil {
		return err
	}
	ip.Fire()
	return nil
}

// Interrupter holds the pre-resolved linear-memory addresses needed to raise
// KeyboardInterrupt. Resolve it with PrepareInterrupt BEFORE starting the
// loop you intend to interrupt (resolving needs the instance lock, which a
// running Eval holds), then call Fire from a watchdog goroutine.
type Interrupter struct {
	m            *Module
	asyncExcAddr uint32
	breakerAddr  uint32
	kbdObj       uint32
}

// PrepareInterrupt resolves the interrupt addresses up front.
func (i *Interpreter) PrepareInterrupt() (*Interrupter, error) {
	ae, err := i.scalarCall(0, wasm2go.Inv_0_0)
	if err != nil {
		return nil, err
	}
	be, err := i.scalarCall(3, wasm2go.Inv_0_3)
	if err != nil {
		return nil, err
	}
	ko, err := i.scalarCall(4, wasm2go.Inv_0_4)
	if err != nil {
		return nil, err
	}
	return &Interrupter{m: i.m, asyncExcAddr: ae, breakerAddr: be, kbdObj: ko}, nil
}

// Fire performs the memory writes that raise KeyboardInterrupt. It does not
// take the instance lock — that is the point: it runs concurrently with a
// busy Eval. The eval-breaker is read-modify-written with the async bit (8 =
// _PY_ASYNC_EXCEPTION_BIT); a single writer makes the non-atomic RMW safe.
func (ip *Interrupter) Fire() {
	mem := wasm2go.Memory(ip.m.g)
	binary.LittleEndian.PutUint32(mem[ip.asyncExcAddr:], ip.kbdObj)
	v := binary.LittleEndian.Uint32(mem[ip.breakerAddr:])
	binary.LittleEndian.PutUint32(mem[ip.breakerAddr:], v|8)
}
