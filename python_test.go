package python

// End-to-end behavioural suite for the go-python wasm2go backend, driven
// through the public Python API. Covers: evaluation and the standard library,
// the eval result contract (last-expression value, exceptions, SystemExit,
// captured stdio), per-instance stdio and memory-cap config, host-provided
// subprocess, concurrent isolated instances, context cancellation of a
// runaway loop, the fail-closed zero Config, filesystem scoping and
// per-instance filesystem isolation, environment isolation, and the outbound
// network policy hooks. The Go <-> Python value bridge has its own suite in
// bridge_test.go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gopythonfs "github.com/goccy/go-python/fs"
)

var ctx = context.Background()

// newPy builds an instance from cfg and closes it with the test.
func newPy(t *testing.T, cfg Config) *Python {
	t.Helper()
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// mustEval evaluates and fails on a host error or a Python exception.
func mustEval(t *testing.T, p *Python, src string) Result {
	t.Helper()
	r, err := p.Eval(ctx, src)
	if err != nil {
		t.Fatalf("Eval(%q) host error: %v", src, err)
	}
	if r.Error != nil {
		t.Fatalf("Eval(%q) python error: %v", src, r.Error)
	}
	return r
}

// evalRepr evaluates the expression src and returns repr() of its value.
func evalRepr(t *testing.T, p *Python, src string) string {
	t.Helper()
	r := mustEval(t, p, "repr("+src+")")
	s, err := As[StrValue](r.Value)
	if err != nil {
		t.Fatalf("repr(%s): %v", src, err)
	}
	return s.String()
}

// TestEvalBasics is a smoke test of the CPython backend through the public
// API: arithmetic, a stdlib import, print() capture, and json against the
// embedded standard library — so it needs no on-disk stdlib path.
func TestEvalBasics(t *testing.T) {
	p := newPy(t, Config{})

	r := mustEval(t, p, "1 + 1")
	if n, err := As[IntValue](r.Value); err != nil {
		t.Fatalf("1+1: %v", err)
	} else if v, _ := n.Int64(); v != 2 {
		t.Errorf("1+1 = %d, want 2", v)
	}

	mustEval(t, p, "import math, json")
	if got := evalRepr(t, p, "math.sqrt(2)"); got != "1.4142135623730951" {
		t.Errorf("math.sqrt(2) = %q", got)
	}
	if r := mustEval(t, p, "print('hi wasm2go')"); r.Stdout != "hi wasm2go\n" {
		t.Errorf("print stdout = %q, want %q", r.Stdout, "hi wasm2go\n")
	}
	if got := evalRepr(t, p, "json.dumps({'a': 1})"); got != `'{"a": 1}'` {
		t.Errorf("json.dumps = %q", got)
	}
}

// TestEvalLastExpressionValue pins Result.Value's contract: the value of the
// last statement when it is an expression statement, None otherwise, with
// the preceding statements executed in the persistent namespace.
func TestEvalLastExpressionValue(t *testing.T) {
	p := newPy(t, Config{})

	r := mustEval(t, p, "import json\nx = {'k': [1, 2]}\njson.dumps(x)")
	if s, err := As[StrValue](r.Value); err != nil || s.String() != `{"k": [1, 2]}` {
		t.Errorf("last-expression value = %v (%v)", r.Value, err)
	}
	if r := mustEval(t, p, "y = 5"); r.Value.Kind() != KindNone {
		t.Errorf("statement-only source: value kind %s, want none", r.Value.Kind())
	}
	if r := mustEval(t, p, "x['k']"); r.Value.Kind() != KindList {
		t.Errorf("persisted binding: kind %s, want list", r.Value.Kind())
	}
	if r := mustEval(t, p, ""); r.Value.Kind() != KindNone {
		t.Errorf("empty source: kind %s, want none", r.Value.Kind())
	}
	// A syntax error in a multi-statement source is a real SyntaxError.
	r, err := p.Eval(ctx, "x = 1\ndef (:\n")
	if err != nil {
		t.Fatal(err)
	}
	var pe *PythonError
	if !errors.As(r.Error, &pe) || pe.Type != "SyntaxError" {
		t.Errorf("syntax error: got %v", r.Error)
	}
}

// TestNormalAndStdlib exercises ordinary evaluation plus a spread of stdlib
// modules to confirm the bundled standard library is importable and functional.
func TestNormalAndStdlib(t *testing.T) {
	p := newPy(t, Config{})

	if got := evalRepr(t, p, "sum(range(101))"); got != "5050" {
		t.Errorf("sum(range(101)) = %q, want 5050", got)
	}
	// Persistent globals across calls (REPL semantics).
	mustEval(t, p, "acc = 0")
	for n := 1; n <= 5; n++ {
		mustEval(t, p, "acc += 1")
	}
	if got := evalRepr(t, p, "acc"); got != "5" {
		t.Errorf("persistent acc = %q, want 5", got)
	}

	mustEval(t, p, "import math, base64, hashlib, json, re, collections, textwrap, functools, datetime, decimal")
	cases := []struct{ src, want string }{
		{"round(math.factorial(10))", "3628800"},
		// decimal and hashlib are backed by C code that lives in objects
		// with colliding basenames across CPython's source tree
		// (libmpdec/context.o vs Python/context.o, _hacl/*). A build that
		// links the wrong member turns their functions into host stubs, and
		// the modules degrade silently (prec 0, InvalidOperation) rather than
		// failing to import — so pin the arithmetic itself.
		{"str(decimal.Decimal('1.1') + decimal.Decimal('2.2'))", "'3.3'"},
		{"decimal.getcontext().prec", "28"},
		{"hashlib.md5(b'x').hexdigest()[:6]", "'9dd4e4'"},
		{"base64.b64encode(b'hi').decode()", "'aGk='"},
		{"hashlib.sha256(b'abc').hexdigest()[:8]", "'ba7816bf'"},
		{"json.dumps({'b':2,'a':1}, sort_keys=True)", `'{"a": 1, "b": 2}'`},
		{"re.findall(r'\\d+', 'a1b22c333')", "['1', '22', '333']"},
		{"collections.Counter('aabbbc')['b']", "3"},
		{"textwrap.shorten('a b c d e', width=7)", "'a [...]'"},
		{"functools.reduce(lambda a,b:a*b, range(1,6))", "120"},
		{"datetime.date(2020,2,29).isoformat()", "'2020-02-29'"},
	}
	for _, c := range cases {
		if got := evalRepr(t, p, c.src); got != c.want {
			t.Errorf("%s = %q, want %q", c.src, got, c.want)
		}
	}
}

// TestEvalException pins the exception contract: an uncaught exception is
// Result.Error as a *PythonError carrying the type name, str(exc), the
// formatted traceback, and the live exception instance.
func TestEvalException(t *testing.T) {
	p := newPy(t, Config{})

	r, err := p.Eval(ctx, "print('before')\nraise ValueError('boom')")
	if err != nil {
		t.Fatal(err)
	}
	var pe *PythonError
	if !errors.As(r.Error, &pe) {
		t.Fatalf("Result.Error = %T %v, want *PythonError", r.Error, r.Error)
	}
	if pe.Type != "ValueError" || pe.Message != "boom" {
		t.Errorf("PythonError = %q / %q", pe.Type, pe.Message)
	}
	if pe.Error() != "ValueError: boom" {
		t.Errorf("Error() = %q", pe.Error())
	}
	if !strings.Contains(pe.Traceback, "Traceback (most recent call last)") ||
		!strings.HasSuffix(strings.TrimSpace(pe.Traceback), "ValueError: boom") {
		t.Errorf("Traceback = %q", pe.Traceback)
	}
	if r.Stdout != "before\n" {
		t.Errorf("stdout before the raise not delivered: %q", r.Stdout)
	}
	// The instance is live: its args are reachable through the protocol.
	if pe.Value == nil {
		t.Fatal("PythonError.Value is nil")
	}
	args, err := pe.Value.Attr(ctx, "args")
	if err != nil {
		t.Fatal(err)
	}
	tup, err := As[TupleValue](args)
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	first, err := tup.Index(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := As[StrValue](first); err != nil || s.String() != "boom" {
		t.Errorf("args[0] = %v (%v)", first, err)
	}

	// A user-defined exception reports its qualified name.
	r, err = p.Eval(ctx, "class MyError(Exception):\n    pass\nraise MyError('x')")
	if err != nil {
		t.Fatal(err)
	}
	if !errors.As(r.Error, &pe) || pe.Type != "__main__.MyError" {
		t.Errorf("custom exception type = %v", r.Error)
	}
}

// TestEvalSystemExit pins that sys.exit() is reported through ExitCode with
// CPython's status mapping, and that the instance stays usable afterwards.
func TestEvalSystemExit(t *testing.T) {
	p := newPy(t, Config{})
	cases := []struct {
		src  string
		code int
	}{
		{"import sys\nsys.exit()", 0},
		{"import sys\nsys.exit(3)", 3},
		{"raise SystemExit(7)", 7},
		{"raise SystemExit('bye')", 1},
	}
	for _, c := range cases {
		r, err := p.Eval(ctx, c.src)
		if err != nil {
			t.Fatalf("%q: %v", c.src, err)
		}
		code, ok := ExitCode(r.Error)
		if !ok || code != c.code {
			t.Errorf("%q: ExitCode = %d,%v (err=%v), want %d", c.src, code, ok, r.Error, c.code)
		}
		if c.src == "raise SystemExit('bye')" && !strings.Contains(r.Stderr, "bye") {
			t.Errorf("non-int exit code not printed to stderr: %q", r.Stderr)
		}
	}
	if got := evalRepr(t, p, "1+1"); got != "2" {
		t.Errorf("post-exit eval broken: %q", got)
	}
	if _, ok := ExitCode(errors.New("other")); ok {
		t.Error("ExitCode accepted an unrelated error")
	}
}

// TestConfigStdio verifies the per-instance stdio redirection knobs:
// Config.Stdin feeds guest fd 0, Config.Stdout receives guest fd 1, and
// Python-level print() is still captured into Result by the bridge.
func TestConfigStdio(t *testing.T) {
	// print() captured by the bridge even though fd 1 defaults to discard.
	p := newPy(t, Config{})
	if r := mustEval(t, p, "print('hello-bridge')"); !strings.Contains(r.Stdout, "hello-bridge") {
		t.Errorf("print() not captured in Result.Stdout: %q", r.Stdout)
	}

	// Raw fd 1 writes are routed to Config.Stdout.
	var out bytes.Buffer
	p2 := newPy(t, Config{Stdout: &out})
	mustEval(t, p2, "import os; os.write(1, b'raw-fd1')")
	if !strings.Contains(out.String(), "raw-fd1") {
		t.Errorf("fd 1 not routed to Config.Stdout: %q", out.String())
	}

	// Config.Stdin backs sys.stdin.
	p3 := newPy(t, Config{Stdin: strings.NewReader("line-in\nsecond\n")})
	mustEval(t, p3, "import sys")
	if got := evalRepr(t, p3, "sys.stdin.read()"); !strings.Contains(got, "line-in") || !strings.Contains(got, "second") {
		t.Errorf("Config.Stdin not delivered to sys.stdin: %q", got)
	}
}

// TestConfigMaxMemory verifies the linear-memory cap: an instance boots and
// runs under a generous cap, and an allocation past the cap raises a Python
// MemoryError instead of trapping the host or growing memory unbounded.
func TestConfigMaxMemory(t *testing.T) {
	p := newPy(t, Config{MaxMemoryBytes: 64 << 20})
	if got := evalRepr(t, p, "1 + 1"); got != "2" {
		t.Fatalf("eval under cap: %q", got)
	}
	r, err := p.Eval(ctx, "b = bytearray(200_000_000)")
	if err != nil {
		t.Fatalf("allocation past cap trapped the host (cap not enforced cleanly): %v", err)
	}
	var pe *PythonError
	if !errors.As(r.Error, &pe) || pe.Type != "MemoryError" {
		t.Errorf("expected MemoryError under a 64 MiB cap, got %v", r.Error)
	}
}

// allowExec grants every subprocess spawn (the zero Config denies them).
func allowExec(string, []string) bool { return true }

// TestSubprocessRun exercises host-provided subprocess: subprocess.run spawns
// a host binary (via the posix_spawn -> proc_spawn host import) with inherited
// stdio and reports its exit code.
func TestSubprocessRun(t *testing.T) {
	p := newPy(t, Config{Exec: allowExec})
	mustEval(t, p, "import subprocess\nrc = subprocess.run(['/bin/echo', 'hi']).returncode")
	if got := evalRepr(t, p, "rc"); got != "0" {
		t.Errorf("returncode = %s, want 0", got)
	}
}

// TestSubprocessExitCode verifies a non-zero child exit is reported.
func TestSubprocessExitCode(t *testing.T) {
	p := newPy(t, Config{Exec: allowExec})
	mustEval(t, p, "import subprocess\nrc = subprocess.run(['/bin/sh', '-c', 'exit 7']).returncode")
	if got := evalRepr(t, p, "rc"); got != "7" {
		t.Errorf("returncode = %s, want 7", got)
	}
}

// TestSubprocessExecHook verifies the Config.Exec whitelist gates spawns: a
// denied spawn surfaces as PermissionError, and the hook sees the path.
func TestSubprocessExecHook(t *testing.T) {
	var seen string
	p := newPy(t, Config{Exec: func(path string, argv []string) bool {
		seen = path
		return false
	}})
	out := pyTry(t, p, "import subprocess\nsubprocess.run(['/bin/echo', 'x'])")
	if seen != "/bin/echo" {
		t.Errorf("exec hook not consulted, saw %q", seen)
	}
	if !strings.Contains(out, "PermissionError") {
		t.Errorf("expected PermissionError, got %s", out)
	}
}

// TestSubprocessCaptureOutput exercises subprocess.run(capture_output=True):
// the child's stdout is wired to a host pipe (the Pipe + proc_spawn host
// imports) and read back into r.stdout rather than inherited.
//
// This is also a regression for a wasm2go asm-codegen miscompile: the
// capture path runs through a transpiled helper whose scan-loop carry was
// wrongly coalesced into a caller-save register that a CALL on the loop-exit
// path clobbered, corrupting a later address computation and segfaulting the
// guest. The bug only reproduced on the asm build path (amd64/arm64, not
// purego); the pure-Go bounds-checked path masked it.
func TestSubprocessCaptureOutput(t *testing.T) {
	p := newPy(t, Config{Exec: allowExec})
	mustEval(t, p, "import subprocess\nr = subprocess.run(['/bin/echo', 'cap-works'], capture_output=True)\nout = (r.returncode, r.stdout)")
	got := evalRepr(t, p, "out")
	if !strings.Contains(got, "cap-works") {
		t.Errorf("captured stdout = %s, want it to contain b'cap-works\\n'", got)
	}
	if !strings.HasPrefix(got, "(0,") {
		t.Errorf("returncode != 0 in %s", got)
	}
}

// TestConcurrentIsolation: multiple instances, each its own wasm instance,
// run concurrently with fully independent global state.
func TestConcurrentIsolation(t *testing.T) {
	const N = 4
	insts := make([]*Python, N)
	for k := 0; k < N; k++ {
		insts[k] = newPy(t, Config{})
	}

	var wg sync.WaitGroup
	for k := 0; k < N; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			mustEval(t, insts[k], fmt.Sprintf("marker = %d", (k+1)*100))
		}(k)
	}
	wg.Wait()

	errs := make([]error, N)
	wg = sync.WaitGroup{}
	for k := 0; k < N; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			if _, err := insts[k].Eval(ctx, "s = 0\nfor _ in range(2000):\n    s += 1\n"); err != nil {
				errs[k] = err
				return
			}
			r, err := insts[k].Eval(ctx, "marker")
			if err != nil {
				errs[k] = err
				return
			}
			n, _ := As[IntValue](r.Value)
			if v, _ := n.Int64(); v != int64((k+1)*100) {
				errs[k] = fmt.Errorf("instance %d marker=%v want %d", k, r.Value, (k+1)*100)
			}
		}(k)
	}
	wg.Wait()
	for k, err := range errs {
		if err != nil {
			t.Errorf("instance %d: %v", k, err)
		}
	}
}

