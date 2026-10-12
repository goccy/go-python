package fs

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"runtime"
	"sort"
	"testing"
)

func newStdlibFS(t *testing.T) (FS, *MemFS) {
	t.Helper()
	mem := NewMemFS()
	fsys, err := WithStdlib(mem)
	if err != nil {
		t.Fatal(err)
	}
	return fsys, mem
}

func zipContent(t *testing.T, name string) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(stdlibZip), int64(len(stdlibZip)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := zr.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readFile(t *testing.T, fsys FS, name string) []byte {
	t.Helper()
	f, err := fsys.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func dirNames(t *testing.T, fsys FS, name string) []string {
	t.Helper()
	f, err := fsys.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	ents, err := f.ReadDir(-1)
	if err != nil {
		t.Fatalf("readdir %s: %v", name, err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestWithStdlibServesTheLibrary(t *testing.T) {
	fsys, _ := newStdlibFS(t)
	want := zipContent(t, "os.py")
	for _, name := range []string{
		stdlibMount + "/os.py",
		"/" + stdlibMount + "/os.py",
		stdlibMount + "/json/../os.py",
	} {
		if got := readFile(t, fsys, name); !bytes.Equal(got, want) {
			t.Errorf("%s: content differs from the embedded os.py", name)
		}
	}
	fi, err := fsys.Stat(stdlibMount + "/json")
	if err != nil || !fi.IsDir() {
		t.Fatalf("stat json = %v, %v; want a directory", fi, err)
	}
	names := dirNames(t, fsys, stdlibMount)
	if !sort.StringsAreSorted(names) {
		t.Errorf("library listing is not sorted: %v", names)
	}
	for _, want := range []string{"os.py", "json"} {
		if i := sort.SearchStrings(names, want); i == len(names) || names[i] != want {
			t.Errorf("library listing lacks %s", want)
		}
	}
	// The mount point is reachable by listing its parents.
	for dir, child := range map[string]string{"": "usr", "usr": "lib", "usr/lib": "python3.14"} {
		if got := dirNames(t, fsys, dir); len(got) != 1 || got[0] != child {
			t.Errorf("listing %q = %v, want [%s]", dir, got, child)
		}
	}
}

func TestWithStdlibIsReadOnly(t *testing.T) {
	fsys, mem := newStdlibFS(t)
	lib := stdlibMount + "/"
	isPerm := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, iofs.ErrPermission) {
			t.Errorf("%s: err = %v, want a permission error", what, err)
		}
	}
	for _, flag := range []int{os.O_WRONLY, os.O_RDWR, os.O_RDONLY | os.O_TRUNC, os.O_WRONLY | os.O_APPEND} {
		_, err := fsys.OpenFile(lib+"os.py", flag, 0)
		isPerm("open os.py for writing", err)
	}
	_, err := fsys.OpenFile(lib+"new.py", os.O_WRONLY|os.O_CREATE, 0o644)
	isPerm("create new.py", err)
	isPerm("mkdir __pycache__", fsys.Mkdir(lib+"__pycache__", 0o755))
	if err := fsys.Mkdir(lib+"json", 0o755); !errors.Is(err, iofs.ErrExist) {
		t.Errorf("mkdir json: err = %v, want exist", err)
	}
	isPerm("remove os.py", fsys.Remove(lib+"os.py"))
	isPerm("remove the mount point", fsys.Remove(stdlibMount))
	isPerm("rename os.py out", fsys.Rename(lib+"os.py", "os.py"))

	f, err := fsys.OpenFile(lib+"os.py", os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, err = f.Write([]byte("x"))
	isPerm("write through a read-only handle", err)
	isPerm("truncate through a read-only handle", f.Truncate(0))

	// The directories holding the mount point cannot be moved or replaced.
	if err := mem.WriteFile("x", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	isPerm("rename usr", fsys.Rename("usr", "elsewhere"))
	isPerm("rename usr/lib", fsys.Rename("usr/lib", "elsewhere"))
	isPerm("rename over usr", fsys.Rename("x", "usr"))
	isPerm("remove usr/lib", fsys.Remove("usr/lib"))
}

func TestWithStdlibWritesGoToTheMemFS(t *testing.T) {
	fsys, mem := newStdlibFS(t)
	f, err := fsys.OpenFile("app.py", os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("x = 1\n")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, mem, "app.py"); string(got) != "x = 1\n" {
		t.Fatalf("app.py in the MemFS = %q", got)
	}
	if err := fsys.Mkdir("usr/local", 0o755); err != nil {
		t.Fatalf("mkdir beside the mount point: %v", err)
	}
	// Another instance's FS sees none of it.
	other, _ := newStdlibFS(t)
	if _, err := other.Stat("app.py"); !errors.Is(err, iofs.ErrNotExist) {
		t.Fatalf("app.py leaked into another FS: %v", err)
	}
}

func TestWithStdlibRefusesAnOccupiedMountPoint(t *testing.T) {
	mem := NewMemFS()
	if err := mem.WriteFile(stdlibMount+"/os.py", []byte("shadowed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := WithStdlib(mem); err == nil {
		t.Fatal("WithStdlib mounted over files the MemFS holds")
	}
}

// Mounting the library must not copy it: per FS it should cost a handful of
// directories, not the ~10 MB of file contents.
func TestWithStdlibSharesTheLibrary(t *testing.T) {
	newStdlibFS(t) // decompress the library
	const n = 10
	keep := make([]FS, 0, n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range n {
		fsys, _ := newStdlibFS(t)
		keep = append(keep, fsys)
	}
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(keep)
	if perFS := (after.TotalAlloc - before.TotalAlloc) / n; perFS > 64<<10 {
		t.Fatalf("WithStdlib allocated %d bytes per call", perFS)
	}
}
