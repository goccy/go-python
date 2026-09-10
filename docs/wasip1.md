# Running go-python as a `wasip1/wasm` binary

go-python is pure Go, so it cross-compiles to WebAssembly (`GOOS=wasip1
GOARCH=wasm`) and runs under any WASI runtime — i.e. the embedded CPython
interpreter ends up running *wasm-inside-wasm*. This was verified end to end
with CPython 3.14.6 executing `math`, `json`, `sys` and string operations.

## Building

```sh
GOOS=wasip1 GOARCH=wasm go build -o python.wasm ./cmd/python
```

For `GOARCH=wasm` the build automatically selects the wasm2go **pure** code
path (build constraint `(!amd64 && !arm64) || purego`, bounds-checked slice
access) rather than the `amd64`/`arm64` assembly path. No extra flags are
needed.

## Use the in-memory stdlib FS (the library default)

There is one catch for the wasm target: how the interpreter gets at the
standard library.

The `python` command (`cmd/python`) runs on the host filesystem and extracts
the embedded stdlib to a host temporary directory (`ExtractStdlib`). Under a
**nested** WASI setup — the inner CPython's file ops go through `WasiStubs` →
Go `os.*` → Go's `wasip1` runtime → the outer WASI runtime → the host
filesystem — that extraction *writes* fine but CPython's `getpath` fails when
it tries to *read* the tree back:

```
OSError: [Errno 8] Bad file descriptor
[pyembed] Py_InitializeFromConfig failed: ... error evaluating path
```

(The exact cause is a `fd_readdir`/`filestat` semantics gap between Go's
`wasip1` runtime and the outer WASI host; it is not specific to any one
runtime.)

The robust approach for the wasm target is the **in-memory stdlib FS**, which
is what the library's zero `Config` uses: the standard library then lives
entirely in Go memory and no real WASI filesystem is touched for stdlib
access, so the nested-FS problem disappears. This is also the right pattern
for sandboxed embedding in general.

```go
package main

import (
	"context"
	"fmt"
	"os"

	python "github.com/goccy/go-python"
)

func main() {
	p, err := python.New(python.Config{
		// FS is nil: a private in-memory filesystem pre-loaded with the
		// stdlib, StdlibDir "/".
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "new:", err)
		os.Exit(1)
	}
	defer p.Close()

	res, err := p.Eval(context.Background(), `
import sys, math, json
print("1+1 =", 1 + 1)
print("sqrt(2) =", round(math.sqrt(2), 6))
print("json:", json.dumps({"a": [1, 2, 3]}))
print("sys.version:", sys.version.split()[0])
`)
	if err != nil {
		fmt.Fprintln(os.Stderr, "eval:", err)
		os.Exit(1)
	}
	fmt.Print(res.Stdout)
	if res.Error != nil {
		fmt.Fprintln(os.Stderr, res.Error)
		os.Exit(1)
	}
}
```

Build it the same way:

```sh
GOOS=wasip1 GOARCH=wasm go build -o app.wasm ./path/to/main
```

## Running

Any WASI (`wasi_snapshot_preview1`) runtime works — `wasmtime`, `wasmer`, or
the pure-Go [wazero](https://github.com/tetratelabs/wazero) used as a library.
With `wasmtime`:

```sh
wasmtime run app.wasm
```

When using the in-memory stdlib FS, no `--dir` preopen is required. If you do
fall back to the host-fs `cmd/python`, you would need a writable preopen for
the temp directory (e.g. `wasmtime run --dir=/tmp --env TMPDIR=/tmp ...`) —
but see the note above about the `getpath` failure under nested WASI.

### Verified output

```
1+1 = 2
sqrt(2) = 1.414214
json: {"a": [1, 2, 3]}
sys.version: 3.14.6
```
