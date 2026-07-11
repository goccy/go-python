# go-python

[![CI](https://github.com/goccy/go-python/actions/workflows/ci.yml/badge.svg)](https://github.com/goccy/go-python/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/goccy/go-python.svg)](https://pkg.go.dev/github.com/goccy/go-python)

**CPython in pure Go — embed and run Python anywhere Go runs. No cgo, no native
Python install, one static binary.**

`go-python` runs CPython compiled to WebAssembly and transpiled to Go by
[`goccy/pythonwasm2go`](https://github.com/goccy/pythonwasm2go). It gives Go
programs a real CPython runtime with an embedded standard library, without cgo,
without a native Python installation, and without shipping a wasm runtime at
execution time.

The package is built for embedding Python safely. A host application can create
isolated interpreters, decide exactly what each interpreter can see or touch,
and run agent-generated Python with explicit controls over filesystem,
environment, stdio, networking, subprocesses, memory, and cancellation.

## Why this library

[`gpython`](https://github.com/go-python/gpython) is a pure-Go Python VM, but it
is a partial reimplementation / port of Python 3.4 and does not include many
CPython C-backed modules. That is useful for some embedding use cases, but it
is not the same thing as running CPython.

`go-python` takes a different path: CPython is built to WebAssembly, then the
wasm is transpiled into ordinary Go source. The result keeps the deployment
advantages of a Go dependency while running the CPython runtime and a bundled
CPython standard library.

This matters most when Python code is produced or selected by an AI agent. The
host process can execute familiar Python while still enforcing a Go-side policy
before the guest reaches host resources.

## Features

- **Pure Go, no cgo, no wasm runtime.** CPython is compiled to WebAssembly and
  then transpiled to Go by
  [`goccy/pythonwasm2go`](https://github.com/goccy/pythonwasm2go). Applications
  import a Go module and build with the Go toolchain.
- **Real CPython with an embedded standard library.** `stdlib.zip` contains a
  trimmed CPython `Lib/` tree and is embedded in the module. A zero-value
  `Config` automatically extracts it for local execution, while
  `NewStdlibMemFS` serves it from an in-memory filesystem for fully isolated
  embeddings.
- **Auto-generated end-to-end.** The checked-in bridge (`python.go`) and
  embedded standard library (`stdlib.zip`) are pulled from
  [`goccy/python-wasm`](https://github.com/goccy/python-wasm). Updating the
  upstream release refreshes the generated CPython binding without hand-writing
  the runtime surface.
- **End-to-end provenance.** Upstream-sourced artifacts are protected by
  [GitHub artifact attestations](https://docs.github.com/en/actions/security-for-github-actions/using-artifact-attestations).
  `make verify` checks the signed release provenance for both `python.go` and
  `stdlib.zip`, and CI runs the same verification before tests.
- **Isolated multi-interpreter API.** Each `Interpreter` owns its own wasm
  module, linear memory, WASI host, and CPython runtime. Interpreters can run
  concurrently with independent globals and filesystem state.
- **Agent-oriented sandbox controls.** The host can control:
  - filesystem scope with `PreopenDir`;
  - private filesystem backends with `FS`, including in-memory `MemFS`;
  - per-path read/write decisions with `FSAccess`;
  - guest environment variables with `Env` (host `os.Environ` is not leaked);
  - guest `stdin`, `stdout`, and `stderr` with explicit `io.Reader` /
    `io.Writer` values;
  - hostname resolution with `Resolve`;
  - outbound connect destinations with `Dial`;
  - socket accept/recv/send operations with `NetAccess`;
  - host subprocess execution with `Exec`;
  - wasm linear-memory growth with `MaxMemoryBytes`;
  - long-running Python code with `Interrupt` / `PrepareInterrupt`.
- **Python command front-end.** `cmd/python` and package `cli` provide a
  Python-like command entry point for `python -c <command>` and
  `python <file.py> [args...]`.
- **WASI-friendly build.** Because this is pure Go, embedding applications can
  also cross-compile to `GOOS=wasip1 GOARCH=wasm`. See
  [docs/wasip1.md](docs/wasip1.md).

## Status

The checked-in upstream artifacts currently track
[`goccy/python-wasm`](https://github.com/goccy/python-wasm) `v0.1.5`
(vendored as [`goccy/pythonwasm2go`](https://github.com/goccy/pythonwasm2go)
`v0.2.0`), which builds CPython 3.14.6.

The main API supports expression / statement evaluation, persistent globals per
interpreter, standard-library imports, stdout/stderr capture, isolated
interpreter instances, in-memory filesystems, filesystem/network/subprocess
policy hooks, memory caps, and host-triggered `KeyboardInterrupt`.

The command front-end currently supports `-c` and script-file execution.
Interactive REPL mode and `python -m` are not implemented yet.

## Installation

```sh
go get github.com/goccy/go-python
```

`go-python` requires Go 1.25 or newer.

## Synopsis

### Evaluate Python

```go
package main

import (
	"fmt"

	python "github.com/goccy/go-python"
)

func main() {
	interp, err := python.NewInterpreter(python.Config{})
	if err != nil {
		panic(err)
	}
	defer interp.Close()

	res, err := interp.Eval(`
import json, math
print(json.dumps({"sqrt2": round(math.sqrt(2), 6)}))
`)
	if err != nil {
		panic(err)
	}
	if !res.Ok {
		panic(res.Error)
	}
	fmt.Print(res.Stdout)
}
```

### Run agent-generated code in a tighter sandbox

The zero-value `Config` is convenient for local execution, but it is not a
deny-all sandbox: the host filesystem is visible unless scoped, and networking
or subprocess execution must be denied with hooks when running untrusted code.
For agent-generated code, pass an explicit policy.

```go
fsys, err := python.NewStdlibMemFS()
if err != nil {
	panic(err)
}

interp, err := python.NewInterpreter(python.Config{
	FS:             fsys, // private in-memory filesystem; StdlibDir defaults to "/"
	Env:            []string{"APP_MODE=agent"},
	MaxMemoryBytes: 64 << 20,
	NetAccess:      func(op string) bool { return false },
	Resolve:        func(host string) bool { return false },
	Dial:           func(network, ip string, port int) bool { return false },
	Exec:           func(path string, argv []string) bool { return false },
})
if err != nil {
	panic(err)
}
defer interp.Close()

res, err := interp.Eval("sum(range(101))")
if err != nil {
	panic(err)
}
if !res.Ok {
	panic(res.Error)
}
fmt.Println(res.Repr) // 5050
```

### Interrupt a long-running evaluation

```go
interrupt, err := interp.PrepareInterrupt()
if err != nil {
	panic(err)
}

done := make(chan python.EvalResult, 1)
go func() {
	res, _ := interp.Eval("while True:\n    pass")
	done <- res
}()

time.AfterFunc(time.Second, interrupt.Fire)
res := <-done // res.Ok == false, res.Error contains KeyboardInterrupt
```

### Use the command front-end

```sh
go run ./cmd/python -c 'import sys; print(sys.version)'
go run ./cmd/python ./script.py arg1 arg2
```

## Verifying provenance

The generated bridge and embedded standard library are release artifacts from
`goccy/python-wasm`. You can verify that the local files came from the trusted
upstream release workflow without a GitHub access token:

```sh
make verify
```

The target:

1. computes the SHA-256 digest of `python.go` and `stdlib.zip`;
2. fetches the public attestation bundle for each digest from the GitHub
   attestations API;
3. runs `gh attestation verify --bundle` with
   `--signer-workflow goccy/python-wasm/.github/workflows/release.yml`.

CI runs `make verify` before `go test ./...`, so pull requests must preserve
the signed upstream artifacts.

## Resource footprint

The runtime cost is different from a small hand-written interpreter. This
package embeds a CPython standard library archive and depends on a
wasm2go-transpiled CPython engine, so cold builds and interpreter startup are
heavier than a typical Go dependency. In exchange, applications get a cgo-free,
cross-compilable CPython runtime whose host access is mediated through Go.

For local measurements, run:

```sh
go test ./...
cd bench
go test -bench . -benchmem ./...
go test -run TestMemoryFootprint -v ./...
```

The `bench/` module runs identical Python source on go-python (real CPython
3.14.6) and on [`gpython`](https://github.com/go-python/gpython) (a pure-Go
Python 3.4 VM), so the two are measured on equal footing (`benchstat` `sec/op`,
darwin/amd64, n=10):

| workload | go-python | gpython |
| --- | ---: | ---: |
| `fib(28)` (recursive calls) | 122.8 ms | 217.8 ms |
| `sum(range(200000))` (loop) | 54.1 ms | 17.1 ms |
| interpreter startup | 13.2 ms | 16 µs |

The two make different trade-offs: go-python runs a real CPython engine, so it
is faster on call-heavy code and keeps per-call allocation tiny (≈0.5 KB/op vs
gpython's hundreds of MB for `fib`), while gpython's lightweight VM starts up
and runs tight loops faster.

## License

- **The Go source code of this repository is licensed under [MIT](./LICENSE).**
  That covers everything written or generated here — `interpreter.go`, the
  generated bridge `python.go`, the CLI, the tests and the benchmarks.
- **`stdlib.zip` is not MIT**: it is a repackaged subset of the CPython 3.14.6
  standard library — a derivative work of
  [CPython](https://github.com/python/cpython) — and keeps CPython's own
  license, the Python Software Foundation License Agreement
  ([`LICENSE-PSF`](./LICENSE-PSF)), vendored verbatim. CPython is
  Copyright (c) 2001-present Python Software Foundation; All Rights Reserved.
- The [`pythonwasm2go`](https://github.com/goccy/pythonwasm2go) dependency (the
  transpiled interpreter) is likewise distributed under CPython's license in its
  own repository.

### Using go-python in your own project

- **As a library dependency** (source distribution): your repository contains no
  CPython-derived bytes — only an import path and a go.mod entry. License your
  own code however you like (MIT, proprietary, ...); no CPython license text
  needs to accompany it. Your users receive go-python and pythonwasm2go from
  their own origins, under their own licenses.
- **Shipping a compiled binary**: the binary embeds the transpiled interpreter
  and `stdlib.zip`. The PSF License Agreement is permissive and expressly allows
  this — it grants the right to "reproduce, analyze, test, perform and/or
  display publicly, prepare derivative works, distribute, and otherwise use
  Python ... in any derivative version" (§2), including in commercial and
  closed-source products, with no copyleft. Your own code keeps its own license
  and does not inherit CPython's. The only condition is attribution: retain
  CPython's copyright notice and PSF's license — shipping [`LICENSE-PSF`](./LICENSE-PSF)
  (or a third-party-notices entry pointing at it) alongside your binary
  satisfies it. **So redistribution, including inside a proprietary product, is
  fine.**
- **Python code you run** on the interpreter, and its output, remain yours.

This summary is not legal advice; the license texts govern.
