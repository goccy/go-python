// Command python is a thin wrapper around the cli package: a standalone
// `python` executable backed by the go-python (CPython-via-wasm2go)
// interpreter. All behaviour lives in github.com/goccy/go-python/cli so the
// same command can be embedded in another binary (e.g. an agent sandbox).
package main

import (
	"os"

	"github.com/goccy/go-python/cli"
)

func main() {
	os.Exit(cli.Run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}
