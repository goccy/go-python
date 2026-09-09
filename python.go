// Package python embeds the CPython interpreter — compiled to wasm and
// transpiled to pure Go — behind a sandboxed, multi-instance API. Each
// Python value is an isolated interpreter: its own memory, filesystem view,
// and environment. No cgo, no system python.
package python

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/goccy/go-python/internal"
)

// Python is one isolated CPython interpreter instance.
type Python struct {
	raw *internal.Python

	// funcs maps the ids baked into materialised Go functions to their Go
	// functions (see bridge.go).
	funcsMu    sync.RWMutex
	funcs      map[int32]GoFunc
	nextFuncID int32
}

// Result is the outcome of one Eval: the expression's value and the
// Python-level error state. An uncaught exception is a fact ABOUT the
// evaluation, so it lives here as Error (*PythonError), not in Eval's own
// error return — that one reports host and transport failures.
type Result struct {
	// Value is the value of the source's last statement when that is an
	// expression statement (the REPL's "value of the last line"), None
	// otherwise. Valid when Error is nil.
	Value Value
	// Error is non-nil when the code raised: an uncaught exception as a
	// *PythonError, or — for an uncaught SystemExit — the error ExitCode
	// recognises.
	Error error
	// Stdout / Stderr capture what the code printed to sys.stdout /
	// sys.stderr (the bridge redirects both onto in-memory buffers for the
	// duration).
	Stdout string
	Stderr string
}

// New builds a fresh Python instance, applies the sandbox config, boots the
// CPython runtime, and returns it ready to Eval.
func New(cfg Config) (*Python, error) {
	// Resolve the filesystem and standard library location: every file the
	// instance opens goes through Config.FS, and StdlibDir is a path inside
	// it. With a supplied backend the stdlib must live in the FS (host
	// backends pair with ExtractStdlib; NewStdlibMemFS pre-loads a MemFS).
	// The nil default is a PRIVATE in-memory filesystem pre-loaded with the
	// stdlib - sandboxed, nothing touches the host disk.
	if cfg.FS == nil {
		fsys, err := NewStdlibMemFS()
		if err != nil {
			return nil, fmt.Errorf("build in-memory stdlib: %w", err)
		}
		cfg.FS = fsys
	}
	if cfg.StdlibDir == "" {
		cfg.StdlibDir = "/"
	}

	p := &Python{}
	raw, err := internal.New(internal.InstanceOptions{
		StdlibDir:          cfg.StdlibDir,
		Env:                cfg.Env,
		FS:                 cfg.FS,
		Dial:               cfg.Dial,
		Resolve:            cfg.Resolve,
		Exec:               cfg.Exec,
		Stdin:              cfg.Stdin,
		Stdout:             cfg.Stdout,
		Stderr:             cfg.Stderr,
		MaxMemoryBytes:     cfg.MaxMemoryBytes,
		MemoryReserveBytes: cfg.MemoryReserveBytes,
	})
	if err != nil {
		return nil, err
	}
	raw.UserHandler = p.handleUserCallback
	p.raw = raw
	return p, nil
}

// Eval compiles and runs src in this instance's persistent __main__
// namespace and returns the structured result. An uncaught exception is
// reported via Result.Error (*PythonError), not as Eval's own error; a Go
// error indicates a host/transport failure (a wasm trap, encoding problem,
// ...) or a context cancellation.
//
// Cancelling ctx while the code runs raises KeyboardInterrupt at the next
// bytecode back-edge (CPython's own eval-breaker mechanism, tripped by a
// host-side memory write) and Eval returns ctx.Err(). A single long-running
// C-level operation is not preempted until it returns to the interpreter
// loop.
func (p *Python) Eval(ctx context.Context, src string) (Result, error) {
	resp, interrupted, err := p.raw.EvalOp(ctx, src)
	if err != nil {
		return Result{}, err
	}
	return p.decodeEvalResult(ctx, resp, interrupted)
}

// RunFile runs the script at path (a path inside Config.FS) as __main__,
// the way `python path args...` does: sys.argv is [path, args...], the
// script's directory heads sys.path, and __file__ / __name__ are set. Output
// is not captured — it streams to Config.Stdout / Config.Stderr. An
// uncaught exception is returned as a *PythonError, a SystemExit as the
// error ExitCode recognises.
func (p *Python) RunFile(ctx context.Context, path string, args []string) error {
	if err := p.AddPath(ctx, filepath.Dir(path)); err != nil {
		return err
	}
	sys, err := p.Import(ctx, "sys")
	if err != nil {
		return err
	}
	argv := make([]Value, 0, len(args)+1)
	argv = append(argv, ValueOf(path))
	for _, a := range args {
		argv = append(argv, ValueOf(a))
	}
	argvList, err := p.NewList(ctx, argv...)
	if err != nil {
		return err
	}
	if err := sys.SetAttr(ctx, "argv", argvList); err != nil {
		return err
	}
	runpy, err := p.Import(ctx, "runpy")
	if err != nil {
		return err
	}
	_, runErr := runpy.CallMethod(ctx, "run_path", ValueOf(path), Kwarg("run_name", ValueOf("__main__")))
	// sys.stdout / sys.stderr are block-buffered when they are not a tty;
	// a script's output would otherwise reach Config.Stdout only at Close,
	// the way a python process flushes at exit. Flush now so the caller
	// sees everything the script printed when RunFile returns, before any
	// traceback it goes on to report.
	if err := p.FlushStdio(ctx); err != nil && runErr == nil {
		return err
	}
	return runErr
}

