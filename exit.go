package python

// Guest exit handling.

import "github.com/goccy/go-python/internal"

// ExitCode reports whether err (from Eval, RunFile, or a value operation)
// is the guest terminating itself — an uncaught SystemExit (sys.exit(),
// raise SystemExit) — and returns the exit status as CPython would report
// it to the shell: None -> 0, an int -> that int, anything else -> 1 (after
// printing it to stderr). An embedder running a whole script typically
// forwards the status to os.Exit after Closing the instance. The
// interpreter is cleanly unwound and stays usable.
func ExitCode(err error) (int, bool) {
	return internal.WasiExitCode(err)
}
