// Package pybench compares two ways to run Python from Go:
//
//   - go-python: real CPython 3.14 compiled to wasm and transpiled to pure Go
//     by wasm2go (this project).
//   - gpython:   github.com/go-python/gpython, a pure-Go reimplementation of a
//     Python VM (~3.4 semantics).
//
// Both run the SAME Python source. Each iteration compiles+runs the snippet in
// a persistent module/globals, so the two are measured on equal footing.
//
//	go test -bench . -benchmem ./...
//	go test -run TestMemoryFootprint -v ./...
package pybench

import (
	"context"
	"runtime"
	"testing"

	python "github.com/goccy/go-python"

	"github.com/go-python/gpython/compile"
	"github.com/go-python/gpython/py"
	_ "github.com/go-python/gpython/stdlib"
)

// Workloads (identical source for both engines).
const (
	fibDef = `
def fib(n):
    return n if n < 2 else fib(n-1) + fib(n-2)
`
	fibCall = "r = fib(28)" // ~832040 calls of recursive work
	loopSum = `
s = 0
for i in range(200000):
    s += i
`
)

// ---- go-python (wasm2go CPython) -----------------------------------------

func goPythonInterp(tb testing.TB) *python.Python {
	tb.Helper()
	interp, err := python.New(python.Config{})
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	return interp
}

func goPythonEval(tb testing.TB, interp *python.Python, src string) {
	r, err := interp.Eval(context.Background(), src)
	if err != nil {
		tb.Fatalf("eval host error: %v", err)
	}
	if r.Error != nil {
		tb.Fatalf("eval python error: %s", r.Error)
	}
}

func BenchmarkFibRecursive_GoPython(b *testing.B) {
	interp := goPythonInterp(b)
	defer interp.Close()
	goPythonEval(b, interp, fibDef)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		goPythonEval(b, interp, fibCall)
	}
}

func BenchmarkLoopSum_GoPython(b *testing.B) {
	interp := goPythonInterp(b)
	defer interp.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		goPythonEval(b, interp, loopSum)
	}
}

func BenchmarkStartup_GoPython(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		interp := goPythonInterp(b)
		interp.Close()
	}
}

// ---- gpython (pure-Go Python VM) -----------------------------------------

// gpythonExec compiles src in exec mode (so multi-statement snippets run in
// full, unlike RunSrc's interactive SingleMode) and runs it in mod's globals.
func gpythonExec(tb testing.TB, ctx py.Context, mod *py.Module, src string) {
	code, err := compile.Compile(src+"\n", "bench", py.ExecMode, 0, true)
	if err != nil {
		tb.Fatalf("gpython compile: %v", err)
	}
	if _, err := py.RunCode(ctx, code, "bench", mod); err != nil {
		tb.Fatalf("gpython run: %v", err)
	}
}

func gpythonModule(tb testing.TB, setup string) (py.Context, *py.Module) {
	tb.Helper()
	ctx := py.NewContext(py.DefaultContextOpts())
	code, err := compile.Compile(setup+"\n", "setup", py.ExecMode, 0, true)
	if err != nil {
		tb.Fatalf("gpython setup compile: %v", err)
	}
	mod, err := py.RunCode(ctx, code, "setup", nil)
	if err != nil {
		tb.Fatalf("gpython setup: %v", err)
	}
	return ctx, mod
}

func BenchmarkFibRecursive_Gpython(b *testing.B) {
	ctx, mod := gpythonModule(b, fibDef)
	defer ctx.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gpythonExec(b, ctx, mod, fibCall)
	}
}

func BenchmarkLoopSum_Gpython(b *testing.B) {
	ctx, mod := gpythonModule(b, "pass")
	defer ctx.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		gpythonExec(b, ctx, mod, loopSum)
	}
}

func BenchmarkStartup_Gpython(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		ctx := py.NewContext(py.DefaultContextOpts())
		if _, err := py.RunSrc(ctx, "pass", "startup", nil); err != nil {
			b.Fatalf("gpython startup: %v", err)
		}
		ctx.Close()
	}
}

// ---- memory footprint (resident-ish heap of one live interpreter) ---------

// TestMemoryFootprint reports the Go heap a single live interpreter occupies
// after booting and running fib(30) once. Run with -v.
func TestMemoryFootprint(t *testing.T) {
	measure := func(name string, build func() func()) {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		cleanup := build()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		t.Logf("%-10s heapAlloc=%6.1f MiB  sys=%6.1f MiB",
			name,
			float64(after.HeapAlloc-before.HeapAlloc)/(1024*1024),
			float64(after.Sys-before.Sys)/(1024*1024))
		cleanup()
	}

	measure("go-python", func() func() {
		interp := goPythonInterp(t)
		goPythonEval(t, interp, fibDef)
		goPythonEval(t, interp, "r = fib(30)")
		return func() { interp.Close() }
	})
	measure("gpython", func() func() {
		ctx, mod := gpythonModule(t, fibDef)
		gpythonExec(t, ctx, mod, "r = fib(30)")
		return func() { ctx.Close() }
	})
}