// FlushStdio flushes sys.stdout and sys.stderr, delivering buffered output
// to Config.Stdout / Config.Stderr. Eval captures print() output itself;
// code run through Call or RunFile writes to the (block-buffered) real
// streams, which a python process only flushes at exit — call this when
// the output should be visible before Close.
func (p *Python) FlushStdio(ctx context.Context) error {
	sys, err := p.Import(ctx, "sys")
	if err != nil {
		return err
	}
	for _, name := range []string{"stdout", "stderr"} {
		stream, err := sys.Attr(ctx, name)
		if err != nil {
			return err
		}
		obj, ok := stream.(ObjectValue)
		if !ok {
			continue // sys.stdout = None
		}
		if _, err := obj.CallMethod(ctx, "flush"); err != nil {
			return err
		}
	}
	return nil
}

// AddPath prepends dirs (paths inside Config.FS) to sys.path, so modules
// there become importable.
func (p *Python) AddPath(ctx context.Context, dirs ...string) error {
	if len(dirs) == 0 {
		return nil
	}
	sys, err := p.Import(ctx, "sys")
	if err != nil {
		return err
	}
	pathVal, err := sys.Attr(ctx, "path")
	if err != nil {
		return err
	}
	path, err := As[ListValue](pathVal)
	if err != nil {
		return fmt.Errorf("python: sys.path: %w", err)
	}
	// Insert in reverse so dirs end up in the given order at the front.
	for i := len(dirs) - 1; i >= 0; i-- {
		if _, err := path.CallMethod(ctx, "insert", ValueOf(0), ValueOf(dirs[i])); err != nil {
			return fmt.Errorf("python: extend sys.path: %w", err)
		}
	}
	return nil
}

// Import is `import name` (a dotted name imports the leaf module) and
// returns the module. Import(ctx, "__main__") is the namespace Eval runs in.
func (p *Python) Import(ctx context.Context, name string) (ModuleValue, error) {
	resp, interrupted, err := p.raw.ImportOp(ctx, name)
	if err != nil {
		return ModuleValue{}, err
	}
	v, err := p.decodeNodeResult(ctx, resp, interrupted)
	if err != nil {
		return ModuleValue{}, err
	}
	m, err := As[ModuleValue](v)
	if err != nil {
		return ModuleValue{}, fmt.Errorf("python: import %s: %w", name, err)
	}
	return m, nil
}

// NewList materialises elems as a guest list in one crossing.
func (p *Python) NewList(ctx context.Context, elems ...Value) (ListValue, error) {
	return newAggregate[ListValue](ctx, p, p.raw.NewListOp, elems)
}

// NewTuple materialises elems as a guest tuple in one crossing.
func (p *Python) NewTuple(ctx context.Context, elems ...Value) (TupleValue, error) {
	return newAggregate[TupleValue](ctx, p, p.raw.NewTupleOp, elems)
}

// NewSet materialises elems as a guest set in one crossing.
func (p *Python) NewSet(ctx context.Context, elems ...Value) (SetValue, error) {
	return newAggregate[SetValue](ctx, p, p.raw.NewSetOp, elems)
}

// NewFrozenSet materialises elems as a guest frozenset in one crossing.
func (p *Python) NewFrozenSet(ctx context.Context, elems ...Value) (FrozenSetValue, error) {
	return newAggregate[FrozenSetValue](ctx, p, p.raw.NewFrozenSetOp, elems)
}

// NewDict materialises items as a guest dict in one crossing.
func (p *Python) NewDict(ctx context.Context, items ...DictItem) (DictValue, error) {
	enc, err := p.encodeDictItems(items)
	if err != nil {
		return DictValue{}, err
	}
	resp, interrupted, err := p.raw.NewDictOp(ctx, enc)
	if err != nil {
		return DictValue{}, err
	}
	v, err := p.decodeNodeResult(ctx, resp, interrupted)
	if err != nil {
		return DictValue{}, err
	}
	return As[DictValue](v)
}

// newAggregate is the shared shape of NewList / NewTuple / NewSet /
// NewFrozenSet: encode the elements, run the constructor op, expect T back.
func newAggregate[T Value](ctx context.Context, p *Python,
	op func(context.Context, []byte) ([]byte, bool, error), elems []Value) (T, error) {
	var zero T
	enc, err := p.encodeList(elems)
	if err != nil {
		return zero, err
	}
	resp, interrupted, err := op(ctx, enc)
	if err != nil {
		return zero, err
	}
	v, err := p.decodeNodeResult(ctx, resp, interrupted)
	if err != nil {
		return zero, err
	}
	return As[T](v)
}

// Close finalizes the instance. The Python must not be used afterward: every
// later Eval/Call errors, and outstanding handles become inert (their
// finalizers only touch host-side state). Idempotent.
func (p *Python) Close() error {
	return p.raw.Close()
}