// TestEvalContextCancel: a `while True: pass` is stopped by cancelling the
// context — a host watchdog goroutine trips the eval-breaker with a memory
// write, no wasm code executed on the busy instance — and Eval returns
// ctx.Err(). The instance stays usable afterwards.
func TestEvalContextCancel(t *testing.T) {
	p := newPy(t, Config{})

	cctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.Eval(cctx, "while True:\n    pass\n")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Eval(infinite) err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("interruption took %v", elapsed)
	}

	// The interpreter must remain usable after recovering from the interrupt,
	// and a cancellable ctx that does NOT fire must leave no interrupt behind.
	cctx2, cancel2 := context.WithTimeout(ctx, time.Minute)
	defer cancel2()
	r, err := p.Eval(cctx2, "1+1")
	if err != nil || r.Error != nil {
		t.Fatalf("post-interrupt eval: %v %v", err, r.Error)
	}
	if got := evalRepr(t, p, "sum(range(10))"); got != "45" {
		t.Fatalf("post-interrupt eval broken: %q", got)
	}
}

// TestZeroConfigDeniesCapabilities pins the fail-closed default: with a zero
// Config, name resolution, outbound connections, and subprocess spawns are
// all refused, and the host filesystem is invisible.
func TestZeroConfigDeniesCapabilities(t *testing.T) {
	p := newPy(t, Config{})

	if out := pyTry(t, p, "import subprocess\nsubprocess.run(['/bin/echo', 'x'])"); !strings.Contains(out, "PermissionError") {
		t.Errorf("subprocess with zero Config: %q, want PermissionError", out)
	}
	if out := pyTry(t, p, "import socket\nsocket.getaddrinfo('example.com', 80)"); !strings.HasPrefix(out, "gaierror") {
		t.Errorf("resolve with zero Config: %q, want socket.gaierror", out)
	}
	if out := pyTry(t, p, "import socket\ns = socket.socket(socket.AF_INET, socket.SOCK_STREAM)\ns.connect(('93.184.216.34', 80))"); !strings.Contains(out, "Error") {
		t.Errorf("connect with zero Config: %q, want an error", out)
	}
	if out := pyTry(t, p, "open('/etc/hosts').read()"); !strings.Contains(out, "FileNotFoundError") {
		t.Errorf("host file visible with zero Config: %q", out)
	}
}

