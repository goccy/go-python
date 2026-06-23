package python

// End-to-end behavioural suite for the go-python wasm2go backend, driven
// through the multi-interpreter Interpreter API. Covers: a smoke test of the
// wasm2go eval path, normal execution and stdlib, per-interpreter stdio and
// memory-cap config, host-provided subprocess, concurrent isolated
// interpreters, interrupting an infinite loop, filesystem whitelist control,
// environment isolation, the outbound-network posture of this WASI preview1
// build, and per-interpreter filesystem isolation.

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newInst builds an interpreter from cfg. With cfg.StdlibDir empty (the usual
// case) NewInterpreter extracts the embedded standard library, so the tests
// need no on-disk stdlib path.
func newInst(t *testing.T, cfg Config) *Interpreter {
	t.Helper()
	inst, err := NewInterpreter(cfg)
	if err != nil {
		t.Fatalf("NewInterpreter: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return inst
}

// mustEval evaluates and fails on a host error or a Python exception.
func mustEval(t *testing.T, i *Interpreter, src string) EvalResult {
	t.Helper()
	r, err := i.Eval(src)
	if err != nil {
		t.Fatalf("Eval(%q) host error: %v", src, err)
	}
	if !r.Ok {
		t.Fatalf("Eval(%q) python error: %s", src, r.Error)
	}
	return r
}

// TestGate2Wasm2GoEval is a smoke test of the wasm2go CPython backend through
// the recommended Interpreter API (NewInterpreter / Eval). It covers
// arithmetic, a stdlib import, print() capture, and json round-tripping
// against the embedded standard library — so it needs no on-disk stdlib path.
func TestGate2Wasm2GoEval(t *testing.T) {
	i := newInst(t, Config{})

	if r := mustEval(t, i, "1 + 1"); r.Repr != "2" {
		t.Errorf("1+1 = %q, want 2", r.Repr)
	}

	mustEval(t, i, "import math, json")

	if r := mustEval(t, i, "math.sqrt(2)"); r.Repr != "1.4142135623730951" {
		t.Errorf("math.sqrt(2) = %q, want 1.4142135623730951", r.Repr)
	}

	if r := mustEval(t, i, "print('hi wasm2go')"); r.Stdout != "hi wasm2go\n" {
		t.Errorf("print stdout = %q, want %q", r.Stdout, "hi wasm2go\n")
	}

	if r := mustEval(t, i, "json.dumps({'a': 1})"); r.Repr != `'{"a": 1}'` {
		t.Errorf("json.dumps = %q, want %q", r.Repr, `'{"a": 1}'`)
	}
}

// TestNormalAndStdlib exercises ordinary evaluation plus a spread of stdlib
// modules to confirm the bundled standard library is importable and functional.
func TestNormalAndStdlib(t *testing.T) {
	i := newInst(t, Config{})

	if r := mustEval(t, i, "sum(range(101))"); r.Repr != "5050" {
		t.Errorf("sum(range(101)) = %q, want 5050", r.Repr)
	}
	// Persistent globals across calls (REPL semantics).
	mustEval(t, i, "acc = 0")
	for n := 1; n <= 5; n++ {
		mustEval(t, i, "acc += 1")
	}
	if r := mustEval(t, i, "acc"); r.Repr != "5" {
		t.Errorf("persistent acc = %q, want 5", r.Repr)
	}

	// Import once (statement, exec mode), then evaluate each case as a bare
	// expression so py_eval returns its repr.
	mustEval(t, i, "import math, base64, hashlib, json, re, collections, textwrap, functools, datetime")
	cases := []struct{ src, want string }{
		{"round(math.factorial(10))", "3628800"},
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
		r := mustEval(t, i, c.src)
		if r.Repr != c.want {
			t.Errorf("%s = %q, want %q (stderr=%q)", c.src, r.Repr, c.want, r.Stderr)
		}
	}
}

// TestEmbeddedStdlib proves the module is self-contained: NewInterpreter with a
// zero-value Config (no StdlibDir) extracts the embedded standard library and
// runs imports against it, with no external standard-library tree on disk.
func TestEmbeddedStdlib(t *testing.T) {
	inst, err := NewInterpreter(Config{}) // StdlibDir empty → embedded stdlib
	if err != nil {
		t.Fatalf("NewInterpreter with embedded stdlib: %v", err)
	}
	defer inst.Close()

	mustEval(t, inst, "import json, base64, hashlib, textwrap")
	if r := mustEval(t, inst, "json.dumps(sorted(set([3,1,2])))"); r.Repr != "'[1, 2, 3]'" {
		t.Errorf("embedded-stdlib json = %q, want '[1, 2, 3]'", r.Repr)
	}
	if r := mustEval(t, inst, "hashlib.md5(b'x').hexdigest()[:6]"); r.Repr != "'9dd4e4'" {
		t.Errorf("embedded-stdlib hashlib = %q", r.Repr)
	}
}

// TestConfigStdio verifies the per-interpreter stdio redirection knobs:
// Config.Stdin feeds guest fd 0, Config.Stdout receives guest fd 1, and
// Python-level print() is still captured into EvalResult by the bridge.
func TestConfigStdio(t *testing.T) {
	// print() captured by the bridge even though fd 1 defaults to discard.
	ip, err := NewInterpreter(Config{})
	if err != nil {
		t.Fatal(err)
	}
	r, err := ip.Eval("print('hello-bridge')")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Stdout, "hello-bridge") {
		t.Errorf("print() not captured in EvalResult.Stdout: %q", r.Stdout)
	}
	if err := ip.Close(); err != nil {
		t.Errorf("close: %v", err)
	}

	// Raw fd 1 writes are routed to Config.Stdout.
	var out bytes.Buffer
	ip2, err := NewInterpreter(Config{Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := ip2.Eval("import os; os.write(1, b'raw-fd1')"); err != nil || !r.Ok {
		t.Fatalf("os.write: err=%v r=%+v", err, r)
	}
	if !strings.Contains(out.String(), "raw-fd1") {
		t.Errorf("fd 1 not routed to Config.Stdout: %q", out.String())
	}
	if err := ip2.Close(); err != nil {
		t.Errorf("close: %v", err)
	}

	// Config.Stdin backs sys.stdin (read in eval mode for a repr).
	ip3, err := NewInterpreter(Config{Stdin: strings.NewReader("line-in\nsecond\n")})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := ip3.Eval("import sys"); err != nil || !r.Ok {
		t.Fatalf("import sys: %v %+v", err, r)
	}
	r3, err := ip3.Eval("sys.stdin.read()")
	if err != nil || !r3.Ok {
		t.Fatalf("stdin read: %v %+v", err, r3)
	}
	if !strings.Contains(r3.Repr, "line-in") || !strings.Contains(r3.Repr, "second") {
		t.Errorf("Config.Stdin not delivered to sys.stdin: %q", r3.Repr)
	}
	if err := ip3.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

// TestConfigMaxMemory verifies the linear-memory cap: an interpreter boots and
// runs under a generous cap, and an allocation past the cap raises a Python
// MemoryError instead of trapping the host or growing memory unbounded.
func TestConfigMaxMemory(t *testing.T) {
	ip, err := NewInterpreter(Config{MaxMemoryBytes: 64 << 20})
	if err != nil {
		t.Fatalf("boot under 64 MiB cap: %v", err)
	}
	defer ip.Close()
	if r, err := ip.Eval("1 + 1"); err != nil || !r.Ok {
		t.Fatalf("eval under cap: %v %+v", err, r)
	}
	r, err := ip.Eval("b = bytearray(200_000_000)")
	if err != nil {
		t.Fatalf("allocation past cap trapped the host (cap not enforced cleanly): %v", err)
	}
	if r.Ok {
		t.Errorf("expected failure allocating 200 MB under a 64 MiB cap, got OK")
	}
	if !strings.Contains(r.Error, "MemoryError") {
		t.Errorf("expected MemoryError, got: %s", r.Error)
	}
}

// TestSubprocessRun exercises host-provided subprocess: subprocess.run spawns
// a host binary (via the posix_spawn -> proc_spawn host import) with inherited
// stdio and reports its exit code.
func TestSubprocessRun(t *testing.T) {
	ip, err := NewInterpreter(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ip.Close()
	run := `
import subprocess
rc = subprocess.run(["/bin/echo", "hi"]).returncode
`
	if r, err := ip.Eval(run); err != nil || !r.Ok {
		t.Fatalf("run: err=%v r=%+v", err, r)
	}
	r, err := ip.Eval("rc")
	if err != nil || !r.Ok {
		t.Fatalf("rc: err=%v r=%+v", err, r)
	}
	if strings.TrimSpace(r.Repr) != "0" {
		t.Errorf("returncode = %s, want 0", r.Repr)
	}
}

// TestSubprocessExitCode verifies a non-zero child exit is reported.
func TestSubprocessExitCode(t *testing.T) {
	ip, err := NewInterpreter(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ip.Close()
	run := `
import subprocess
rc = subprocess.run(["/bin/sh", "-c", "exit 7"]).returncode
`
	if r, err := ip.Eval(run); err != nil || !r.Ok {
		t.Fatalf("run: err=%v r=%+v", err, r)
	}
	r, _ := ip.Eval("rc")
	if strings.TrimSpace(r.Repr) != "7" {
		t.Errorf("returncode = %s, want 7", r.Repr)
	}
}

// TestSubprocessExecHook verifies the Config.Exec whitelist gates spawns: a
// denied spawn surfaces as PermissionError, and the hook sees the path.
func TestSubprocessExecHook(t *testing.T) {
	var seen string
	ip, err := NewInterpreter(Config{Exec: func(path string, argv []string) bool {
		seen = path
		return false
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer ip.Close()
	src := `
import subprocess
try:
    subprocess.run(["/bin/echo", "x"])
    out = "NO-ERROR"
except PermissionError:
    out = "PermissionError"
except Exception as e:
    out = type(e).__name__
`
	if r, err := ip.Eval(src); err != nil || !r.Ok {
		t.Fatalf("eval: err=%v r=%+v", err, r)
	}
	r, _ := ip.Eval("out")
	if seen != "/bin/echo" {
		t.Errorf("exec hook not consulted, saw %q", seen)
	}
	if !strings.Contains(r.Repr, "PermissionError") {
		t.Errorf("expected PermissionError, got %s", r.Repr)
	}
}

// TestSubprocessCaptureOutput exercises subprocess.run(capture_output=True):
// the child's stdout is wired to a host pipe (the Pipe + proc_spawn host
// imports) and read back into r.stdout rather than inherited.
//
// This is also a regression for a wasm2go asm-codegen miscompile: the
// capture path runs through a transpiled helper (Fn7078) whose scan-loop carry
// was wrongly coalesced into a caller-save register that a CALL on the loop-
// exit path clobbered, corrupting a later address computation and segfaulting
// the guest. The bug only reproduced on the asm build path (amd64/arm64, not
// purego); the pure-Go bounds-checked path masked it. See the wasm2go
// regalloc coalesce pass (liveAcrossExternalCall) for the fix.
func TestSubprocessCaptureOutput(t *testing.T) {
	ip, err := NewInterpreter(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer ip.Close()
	src := `
import subprocess
r = subprocess.run(["/bin/echo", "cap-works"], capture_output=True)
out = (r.returncode, r.stdout)
`
	if r, err := ip.Eval(src); err != nil || !r.Ok {
		t.Fatalf("eval: err=%v r=%+v", err, r)
	}
	r, _ := ip.Eval("out")
	if !strings.Contains(r.Repr, "cap-works") {
		t.Errorf("captured stdout = %s, want it to contain b'cap-works\\n'", r.Repr)
	}
	if !strings.HasPrefix(strings.TrimSpace(r.Repr), "(0,") {
		t.Errorf("returncode != 0 in %s", r.Repr)
	}
}

// TestConcurrentIsolation is Gate 3: multiple interpreters, each its own wasm
// instance, run concurrently with fully independent global state.
func TestConcurrentIsolation(t *testing.T) {
	const N = 4
	insts := make([]*Interpreter, N)
	for k := 0; k < N; k++ {
		insts[k] = newInst(t, Config{})
	}

	// Seed a distinct global in each interpreter, concurrently.
	var wg sync.WaitGroup
	for k := 0; k < N; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			mustEval(t, insts[k], fmt.Sprintf("marker = %d", (k+1)*100))
		}(k)
	}
	wg.Wait()

	// Each interpreter must see only its own marker, and a compute-heavy
	// loop in one must not disturb the others. Run reads concurrently.
	errs := make([]error, N)
	wg = sync.WaitGroup{}
	for k := 0; k < N; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			// Some work to interleave execution across instances.
			work := `
s = 0
for _ in range(2000):
    s += 1
`
			if _, err := insts[k].Eval(work); err != nil {
				errs[k] = err
				return
			}
			r, err := insts[k].Eval("marker")
			if err != nil {
				errs[k] = err
				return
			}
			want := fmt.Sprintf("%d", (k+1)*100)
			if r.Repr != want {
				errs[k] = fmt.Errorf("instance %d marker=%q want %q", k, r.Repr, want)
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

// TestInterruptInfiniteLoop is Gate 4: a `while True: pass` is stopped from a
// host watchdog goroutine via the eval-breaker memory write, with no wasm code
// executed on the busy instance.
func TestInterruptInfiniteLoop(t *testing.T) {
	i := newInst(t, Config{})

	// Resolve the interrupt addresses BEFORE the loop starts (resolving
	// needs the per-instance lock, which the running Eval will hold).
	ip, err := i.PrepareInterrupt()
	if err != nil {
		t.Fatalf("PrepareInterrupt: %v", err)
	}

	done := make(chan EvalResult, 1)
	go func() {
		r, err := i.Eval(`
while True:
    pass
`)
		if err != nil {
			t.Errorf("Eval(infinite) host error: %v", err)
			done <- EvalResult{}
			return
		}
		done <- r
	}()

	// Let the loop spin, then fire the interrupt.
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	ip.Fire()

	select {
	case r := <-done:
		elapsed := time.Since(start)
		if r.Ok {
			t.Fatalf("interrupted eval reported ok=true (repr=%q); expected KeyboardInterrupt", r.Repr)
		}
		if !strings.Contains(r.Error, "KeyboardInterrupt") {
			t.Fatalf("expected KeyboardInterrupt, got error: %s", r.Error)
		}
		t.Logf("interrupted after %v; error=%q", elapsed, firstLine(r.Error))
	case <-time.After(15 * time.Second):
		t.Fatal("infinite loop was not interrupted within 15s")
	}

	// The interpreter must remain usable after recovering from the interrupt.
	if r := mustEval(t, i, "1+1"); r.Repr != "2" {
		t.Fatalf("post-interrupt eval broken: repr=%q", r.Repr)
	}
}

// TestFilesystemWhitelist verifies the host-controlled FS policy: reads of a
// blocked file fail, reads of an allowed file succeed, writes into a
// read-only subtree fail, and writes into a writable subtree succeed — all
// scoped under a preopen tempdir.
func TestFilesystemWhitelist(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "ok.txt"), "visible")
	mustWrite(t, filepath.Join(root, "secret.txt"), "classified")
	if err := os.MkdirAll(filepath.Join(root, "ro"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "rw"), 0o755); err != nil {
		t.Fatal(err)
	}

	// The host "/" stays visible (so the interpreter can read its stdlib);
	// the whitelist is enforced by the FSAccess hook. The hook sees the
	// guest path (host path minus the leading "/"), so we match the test
	// files by suffix and the read-only subtree by substring. Stdlib reads
	// (from the extracted standard library) are never writes and never match
	// secret.txt, so they pass.
	i := newInst(t, Config{
		FSAccess: func(path string, write bool) bool {
			if strings.HasSuffix(path, "/secret.txt") {
				return false // never readable
			}
			if write && strings.Contains(path, "/ro/") {
				return false // ro/ is read-only
			}
			return true
		},
	})

	// Allowed read.
	if r := mustEval(t, i, fmt.Sprintf("open(%q).read()", filepath.Join(root, "ok.txt"))); r.Repr != "'visible'" {
		t.Errorf("read ok.txt = %q, want 'visible'", r.Repr)
	}
	// Blocked read → PermissionError.
	r := pyTry(t, i, fmt.Sprintf("open(%q).read()", filepath.Join(root, "secret.txt")))
	if !strings.Contains(r, "PermissionError") {
		t.Errorf("read secret.txt: want PermissionError, got %q", r)
	}
	// Blocked write into ro/.
	r = pyTry(t, i, fmt.Sprintf("open(%q,'w').write('x')", filepath.Join(root, "ro", "new.txt")))
	if !strings.Contains(r, "PermissionError") {
		t.Errorf("write ro/new.txt: want PermissionError, got %q", r)
	}
	if _, err := os.Stat(filepath.Join(root, "ro", "new.txt")); !os.IsNotExist(err) {
		t.Errorf("denied write must not create file (err=%v)", err)
	}
	// Allowed write into rw/.
	mustEval(t, i, fmt.Sprintf("open(%q,'w').write('hello')", filepath.Join(root, "rw", "new.txt")))
	got, err := os.ReadFile(filepath.Join(root, "rw", "new.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("rw/new.txt = %q err=%v, want 'hello'", got, err)
	}
}

// TestEnvIsolation confirms the guest sees exactly the Env we provide and not
// the host process environment.
func TestEnvIsolation(t *testing.T) {
	const sentinel = "PYWASM_SENTINEL_DO_NOT_LEAK"
	t.Setenv(sentinel, "1") // present in the host process now

	i := newInst(t, Config{Env: []string{"APP_MODE=sandbox"}})

	mustEval(t, i, "import os")
	if r := mustEval(t, i, "os.environ.get('APP_MODE')"); r.Repr != "'sandbox'" {
		t.Errorf("APP_MODE = %q, want 'sandbox'", r.Repr)
	}
	if r := mustEval(t, i, fmt.Sprintf("os.environ.get(%q) is None", sentinel)); r.Repr != "True" {
		t.Errorf("host env var leaked into guest: %q", r.Repr)
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
// prints the response status line (e.g. "HTTP/1.1 200 OK"). It exercises
// getaddrinfo + connect + send + recv against a real, well-known host, which
// makes "did outbound networking work?" self-evident. The \r\n escapes are
// interpreted by Python, not Go (this is a raw string), so the request is a
// valid CRLF-delimited HTTP message.
//
// It resolves AF_INET explicitly and uses a plain blocking socket. Two
// deliberate avoidances keep it fast and non-flaky in this sandbox:
//   - not socket.create_connection: it asks for AF_UNSPEC and spends ~20s on
//     an IPv6 attempt before falling back to IPv4.
//   - no settimeout(): a socket with a timeout goes through the guest poll
//     path, where recv currently takes ~20s instead of returning when data is
//     ready (a known limitation of the WASI socket host). The requireNetwork
//     guard already confirms example.com is reachable, so blocking is safe.
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

// TestOutboundConnect is the core outbound-networking check: Python resolves
// example.com, connects, and round-trips an HTTP request — exercising the
// host-provided socket()/getaddrinfo()/connect() plus the sock_send/sock_recv
// path.
func TestOutboundConnect(t *testing.T) {
	requireNetwork(t)
	i := newInst(t, Config{})
	r := mustEval(t, i, httpProbe)
	if !strings.HasPrefix(strings.TrimSpace(r.Stdout), "HTTP/") {
		t.Fatalf("HTTP round-trip to example.com failed: stdout=%q stderr=%q", r.Stdout, r.Stderr)
	}
}

// TestOutboundDialWhitelist verifies the connect (IP) whitelist: a denied
// destination fails, an allowed one succeeds. The hook sees example.com's
// resolved IP and port 80.
func TestOutboundDialWhitelist(t *testing.T) {
	requireNetwork(t)

	// Deny every outbound connection.
	var seen []string
	deny := newInst(t, Config{
		Dial: func(network, ip string, p int) bool {
			seen = append(seen, fmt.Sprintf("%s:%d", ip, p))
			return false
		},
	})
	denySrc := `
import socket
addr = socket.getaddrinfo("example.com", 80, socket.AF_INET, socket.SOCK_STREAM)[0][4]
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.connect(addr)
`
	out := pyTry(t, deny, denySrc)
	if !strings.Contains(out, "OSError") && !strings.Contains(out, "PermissionError") {
		t.Fatalf("denied connect should raise OSError, got %q", out)
	}
	if len(seen) == 0 || !strings.HasSuffix(seen[0], ":80") {
		t.Fatalf("dial hook saw %v, want an example.com IP on port 80", seen)
	}

	// Allow everything → the connect succeeds.
	allow := newInst(t, Config{Dial: func(network, ip string, p int) bool { return true }})
	if r := mustEval(t, allow, httpProbe); !strings.HasPrefix(strings.TrimSpace(r.Stdout), "HTTP/") {
		t.Fatalf("allowed connect failed: stdout=%q stderr=%q", r.Stdout, r.Stderr)
	}
}

// TestOutboundResolveWhitelist verifies the hostname whitelist via getaddrinfo:
// a blocked name fails to resolve, an allowed name resolves and connects
// end-to-end.
func TestOutboundResolveWhitelist(t *testing.T) {
	requireNetwork(t)

	i := newInst(t, Config{
		Resolve: func(host string) bool { return host == "example.com" },
	})

	// Blocked hostname → name resolution error (gaierror is an OSError).
	out := pyTry(t, i, "import socket; socket.getaddrinfo('blocked.invalid', 80)")
	if !strings.Contains(out, "gaierror") && !strings.Contains(out, "Error") {
		t.Fatalf("blocked resolve should raise, got %q", out)
	}

	// Allowed hostname resolves and a full HTTP round-trip succeeds.
	r := mustEval(t, i, httpProbe)
	if !strings.HasPrefix(strings.TrimSpace(r.Stdout), "HTTP/") {
		t.Fatalf("resolve+connect round-trip for example.com failed: stdout=%q", r.Stdout)
	}
}

// TestPerInterpreterFSIsolation gives each interpreter its OWN filesystem root
// (a separate preopen directory, each with its own extracted stdlib) and
// verifies that filesystem changes made by one interpreter are invisible to
// another. This is the per-instance FS-isolation guarantee: one interpreter
// cannot observe (or be affected by) another's file writes.
func TestPerInterpreterFSIsolation(t *testing.T) {
	// newFSInstance builds an interpreter whose entire filesystem is a private,
	// in-memory MemFS (pre-loaded with the stdlib). No disk is touched, and two
	// interpreters share no filesystem state.
	newFSInstance := func() *Interpreter {
		fsys, err := NewStdlibMemFS() // private in-memory FS + its own stdlib
		if err != nil {
			t.Fatalf("NewStdlibMemFS: %v", err)
		}
		inst, err := NewInterpreter(Config{FS: fsys}) // StdlibDir defaults to "/"
		if err != nil {
			t.Fatalf("NewInterpreter: %v", err)
		}
		t.Cleanup(func() { _ = inst.Close() })
		return inst
	}

	a := newFSInstance()
	b := newFSInstance()

	// A writes a file into its own FS and reads it back.
	mustEval(t, a, "open('/note.txt','w').write('from-A')")
	if r := mustEval(t, a, "open('/note.txt').read()"); r.Repr != "'from-A'" {
		t.Fatalf("A could not read its own file: %q", r.Repr)
	}

	// B must NOT see A's file — different filesystem.
	out := pyTry(t, b, "open('/note.txt').read()")
	if !strings.Contains(out, "FileNotFoundError") {
		t.Fatalf("B saw A's file (FS not isolated): %q", out)
	}

	// B writes its OWN /note.txt with different content.
	mustEval(t, b, "open('/note.txt','w').write('from-B')")
	if r := mustEval(t, b, "open('/note.txt').read()"); r.Repr != "'from-B'" {
		t.Fatalf("B could not read its own file: %q", r.Repr)
	}

	// A's file is unchanged by B's write — fully independent filesystems.
	if r := mustEval(t, a, "open('/note.txt').read()"); r.Repr != "'from-A'" {
		t.Fatalf("A's file was affected by B's write (FS leak): %q", r.Repr)
	}

	// Directory creation is also isolated.
	mustEval(t, a, "import os")
	mustEval(t, b, "import os")
	mustEval(t, a, "os.mkdir('/only_in_a')")
	if r := mustEval(t, a, "os.path.isdir('/only_in_a')"); r.Repr != "True" {
		t.Fatalf("A could not see its own dir: %q", r.Repr)
	}
	if r := mustEval(t, b, "os.path.isdir('/only_in_a')"); r.Repr != "False" {
		t.Fatalf("B saw A's directory (FS not isolated): %q", r.Repr)
	}
}

// --- helpers ---------------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// pyTry runs src that is expected to raise, returning the captured exception
// text. It wraps src in a try/except that prints the exception, so it works
// for multi-statement programs (exec mode, where repr is empty).
func pyTry(t *testing.T, i *Interpreter, src string) string {
	t.Helper()
	indented := "    " + strings.ReplaceAll(src, "\n", "\n    ")
	wrapped := "try:\n" + indented + "\nexcept Exception as e:\n    print(f'{type(e).__name__}: {e}')"
	r, err := i.Eval(wrapped)
	if err != nil {
		t.Fatalf("pyTry host error: %v", err)
	}
	if !r.Ok {
		t.Fatalf("pyTry harness error: %s", r.Error)
	}
	return strings.TrimSpace(r.Stdout)
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
