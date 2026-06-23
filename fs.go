package python

// Filesystem backend API.
//
// An Interpreter opens every file through an FS backend (Config.FS). By
// default that is the host filesystem; supplying a custom FS — e.g. an
// in-memory MemFS — gives the interpreter a private, arbitrary filesystem so
// its reads/writes never touch disk and are invisible to other interpreters.
// These are re-exports of the generic backend defined in the wasm2go runtime
// so callers only need this package.

import (
	"archive/zip"
	"bytes"
	"fmt"

	"github.com/goccy/pythonwasm2go/base"
)

// FS is the read/write filesystem backend an Interpreter is given via
// Config.FS. It is a write-capable superset of io/fs.FS.
type FS = base.FS

// File is an open file returned by FS.OpenFile.
type File = base.File

// MemFS is an in-memory read/write FS. Separate MemFS values are fully
// isolated from one another.
type MemFS = base.MemFS

// NewMemFS returns an empty in-memory filesystem.
func NewMemFS() *MemFS { return base.NewMemFS() }

// NewStdlibMemFS returns an in-memory filesystem pre-loaded with the embedded
// Python standard library at the root, ready to back an Interpreter:
//
//	fs := python.NewStdlibMemFS()
//	interp, _ := python.NewInterpreter(python.Config{FS: fs}) // StdlibDir = "/"
//
// Each call returns an independent FS, so interpreters built from separate
// NewStdlibMemFS() values share no filesystem state.
func NewStdlibMemFS() (*MemFS, error) {
	zr, err := zip.NewReader(bytes.NewReader(stdlibZip), int64(len(stdlibZip)))
	if err != nil {
		return nil, fmt.Errorf("open embedded stdlib: %w", err)
	}
	fsys := base.NewMemFS()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			if err := fsys.MkdirAll(f.Name, 0o755); err != nil {
				return nil, err
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			rc.Close()
			return nil, err
		}
		rc.Close()
		if err := fsys.WriteFile(f.Name, buf.Bytes(), 0o644); err != nil {
			return nil, err
		}
	}
	return fsys, nil
}