// TestDirFSScopesGuestRoot verifies fs.DirFS: the directory becomes the
// guest's "/", files inside it are readable and writable, and nothing outside
// it is reachable.
func TestDirFSScopesGuestRoot(t *testing.T) {
	// The extracted stdlib directory doubles as the guest root, so the
	// interpreter can boot from it with StdlibDir "/".
	root, err := gopythonfs.ExtractStdlib()
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(root, "ok.txt"), "visible")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, outside, "classified")

	p := newPy(t, Config{
		FS:        gopythonfs.DirFS(root),
		StdlibDir: "/",
	})

	if got := evalRepr(t, p, "open('/ok.txt').read()"); got != "'visible'" {
		t.Errorf("read /ok.txt = %q", got)
	}
	if out := pyTry(t, p, fmt.Sprintf("open(%q).read()", outside)); !strings.Contains(out, "FileNotFoundError") {
		t.Errorf("file outside the root was reachable: %q", out)
	}
	mustEval(t, p, "open('/new.txt', 'w').write('hello')")
	if got, err := os.ReadFile(filepath.Join(root, "new.txt")); err != nil || string(got) != "hello" {
		t.Errorf("new.txt = %q err=%v, want hello", got, err)
	}
}

// TestEnvIsolation confirms the guest sees exactly the Env we provide and not
// the host process environment.
func TestEnvIsolation(t *testing.T) {
	const sentinel = "PYWASM_SENTINEL_DO_NOT_LEAK"
	t.Setenv(sentinel, "1")

	p := newPy(t, Config{Env: []string{"APP_MODE=sandbox"}})
	mustEval(t, p, "import os")
	if got := evalRepr(t, p, "os.environ.get('APP_MODE')"); got != "'sandbox'" {
		t.Errorf("APP_MODE = %q, want 'sandbox'", got)
	}
	if got := evalRepr(t, p, fmt.Sprintf("os.environ.get(%q) is None", sentinel)); got != "True" {
		t.Errorf("host env var leaked into guest: %q", got)
	}
}

