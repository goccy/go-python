// Package cli implements a python-compatible command front-end on top of the
// go-python interpreter.
//
// It is intentionally a library, not just a main package: a single-binary
// sandbox can embed the python command by importing this package and calling
// Run with its own argv and io streams, while cmd/python is only a thin
// wrapper (os.Exit(cli.Run(os.Args, os.Stdin, os.Stdout, os.Stderr))).
//
// Supported today: `python -c <command>` and `python <file.py> [args...]`,
// both with sys.argv populated and code run in a real __main__ module. Module
// (-m) and interactive (REPL) modes return a usage error for now.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	python "github.com/goccy/go-python"
)

// Run executes a python-style invocation and returns a process exit code,
// following CPython's conventions: 0 on success, 1 on an uncaught exception,
// 2 on a command-line / file-open error.
//
// args is the full argv including args[0] (the program name). stdin backs the
// guest's sys.stdin; stdout/stderr receive program output. Run never writes to
// the host process streams directly — everything goes through the passed
// writers — so it is safe to call in-process from an embedding sandbox.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	inv, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "python: %v\n", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}

	// File mode: read the script from the host filesystem up front so a
	// missing/unreadable file is reported the way CPython does (exit 2),
	// before an interpreter is even started.
	source := inv.command
	filename := "<string>"
	scriptDir := ""
	if inv.mode == modeFile {
		data, rerr := os.ReadFile(inv.file)
		if rerr != nil {
			fmt.Fprintf(stderr, "python: can't open file %q: %v\n", inv.file, rerr)
			return 2
		}
		source = string(data)
		filename = inv.file
		if abs, aerr := filepath.Abs(inv.file); aerr == nil {
			filename = abs
			scriptDir = filepath.Dir(abs)
		}
	}

	interp, err := python.NewInterpreter(python.Config{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
	})
	if err != nil {
		fmt.Fprintf(stderr, "python: failed to start interpreter: %v\n", err)
		return 1
	}
	defer interp.Close()

	res, err := interp.Eval(driver(source, filename, inv.argv, scriptDir, inv.mode == modeFile))
	if err != nil {
		// Host/transport failure (wasm trap, encoding problem) — not a
		// Python-level exception.
		fmt.Fprintf(stderr, "python: %v\n", err)
		return 1
	}

	// Surface program output. Python-level print() is captured by the bridge
	// into res.Stdout/res.Stderr; raw fd 1/2 writes already streamed straight
	// to stdout/stderr during Eval.
	if res.Stdout != "" {
		io.WriteString(stdout, res.Stdout)
	}
	if res.Stderr != "" {
		io.WriteString(stderr, res.Stderr)
	}
	if !res.Ok {
		io.WriteString(stderr, res.Error)
		if !strings.HasSuffix(res.Error, "\n") {
			io.WriteString(stderr, "\n")
		}
		return 1
	}
	return 0
}

const usage = "usage: python [-c cmd | file] [arg] ..."

const (
	modeCommand = "c"
	modeFile    = "file"
)

// invocation is the parsed command line.
type invocation struct {
	mode    string   // modeCommand or modeFile
	command string   // -c command (modeCommand)
	file    string   // script path (modeFile)
	argv    []string // sys.argv (argv[0] = "-c" or the script name)
}

// parseArgs implements the subset of CPython's argument grammar for
// `python -c <command> [args...]` and `python <file> [args...]`.
func parseArgs(args []string) (invocation, error) {
	rest := args
	if len(rest) > 0 {
		rest = rest[1:] // drop argv[0] (program name)
	}
	if len(rest) == 0 {
		return invocation{}, fmt.Errorf("no command or file given (interactive mode not supported yet)")
	}
	switch {
	case rest[0] == "-c":
		if len(rest) < 2 {
			return invocation{}, fmt.Errorf("argument expected for the -c option")
		}
		// sys.argv[0] is "-c", followed by everything after the command.
		return invocation{mode: modeCommand, command: rest[1], argv: append([]string{"-c"}, rest[2:]...)}, nil
	case strings.HasPrefix(rest[0], "-c"):
		// `-c<code>` (no space) form.
		return invocation{mode: modeCommand, command: strings.TrimPrefix(rest[0], "-c"), argv: append([]string{"-c"}, rest[1:]...)}, nil
	case strings.HasPrefix(rest[0], "-"):
		return invocation{}, fmt.Errorf("unsupported option %q", rest[0])
	default:
		// File mode: sys.argv[0] is the script name as given.
		return invocation{mode: modeFile, file: rest[0], argv: append([]string{rest[0]}, rest[1:]...)}, nil
	}
}

// driver builds a Python snippet that sets sys.argv, optionally puts the
// script directory on sys.path, and runs source as the __main__ module with
// the right filename (so tracebacks and __file__ are correct).
//
// All interpolated values are encoded with json.Marshal: a Go JSON string /
// array literal is also a valid Python str / list-of-str literal, so this is
// injection-safe regardless of what the source, filename, or args contain.
func driver(source, filename string, argv []string, scriptDir string, isFile bool) string {
	srcLit := jsonLit(source)
	fnLit := jsonLit(filename)
	var b strings.Builder
	b.WriteString("import sys\n")
	b.WriteString("sys.argv = " + jsonLit(argv) + "\n")
	if scriptDir != "" {
		b.WriteString("sys.path.insert(0, " + jsonLit(scriptDir) + ")\n")
	}
	if isFile {
		b.WriteString("_ns = {'__name__': '__main__', '__file__': " + fnLit + "}\n")
	} else {
		b.WriteString("_ns = {'__name__': '__main__'}\n")
	}
	b.WriteString("exec(compile(" + srcLit + ", " + fnLit + ", 'exec'), _ns)\n")
	return b.String()
}

// jsonLit returns v as a JSON literal, which doubles as a Python literal for
// strings and []string. Marshalling a plain string/[]string never errors.
func jsonLit(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
