package fs

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"testing"
)

func readAll(t *testing.T, fsys *MemFS, name string) []byte {
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

// The standard library is shared between MemFS values; every way of
// modifying a file must give the modifying MemFS its own copy and leave the
// others (and the process-wide cache) untouched.
func TestStdlibMemFSCopyOnWrite(t *testing.T) {
	ref, err := NewStdlibMemFS()
	if err != nil {
		t.Fatal(err)
	}
	orig := readAll(t, ref, "os.py")
	if len(orig) < 16 {
		t.Fatalf("os.py unexpectedly short: %d bytes", len(orig))
	}

	tests := []struct {
		name   string
		modify func(t *testing.T, fsys *MemFS)
	}{
		{"WriteAt within the file", func(t *testing.T, fsys *MemFS) {
			f, err := fsys.OpenFile("os.py", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte("XXXX"), 4); err != nil {
				t.Fatal(err)
			}
		}},
		{"Write at the start", func(t *testing.T, fsys *MemFS) {
			f, err := fsys.OpenFile("os.py", os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("XXXX")); err != nil {
				t.Fatal(err)
			}
		}},
		{"append", func(t *testing.T, fsys *MemFS) {
			f, err := fsys.OpenFile("os.py", os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("XXXX")); err != nil {
				t.Fatal(err)
			}
		}},
		{"O_TRUNC then write", func(t *testing.T, fsys *MemFS) {
			f, err := fsys.OpenFile("os.py", os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("XXXX")); err != nil {
				t.Fatal(err)
			}
		}},
		{"Truncate shorter then write", func(t *testing.T, fsys *MemFS) {
			f, err := fsys.OpenFile("os.py", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(8); err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte("XXXX"), 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"Truncate longer", func(t *testing.T, fsys *MemFS) {
			f, err := fsys.OpenFile("os.py", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(int64(len(orig)) + 10); err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteAt([]byte("XXXX"), 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"WriteFile over it", func(t *testing.T, fsys *MemFS) {
			if err := fsys.WriteFile("os.py", []byte("XXXX"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys, err := NewStdlibMemFS()
			if err != nil {
				t.Fatal(err)
			}
			tt.modify(t, fsys)
			if got := readAll(t, fsys, "os.py"); !bytes.Contains(got, []byte("XXXX")) {
				t.Fatalf("modification not visible in the modified FS")
			}
			if got := readAll(t, ref, "os.py"); !bytes.Equal(got, orig) {
				t.Fatalf("modification leaked into another MemFS")
			}
			fresh, err := NewStdlibMemFS()
			if err != nil {
				t.Fatal(err)
			}
			if got := readAll(t, fresh, "os.py"); !bytes.Equal(got, orig) {
				t.Fatalf("modification leaked into the shared stdlib cache")
			}
		})
	}
}

// Building a stdlib MemFS must not copy the library: per instance it should
// cost the directory tree, not the ~10 MB of file contents.
func TestStdlibMemFSDoesNotCopyContents(t *testing.T) {
	if _, err := NewStdlibMemFS(); err != nil { // warm the shared cache
		t.Fatal(err)
	}
	const n = 10
	keep := make([]*MemFS, 0, n)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range n {
		fsys, err := NewStdlibMemFS()
		if err != nil {
			t.Fatal(err)
		}
		keep = append(keep, fsys)
	}
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(keep)
	perFS := (after.TotalAlloc - before.TotalAlloc) / n
	var contents uint64
	entries, _ := loadStdlibEntries()
	for _, e := range entries {
		contents += uint64(len(e.data))
	}
	if perFS > contents/10 {
		t.Fatalf("NewStdlibMemFS allocated %d bytes per call; the library contents are %d bytes", perFS, contents)
	}
}
