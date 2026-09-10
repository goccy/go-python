package fs

// Self-contained Python standard library.
//
// stdlib.zip is a trimmed CPython Lib/ tree (no test suites, idlelib, tkinter,
// distutils, ...) embedded into this module so an embedding application does
// not have to ship the Lib/ directory alongside its binary. The library
// default serves it from memory (NewStdlibMemFS, what a nil python.Config.FS
// gets); ExtractStdlib is the host-filesystem alternative: it unpacks the tree
// once per process into a temp directory and returns that path, suitable for
// python.Config.StdlibDir when the FS is a host backend (NewHostFS).

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed stdlib.zip
var stdlibZip []byte

// NewStdlibMemFS returns an in-memory filesystem pre-loaded with the embedded
// Python standard library at the root, ready to back a Python instance. It is
// what a nil python.Config.FS defaults to; build one explicitly to add files
// of your own before python.New:
//
//	fsys, _ := fs.NewStdlibMemFS()
//	fsys.WriteFile("app.py", src, 0o644)
//	p, _ := python.New(python.Config{FS: fsys}) // StdlibDir defaults to "/"
//
// Each call returns an independent FS, so instances built from separate
// NewStdlibMemFS() values share no filesystem state.
func NewStdlibMemFS() (*MemFS, error) {
	entries, err := loadStdlibEntries()
	if err != nil {
		return nil, err
	}
	fsys := NewMemFS()
	for _, e := range entries {
		if e.dir {
			if err := fsys.MkdirAll(e.name, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		// WriteFile copies the bytes, so every MemFS owns its files and the
		// shared decompressed cache is never written through.
		if err := fsys.WriteFile(e.name, e.data, 0o644); err != nil {
			return nil, err
		}
	}
	return fsys, nil
}

// stdlibEntry is one decompressed member of stdlib.zip.
type stdlibEntry struct {
	name string
	dir  bool
	data []byte
}

var (
	stdlibEntriesOnce sync.Once
	stdlibEntries     []stdlibEntry
	stdlibEntriesErr  error
)

// loadStdlibEntries inflates stdlib.zip once per process. Every instance
// built on the default filesystem needs the whole tree, and inflating it per
// instance dominated New (tens of milliseconds); populating a MemFS from the
// cached bytes is a fraction of that.
func loadStdlibEntries() ([]stdlibEntry, error) {
	stdlibEntriesOnce.Do(func() {
		zr, err := zip.NewReader(bytes.NewReader(stdlibZip), int64(len(stdlibZip)))
		if err != nil {
			stdlibEntriesErr = fmt.Errorf("open embedded stdlib: %w", err)
			return
		}
		entries := make([]stdlibEntry, 0, len(zr.File))
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				entries = append(entries, stdlibEntry{name: f.Name, dir: true})
				continue
			}
			rc, err := f.Open()
			if err != nil {
				stdlibEntriesErr = err
				return
			}
			var buf bytes.Buffer
			if _, err := buf.ReadFrom(rc); err != nil {
				rc.Close()
				stdlibEntriesErr = err
				return
			}
			rc.Close()
			entries = append(entries, stdlibEntry{name: f.Name, data: buf.Bytes()})
		}
		stdlibEntries = entries
	})
	return stdlibEntries, stdlibEntriesErr
}

var (
	stdlibOnce sync.Once
	stdlibPath string
	stdlibErr  error
)

// ExtractStdlib unpacks the embedded standard library into a temporary
// directory (once per process) and returns its path. The result is cached, so
// repeated calls — e.g. one per instance — are cheap. With a host backend
// (NewHostFS) the returned directory is the value to pass as
// python.Config.StdlibDir.
func ExtractStdlib() (string, error) {
	stdlibOnce.Do(func() {
		stdlibPath, stdlibErr = extractStdlibTo("")
	})
	return stdlibPath, stdlibErr
}

// extractStdlibTo unpacks the embedded zip under parent (os.MkdirTemp default
// when empty).
func extractStdlibTo(parent string) (string, error) {
	dir, err := os.MkdirTemp(parent, "go-python-stdlib-")
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(stdlibZip), int64(len(stdlibZip)))
	if err != nil {
		return "", fmt.Errorf("open embedded stdlib: %w", err)
	}
	for _, f := range zr.File {
		// Guard against zip-slip: the cleaned target must stay under dir.
		target := filepath.Join(dir, f.Name)
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) && target != filepath.Clean(dir) {
			return "", fmt.Errorf("illegal path in stdlib zip: %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if err := extractStdlibFile(f, target); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func extractStdlibFile(f *zip.File, target string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, rc); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
