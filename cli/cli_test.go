package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(append([]string{"python"}, args...), strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestRunDashC(t *testing.T) {
	code, out, errb := run(t, "", "-c", "print(1+1)")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if strings.TrimSpace(out) != "2" {
		t.Errorf("stdout=%q, want 2", out)
	}
}

func TestRunDashCNoSpace(t *testing.T) {
	code, out, _ := run(t, "", "-cprint(6*7)")
	if code != 0 || strings.TrimSpace(out) != "42" {
		t.Errorf("exit=%d stdout=%q", code, out)
	}
}

// -c must run in a real __main__ module so `if __name__ == "__main__":` fires.
func TestRunDashCMainModule(t *testing.T) {
	code, out, _ := run(t, "", "-c", "print(__name__)")
	if code != 0 || strings.TrimSpace(out) != "__main__" {
		t.Errorf("exit=%d __name__=%q, want __main__", code, strings.TrimSpace(out))
	}
}

func TestRunDashCArgv(t *testing.T) {
	code, out, errb := run(t, "", "-c", "import sys; print(sys.argv)", "foo", "bar")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if strings.TrimSpace(out) != "['-c', 'foo', 'bar']" {
		t.Errorf("sys.argv=%q", strings.TrimSpace(out))
	}
}

func TestRunStdin(t *testing.T) {
	code, out, errb := run(t, "hello\n", "-c", "import sys; print(sys.stdin.read().strip().upper())")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if strings.TrimSpace(out) != "HELLO" {
		t.Errorf("stdout=%q, want HELLO", out)
	}
}

func TestRunFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prog.py")
	src := `
import sys
print("name", __name__)
print("argv", sys.argv)
if __name__ == "__main__":
    print("total", sum(int(x) for x in sys.argv[1:]))
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errb := run(t, "", path, "3", "4", "5")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "name __main__") {
		t.Errorf("missing __main__: %q", out)
	}
	if !strings.Contains(out, "argv ['"+path+"', '3', '4', '5']") {
		t.Errorf("sys.argv wrong: %q", out)
	}
	if !strings.Contains(out, "total 12") {
		t.Errorf("__main__ block did not run: %q", out)
	}
}

func TestRunFileException(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boom.py")
	src := `
x = 1
raise RuntimeError("oops")
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errb := run(t, "", path)
	if code != 1 {
		t.Errorf("exit=%d, want 1", code)
	}
	if !strings.Contains(errb, "RuntimeError: oops") {
		t.Errorf("stderr missing error: %q", errb)
	}
	// Traceback must reference the script file at the failing line.
	if !strings.Contains(errb, path) {
		t.Errorf("traceback missing script filename %q: %q", path, errb)
	}
}

func TestRunFileNotFound(t *testing.T) {
	code, _, errb := run(t, "", filepath.Join(t.TempDir(), "nope.py"))
	if code != 2 {
		t.Errorf("exit=%d, want 2", code)
	}
	if !strings.Contains(errb, "can't open file") {
		t.Errorf("stderr=%q", errb)
	}
}

func TestRunException(t *testing.T) {
	code, _, errb := run(t, "", "-c", "raise ValueError('boom')")
	if code != 1 {
		t.Errorf("exit=%d, want 1", code)
	}
	if !strings.Contains(errb, "ValueError: boom") {
		t.Errorf("stderr=%q", errb)
	}
}

func TestRunUsageErrors(t *testing.T) {
	if code, _, _ := run(t, ""); code != 2 {
		t.Errorf("no args: exit=%d, want 2", code)
	}
	if code, _, _ := run(t, "", "-c"); code != 2 {
		t.Errorf("-c without code: exit=%d, want 2", code)
	}
	if code, _, _ := run(t, "", "-x"); code != 2 {
		t.Errorf("unknown option: exit=%d, want 2", code)
	}
}