// requireNetwork skips the calling test when outbound access to example.com:80
// is unavailable, so the network-dependent socket tests degrade gracefully in
// offline / sandboxed CI rather than failing.
func requireNetwork(t *testing.T) {
	t.Helper()
	c, err := net.DialTimeout("tcp", "example.com:80", 5*time.Second)
	if err != nil {
		t.Skipf("outbound network to example.com unavailable: %v", err)
	}
	_ = c.Close()
}

// httpProbe resolves example.com, connects, issues a minimal HTTP/1.0 GET and
// prints the response status line. It exercises getaddrinfo + connect + send
// + recv against a real host. It resolves AF_INET explicitly and uses a plain
// blocking socket (no create_connection: it would spend ~20s on an IPv6
// attempt; no settimeout(): the guest poll path is slow in this sandbox).
const httpProbe = `
import socket
addr = socket.getaddrinfo("example.com", 80, socket.AF_INET, socket.SOCK_STREAM)[0][4]
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.connect(addr)
s.sendall(b"GET / HTTP/1.0\r\nHost: example.com\r\nConnection: close\r\n\r\n")
resp = s.recv(64)
s.close()
print(resp.split(b"\r\n", 1)[0].decode())
`

func allowDial(string, string, string, int) bool { return true }
func allowResolve(string) bool                   { return true }

