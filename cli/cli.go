// Package cli implements a python-compatible command front-end on top of the
// go-python interpreter.
//
// It is intentionally a library, not just a main package: a single-binary
// sandbox can embed the python command by importing this package and calling
// Run with its own argv and io streams, while cmd/python is only a thin
// wrapper (os.Exit(cli.Run(os.Args, os.Stdin, os.Stdout, os.Stderr))).
//
// Supported today: `python -c <command>` and `python <file.py> [args...]`,
// both with sys.argv populated and code run in the real __main__ module.
// Module (-m) and interactive (REPL) modes return a usage error for now.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	python "github.com/goccy/go-python"
	"github.com/goccy/go-python/fs"
)

// HostConfig is the configuration the python command starts from: the
// operating system's filesystem with the embedded stdlib extracted onto it,
// and every capability hook allowing — python(1) does not sandbox, so neither
// does this command (the library's zero Config denies these instead).
func HostConfig() (python.Config, error) {
	stdlib, err := python.ExtractStdlib()
	if err != nil {
		return python.Config{}, fmt.Errorf("extract embedded stdlib: %w", err)
	}
	return python.Config{
		FS:        fs.NewHostFS(),
		StdlibDir: stdlib,
		Dial:      func(string, string, string, int) bool { return true },
		Resolve:   func(string) bool { return true },
		Exec:      func(string, []string) bool { return true },
	}, nil
}

// Run executes a python-style invocation and returns a process exit code,
// following CPython's conventions: 0 on success, 1 on an uncaught exception,
// 2 on a command-line / file-open error, and the requested status on
// sys.exit().
//
// args is the full argv including args[0] (the program name). stdin backs the
// guest's sys.stdin; stdout/stderr receive program output as it is produced.
// Run never writes to the host process streams directly — everything goes
// through the passed writers — so it is safe to call in-process from an
// embedding sandbox.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	inv, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "python: %v\n", err)
		fmt.Fprintln(stderr, usage)
		return 2
	}

	// File mode: resolve and check the script up front so a missing file is
	// reported the way CPython does (exit 2), before an interpreter is even
	// started.
	if inv.mode == modeFile {
		abs, aerr := filepath.Abs(inv.file)
		if aerr == nil {
			inv.file = abs
		}
		if _, serr := os.Stat(inv.file); serr != nil {
			fmt.Fprintf(stderr, "python: can't open file %q: %v\n", inv.file, serr)
			return 2
		}
	}

	cfg, err := HostConfig()
	if err != nil {
		fmt.Fprintf(stderr, "python: %v\n", err)
		return 1
	}
	// The python command behaves like python(1): the program sees the host
	// filesystem, stdio, and environment.
	cfg.Stdin = stdin
	cfg.Stdout = stdout
	cfg.Stderr = stderr
	cfg.Env = os.Environ()
	p, err := python.New(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "python: failed to start interpreter: %v\n", err)
		return 1
	}
	defer p.Close()

	ctx := context.Background()
	var runErr error
	switch inv.mode {
	case modeFile:
		runErr = p.RunFile(ctx, inv.file, inv.args)
	default:
		runErr = runCommand(ctx, p, inv.command, inv.args)
	}
	return reportRunError(stderr, runErr)
}

// runCommand is `python -c command args...`: sys.argv is ["-c", args...] and
// the command runs in __main__ through builtins.exec, so its output streams
// to the configured stdout rather than being captured.
func runCommand(ctx context.Context, p *python.Python, command string, args []string) error {
	sys, err := p.Import(ctx, "sys")
	if err != nil {
		return err
	}
	argv := make([]python.Value, 0, len(args)+1)
	argv = append(argv, python.ValueOf("-c"))
	for _, a := range args {
		argv = append(argv, python.ValueOf(a))
	}
	argvList, err := p.NewList(ctx, argv...)
	if err != nil {
		return err
	}
	if err := sys.SetAttr(ctx, "argv", argvList); err != nil {
		return err
	}
	main, err := p.Import(ctx, "__main__")
	if err != nil {
		return err
	}
	globals, err := main.Attr(ctx, "__dict__")
	if err != nil {
		return err
	}
	builtins, err := p.Import(ctx, "builtins")
	if err != nil {
		return err
	}
	code, err := builtins.CallMethod(ctx, "compile", python.ValueOf(command), python.ValueOf("<string>"), python.ValueOf("exec"))
	if err != nil {
		return err
	}
	_, runErr := builtins.CallMethod(ctx, "exec", code, globals)
	// Deliver the program's buffered output before any traceback is
	// reported (a python process flushes its streams at exit).
	if err := p.FlushStdio(ctx); err != nil && runErr == nil {
		return err
	}
	return runErr
}

// reportRunError maps a run outcome onto the python process conventions and
// prints what the interpreter would have: an uncaught exception's traceback
// (exit 1), or nothing for a clean sys.exit() (its status).
func reportRunError(stderr io.Writer, err error) int {
	if err == nil {
		return 0
	}
	if code, ok := python.ExitCode(err); ok {
		return code
	}
	var pe *python.PythonError
	if errors.As(err, &pe) {
		tb := pe.Traceback
		if tb == "" {
			tb = pe.Error()
		}
		io.WriteString(stderr, tb)
		if !strings.HasSuffix(tb, "\n") {
			io.WriteString(stderr, "\n")
		}
		return 1
	}
	// Host/transport failure (wasm trap, encoding problem) — not a
	// Python-level exception.
	fmt.Fprintf(stderr, "python: %v\n", err)
	return 1
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
	args    []string // the arguments after the command / script (sys.argv[1:])
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
		return invocation{mode: modeCommand, command: rest[1], args: rest[2:]}, nil
	case strings.HasPrefix(rest[0], "-c"):
		// `-c<code>` (no space) form.
		return invocation{mode: modeCommand, command: strings.TrimPrefix(rest[0], "-c"), args: rest[1:]}, nil
	case strings.HasPrefix(rest[0], "-"):
		return invocation{}, fmt.Errorf("unsupported option %q", rest[0])
	default:
		return invocation{mode: modeFile, file: rest[0], args: rest[1:]}, nil
	}
}
