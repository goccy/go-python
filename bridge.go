package python

// The Go <-> Python function bridge.
//
// A call's arguments and return value cross the boundary as TYPED BINARY
// nodes (see wire.go): immutable built-ins by value with their kind, every
// other object by handle — the guest pins the actual object in a
// per-interpreter registry (deduplicated by object address, a strong
// reference held) and only the id crosses, surfacing in Go as a
// handle-backed Value. Passing it back dereferences to THE SAME object, so
// identity and mutation survive any number of round trips.
//
// Go -> Python: Call / CallMethod on any ObjectValue (a FunctionValue, a
// ClassValue, a callable instance) go through the py_call / py_call_method
// bridge exports. Python -> Go: NewFunction registers a Go function under
// an id and materialises a builtin function object in the guest that
// forwards its calls over the wasmify callback import, speaking the same
// node protocol; Bind additionally installs it in __main__ under a name.

import (
	"context"
	"errors"
	"fmt"
)

// PythonError is an uncaught Python exception surfaced by Eval, RunFile,
// Call, and the value operations.
type PythonError struct {
	// Type is the exception type's name as Python prints it: "ValueError",
	// "json.decoder.JSONDecodeError", "__main__.MyError".
	Type string
	// Message is str(exc).
	Message string
	// Traceback is the formatted traceback (traceback.format_exception),
	// ending with the "Type: message" line — what the interpreter would have
	// printed had the exception gone uncaught.
	Traceback string
	// Value is the exception instance itself, live in the interpreter: its
	// attributes (args, __cause__, ...) are reachable through the object
	// protocol, and returning the same *PythonError from a GoFunc re-raises
	// this exact instance. nil when the failure was raised by the bridge
	// rather than by Python code (e.g. a released handle).
	Value ObjectValue
}