// TestOutboundConnect is the core outbound-networking check: Python resolves
// example.com, connects, and round-trips an HTTP request — exercising the
// host-provided socket()/getaddrinfo()/connect() plus the sock_send/sock_recv
// path, and that struct addrinfo crosses the shim with the right layout.
func TestOutboundConnect(t *testing.T) {
	requireNetwork(t)
	p := newPy(t, Config{Dial: allowDial, Resolve: allowResolve})
	r := mustEval(t, p, httpProbe)
	if !strings.HasPrefix(strings.TrimSpace(r.Stdout), "HTTP/") {
		t.Fatalf("HTTP round-trip to example.com failed: stdout=%q stderr=%q", r.Stdout, r.Stderr)
	}
}

// TestOutboundDialWhitelist verifies the connect whitelist: a denied
// destination fails, an allowed one succeeds. The hook sees the name, the
// resolved IP, and port 80.
func TestOutboundDialWhitelist(t *testing.T) {
	requireNetwork(t)

	var seen []string
	deny := newPy(t, Config{
		Resolve: allowResolve,
		Dial: func(network, host, ip string, port int) bool {
			seen = append(seen, fmt.Sprintf("%s/%s:%d", host, ip, port))
			return false
		},
	})
	out := pyTry(t, deny, "import socket\naddr = socket.getaddrinfo('example.com', 80, socket.AF_INET, socket.SOCK_STREAM)[0][4]\ns = socket.socket(socket.AF_INET, socket.SOCK_STREAM)\ns.connect(addr)")
	if !strings.Contains(out, "Error") {
		t.Fatalf("denied connect should raise OSError, got %q", out)
	}
	if len(seen) == 0 || !strings.HasSuffix(seen[0], ":80") {
		t.Fatalf("dial hook saw %v, want an example.com IP on port 80", seen)
	}

	allow := newPy(t, Config{Dial: allowDial, Resolve: allowResolve})
	if r := mustEval(t, allow, httpProbe); !strings.HasPrefix(strings.TrimSpace(r.Stdout), "HTTP/") {
		t.Fatalf("allowed connect failed: stdout=%q stderr=%q", r.Stdout, r.Stderr)
	}
}

