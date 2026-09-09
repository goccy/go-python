package python

// Filesystem backend API.
//
// A Python instance opens every file through an FS backend (Config.FS). The
// default is a private in-memory MemFS pre-loaded with the standard library
// (NewStdlibMemFS), so an embedding never touches the host disk unless asked
// to; the go-python/fs package provides the host-backed alternatives
// (fs.NewHostFS, fs.DirFS). These are re-exports so callers only need this
// package for the common case.

import (
	"archive/zip"
	"bytes"
	"fmt"

	gopythonfs "github.com/goccy/go-python/fs"
)

// FS is the read/write filesystem backend a Python instance is given via
// Config.FS. See the go-python/fs package for the provided backends.
type FS = gopythonfs.FS

// File is an open file returned by FS.OpenFile.
type File = gopythonfs.File

// MemFS is an in-memory read/write FS. Separate MemFS values are fully
// isolated from one another.
type MemFS = gopythonfs.MemFS

// NewMemFS returns an empty in-memory filesystem.
func NewMemFS() *MemFS { return gopythonfs.NewMemFS() }

// NewStdlibMemFS returns an in-memory filesystem pre-loaded with the embedded
// Python standard library at the root, ready to back a Python instance. It is
// what a nil Config.FS defaults to; build one explicitly to add files of your
// own before New:
//
//	fsys, _ := python.NewStdlibMemFS()
//	fsys.WriteFile("app.py", src, 0o644)
//	p, _ := python.New(python.Config{FS: fsys}) // StdlibDir defaults to "/"
//
// Each call returns an independent FS, so instances built from separate
// NewStdlibMemFS() values share no filesystem state.
func NewStdlibMemFS() (*MemFS, error) {
	zr, err := zip.NewReader(bytes.NewReader(stdlibZip), int64(len(stdlibZip)))
	if err != nil {
		return nil, fmt.Errorf("open embedded stdlib: %w", err)
	}
	fsys := gopythonfs.NewMemFS()
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