func (e *PythonError) Error() string {
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

// GoFunc is a Go function callable from Python. args is the call's
// positional argument list and kwargs its keyword arguments, as typed
// Values (objects arrive as handle-backed Values); the returned Value
// becomes the call's return value (nil is None). Returning an error raises
// in the caller: a *PythonError carrying a live Value re-raises that exact
// exception instance, any other error raises RuntimeError with the error
// text. A panic in fn is contained and raised the same way.
//
// fn may call back into the instance (Eval, Call, value operations) — the
// invoke lock is released for the callback's duration, and a nested entry
// from the handler's own goroutine is safe. What deadlocks is HOST-side
// lock cycles: a handler that blocks on something only the code that
// initiated the outer call can provide (a mutex it holds, a channel it will
// only service after the call returns) waits forever. Keep handlers
// self-contained or hand work to other goroutines without waiting on the
// outer caller.
type GoFunc func(args []Value, kwargs map[string]Value) (Value, error)

// NewFunction materialises fn as a Python builtin function object named
// name (its __name__). The function is an ordinary first-class Python
// value: pass it as an argument, store it in a data structure, set it as an
// attribute, or install it under a name with Bind. It stays callable for
// the instance's lifetime; the guest may hold references to it long after
// the returned FunctionValue is dropped, so the Go side never releases fn.
func (p *Python) NewFunction(name string, fn GoFunc) (FunctionValue, error) {
	if fn == nil {
		return FunctionValue{}, fmt.Errorf("python: nil GoFunc for %q", name)
	}
	if err := p.ensureDispatcher(); err != nil {
		return FunctionValue{}, err
	}

	p.funcsMu.Lock()
	p.nextFuncID++
	id := p.nextFuncID
	p.funcs[id] = fn
	p.funcsMu.Unlock()

	resp, interrupted, err := p.raw.NewFunctionOp(context.Background(), name, id)
	if err != nil {
		return FunctionValue{}, err
	}
	v, err := p.decodeNodeResult(context.Background(), resp, interrupted)
	if err != nil {
		return FunctionValue{}, err
	}
	return As[FunctionValue](v)
}

// Bind makes fn callable from Python code as the global name in __main__ —
// the namespace Eval runs in — so `name(...)` works in any later Eval. It
// is NewFunction plus setattr(__main__, name, fn). Binding the same name
// again replaces the function (the previous one is unreferenced but its id
// stays allocated).
func (p *Python) Bind(name string, fn GoFunc) error {
	if name == "" {
		return fmt.Errorf("python: empty name for Bind")
	}
	f, err := p.NewFunction(name, fn)
	if err != nil {
		return err
	}
	ctx := context.Background()
	main, err := p.Import(ctx, "__main__")
	if err != nil {
		return err
	}
	return main.SetAttr(ctx, name, f)
}

// ensureDispatcher makes sure the instance's guest-side callback dispatcher
// is registered and this wrapper's function table exists.
func (p *Python) ensureDispatcher() error {
	p.funcsMu.Lock()
	if p.funcs == nil {
		p.funcs = map[int32]GoFunc{}
	}
	p.funcsMu.Unlock()
	return p.raw.EnsureDispatcher()
}

// handleUserCallback serves the Python->Go function bridge: methodID
// carries the bound function's id, req is the guest-encoded argument list
// followed by the kwargs list, and the returned bytes are the response.
// Failures are ALWAYS reported in-band (a Go error return would be
// protobuf-encoded by the generated dispatch and misparse guest-side).
func (p *Python) handleUserCallback(methodID int32, req []byte) ([]byte, error) {
	p.funcsMu.RLock()
	fn, ok := p.funcs[methodID]
	p.funcsMu.RUnlock()
	if !ok {
		return p.callbackRaised(fmt.Errorf("no Go function bound for id %d", methodID)), nil
	}
	r := &nodeReader{b: req}
	count := int(r.u32())
	if r.fail {
		return p.callbackRaised(fmt.Errorf("decode arguments: malformed argument list")), nil
	}
	args := make([]Value, 0, count)
	for i := 0; i < count; i++ {
		v, err := p.decodeNode(r)
		if err != nil {
			return p.callbackRaised(fmt.Errorf("decode arguments: %w", err)), nil
		}
		args = append(args, v)
	}
	var kwargs map[string]Value
	if kcount := int(r.u32()); kcount > 0 && !r.fail {
		kwargs = make(map[string]Value, kcount)
		for i := 0; i < kcount; i++ {
			name := r.lenString()
			v, err := p.decodeNode(r)
			if err != nil {
				return p.callbackRaised(fmt.Errorf("decode keyword arguments: %w", err)), nil
			}
			kwargs[name] = v
		}
	}
	result, err := safeCall(fn, args, kwargs)
	if err != nil {
		return p.callbackRaised(err), nil
	}
	enc, err := p.encodeValue([]byte{wireOK}, result)
	if err != nil {
		return p.callbackRaised(fmt.Errorf("encode Go result: %w", err)), nil
	}
	return enc, nil
}

// callbackRaised encodes the raised response for the Python->Go direction:
// a *PythonError holding a live exception instance of THIS instance is
// re-raised as that instance, anything else as RuntimeError(text).
func (p *Python) callbackRaised(err error) []byte {
	var pe *PythonError
	if errors.As(err, &pe) && pe.Value != nil {
		if b, encErr := p.encodeHandle([]byte{wireRaised, 1}, pe.Value.handle()); encErr == nil {
			return b
		}
	}
	msg := err.Error()
	b := make([]byte, 0, 6+len(msg))
	b = append(b, wireRaised, 0)
	return appendLenBytes(b, []byte(msg))
}

// safeCall contains a panicking GoFunc so a guest-triggered call cannot take
// the whole process down; the panic surfaces as a Python RuntimeError.
func safeCall(fn GoFunc, args []Value, kwargs map[string]Value) (result Value, err error) {
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, fmt.Errorf("Go function panicked: %v", r)
		}
	}()
	return fn(args, kwargs)
}