// TestOutboundResolveWhitelist verifies the hostname whitelist via getaddrinfo:
// a blocked name fails to resolve, an allowed name resolves and connects.
func TestOutboundResolveWhitelist(t *testing.T) {
	requireNetwork(t)
	p := newPy(t, Config{
		Dial:    allowDial,
		Resolve: func(host string) bool { return host == "example.com" },
	})
	if out := pyTry(t, p, "import socket; socket.getaddrinfo('blocked.invalid', 80)"); !strings.HasPrefix(out, "gaierror") {
		t.Fatalf("blocked resolve should raise socket.gaierror, got %q", out)
	}
	if r := mustEval(t, p, httpProbe); !strings.HasPrefix(strings.TrimSpace(r.Stdout), "HTTP/") {
		t.Fatalf("resolve+connect round-trip for example.com failed: stdout=%q", r.Stdout)
	}
}

// TestPerInstanceFSIsolation gives each instance its OWN in-memory filesystem
// and verifies that filesystem changes made by one are invisible to another.
func TestPerInstanceFSIsolation(t *testing.T) {
	a := newPy(t, Config{})
	b := newPy(t, Config{})

	mustEval(t, a, "open('/note.txt','w').write('from-A')")
	if got := evalRepr(t, a, "open('/note.txt').read()"); got != "'from-A'" {
		t.Fatalf("A could not read its own file: %q", got)
	}
	if out := pyTry(t, b, "open('/note.txt').read()"); !strings.Contains(out, "FileNotFoundError") {
		t.Fatalf("B saw A's file (FS not isolated): %q", out)
	}
	mustEval(t, b, "open('/note.txt','w').write('from-B')")
	if got := evalRepr(t, b, "open('/note.txt').read()"); got != "'from-B'" {
		t.Fatalf("B could not read its own file: %q", got)
	}
	if got := evalRepr(t, a, "open('/note.txt').read()"); got != "'from-A'" {
		t.Fatalf("A's file was affected by B's write (FS leak): %q", got)
	}
	mustEval(t, a, "import os")
	mustEval(t, b, "import os")
	mustEval(t, a, "os.mkdir('/only_in_a')")
	if got := evalRepr(t, a, "os.path.isdir('/only_in_a')"); got != "True" {
		t.Fatalf("A could not see its own dir: %q", got)
	}
	if got := evalRepr(t, b, "os.path.isdir('/only_in_a')"); got != "False" {
		t.Fatalf("B saw A's directory (FS not isolated): %q", got)
	}
}

