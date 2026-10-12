// Package fs provides the filesystem backends a Python instance runs on.
//
// Every file the interpreter opens goes through one FS value
// (python.Config.FS):
//
//   - NewMemFS gives a private in-memory filesystem — nothing touches the
//     host disk, and separate MemFS values are fully isolated (this backs
//     the library default, pre-loaded with the standard library);
//   - NewHostFS passes straight through to the operating system's
//     filesystem — how the python command behaves;
//   - DirFS is the host filesystem scoped to one directory, which becomes
//     the guest's root;
//   - any other implementation of FS plugs in the same way.
package fs

import (
	"os"
	"path/filepath"

	"github.com/goccy/pythonwasm2go/base"
)

// FS is the read/write filesystem backend a Python instance is given via
// python.Config.FS. It is a write-capable superset of io/fs.FS. Names are
// guest paths relative to the backend's root: slash-separated with no
// leading slash ("" is the root). Methods should return the standard
// io/fs errors so the runtime maps them to the right guest errno.
type FS = base.FS

// File is an open file returned by FS.OpenFile.
type File = base.File

// MemFS is an in-memory read/write FS. Separate MemFS values are fully
// isolated from one another. Safe for concurrent use.
type MemFS = base.MemFS

// NewMemFS returns an empty in-memory filesystem.
func NewMemFS() *MemFS { return base.NewMemFS() }

// NewHostFS returns a pass-through to the operating system's filesystem:
// the guest sees the host's real root, so absolute paths mean the same
// thing inside and outside the instance. This is how the python command
// behaves.
func NewHostFS() FS { return hostFS{} }

// DirFS returns the host filesystem scoped to dir: dir becomes the guest's
// root, and nothing outside it is reachable.
func DirFS(dir string) FS { return hostFS{root: dir} }

// hostFS is a thin pass-through to the host filesystem, optionally scoped
// to root. It mirrors the runtime's own OS backend, so behavior matches an
// instance running directly on the host filesystem.
type hostFS struct{ root string }

func (o hostFS) join(name string) string {
	if o.root == "" || o.root == "/" {
		return "/" + name
	}
	return filepath.Join(o.root, name)
}

func (o hostFS) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	f, err := os.OpenFile(o.join(name), flag, perm)
	if err != nil {
		return nil, err
	}
	return f, nil
}
func (o hostFS) Mkdir(name string, perm os.FileMode) error { return os.Mkdir(o.join(name), perm) }
func (o hostFS) Chmod(name string, mode os.FileMode) error { return os.Chmod(o.join(name), mode) }
func (o hostFS) Remove(name string) error                  { return os.Remove(o.join(name)) }
func (o hostFS) Rename(a, b string) error                  { return os.Rename(o.join(a), o.join(b)) }
func (o hostFS) Stat(name string) (os.FileInfo, error)     { return os.Stat(o.join(name)) }
func (o hostFS) Lstat(name string) (os.FileInfo, error)    { return os.Lstat(o.join(name)) }
func (o hostFS) Symlink(target, name string) error         { return os.Symlink(target, o.join(name)) }
func (o hostFS) Readlink(name string) (string, error)      { return os.Readlink(o.join(name)) }
func (o hostFS) Link(a, b string) error                    { return os.Link(o.join(a), o.join(b)) }