// TestRunFileAndAddPath runs a script as __main__ from the instance's
// filesystem, with sys.argv set and its directory importable, and checks
// output streams to Config.Stdout rather than being captured.
func TestRunFileAndAddPath(t *testing.T) {
	fsys, err := gopythonfs.NewStdlibMemFS()
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll("app/lib", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("app/helper.py", []byte("def twice(x):\n    return 2 * x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("app/lib/extra.py", []byte("VALUE = 'extra'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("app/main.py", []byte(`
import sys
import helper
import extra
print("name", __name__)
print("argv", sys.argv)
print("twice", helper.twice(21), extra.VALUE)
if __name__ == "__main__":
    sys.exit(int(sys.argv[1]))
`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	p := newPy(t, Config{FS: fsys, Stdout: &out})
	if err := p.AddPath(ctx, "/app/lib"); err != nil {
		t.Fatal(err)
	}
	err = p.RunFile(ctx, "/app/main.py", []string{"5"})
	if code, ok := ExitCode(err); !ok || code != 5 {
		t.Fatalf("RunFile err = %v, want exit 5", err)
	}
	got := out.String()
	for _, want := range []string{"name __main__", "argv ['/app/main.py', '5']", "twice 42 extra"} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q: %q", want, got)
		}
	}

	// An uncaught exception in the script is a *PythonError naming the file.
	if err := fsys.WriteFile("app/boom.py", []byte("x = 1\nraise RuntimeError('oops')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var pe *PythonError
	if err := p.RunFile(ctx, "/app/boom.py", nil); !errors.As(err, &pe) || pe.Type != "RuntimeError" || !strings.Contains(pe.Traceback, "/app/boom.py") {
		t.Errorf("RunFile(boom) = %v", err)
	}
}

// TestUseAfterClose pins that every entry point errors once the instance is
// closed, and that outstanding handles are inert rather than crashing.
func TestUseAfterClose(t *testing.T) {
	p, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	r := mustEval(t, p, "[1, 2, 3]")
	lst, err := As[ListValue](r.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := p.Eval(ctx, "1"); err == nil {
		t.Error("Eval after Close succeeded")
	}
	if _, err := lst.Len(ctx); err == nil {
		t.Error("handle op after Close succeeded")
	}
	if _, err := p.Import(ctx, "json"); err == nil {
		t.Error("Import after Close succeeded")
	}
}

// --- helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pyTry runs src that is expected to raise, returning the exception text
// ("Type: message"). It wraps src in a try/except so it works for
// multi-statement programs.
func pyTry(t *testing.T, p *Python, src string) string {
	t.Helper()
	indented := "    " + strings.ReplaceAll(src, "\n", "\n    ")
	wrapped := "try:\n" + indented + "\nexcept Exception as e:\n    print(f'{type(e).__name__}: {e}')"
	r, err := p.Eval(ctx, wrapped)
	if err != nil {
		t.Fatalf("pyTry host error: %v", err)
	}
	if r.Error != nil {
		t.Fatalf("pyTry harness error: %v", r.Error)
	}
	return strings.TrimSpace(r.Stdout)
}
