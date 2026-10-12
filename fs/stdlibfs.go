package fs

// The standard library as a shared, read-only mount.
//
// Every instance on the default filesystem needs the whole standard library,
// and no instance needs to change it. So the library is decompressed once per
// process into an immutable tree, and every instance mounts that same tree
// read-only at StdlibDir on top of its own MemFS: the library costs memory
// once per process, not once per instance, and an instance's writes all land
// in its private MemFS.

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// StdlibDir is where WithStdlib mounts the standard library: the default
// python.Config.StdlibDir.
const StdlibDir = "/" + stdlibMount

// stdlibMount is StdlibDir as an FS name (no leading slash).
const stdlibMount = "usr/lib/python3.14"

// WithStdlib returns an FS that serves fsys with the embedded standard
// library mounted read-only at StdlibDir. It is what a nil python.Config.FS
// defaults to (on an empty MemFS); build one explicitly to add files of your
// own:
//
//	mem := fs.NewMemFS()
//	mem.WriteFile("app.py", src, 0o644)
//	fsys, _ := fs.WithStdlib(mem)
//	p, _ := python.New(python.Config{FS: fsys})
//
// The library itself is shared by every FS WithStdlib returns and cannot be
// modified: writing, creating, removing or renaming anything under StdlibDir
// fails with a permission error. Everything else is fsys, so instances given
// separate MemFS values share no writable state. Files that fsys itself holds
// under StdlibDir are hidden by the mount; WithStdlib refuses a fsys that
// already has any.
func WithStdlib(fsys *MemFS) (FS, error) {
	lib, err := loadStdlibTree()
	if err != nil {
		return nil, err
	}
	// The mount point and its parents exist in fsys, so they show up when the
	// guest lists "/", "usr" or "usr/lib" and resolves paths through them.
	if err := fsys.MkdirAll(stdlibMount, 0o755); err != nil {
		return nil, fmt.Errorf("mount the standard library: %w", err)
	}
	if err := checkEmptyDir(fsys, stdlibMount); err != nil {
		return nil, err
	}
	return &stdlibMountFS{upper: fsys, lib: lib}, nil
}

func checkEmptyDir(fsys *MemFS, name string) error {
	f, err := fsys.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	ents, err := f.ReadDir(1)
	if err != nil && err != io.EOF {
		return err
	}
	if len(ents) > 0 {
		return fmt.Errorf("mount the standard library: %s already holds files", StdlibDir)
	}
	return nil
}

// stdlibMountFS is upper with lib mounted at stdlibMount.
type stdlibMountFS struct {
	upper *MemFS
	lib   *stdlibTree
}

// route cleans name and reports which backend serves it, with the name that
// backend should see.
func route(name string) (rel string, inLib bool) {
	c := strings.TrimPrefix(path.Clean("/"+name), "/")
	if c == stdlibMount {
		return "", true
	}
	if rest, ok := strings.CutPrefix(c, stdlibMount+"/"); ok {
		return rest, true
	}
	return c, false
}

// isMountParent reports whether the cleaned upper name is a directory the
// mount point lives under ("usr", "usr/lib"). Removing or renaming one, or
// renaming over one, would detach the mount from the tree the guest sees.
func isMountParent(c string) bool {
	return c != "" && strings.HasPrefix(stdlibMount, c+"/")
}

func readOnly(op, name string) error {
	return &iofs.PathError{Op: op, Path: name, Err: iofs.ErrPermission}
}

func (m *stdlibMountFS) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	rel, inLib := route(name)
	if inLib {
		return m.lib.open(rel, name, flag)
	}
	return m.upper.OpenFile(rel, flag, perm)
}

func (m *stdlibMountFS) Mkdir(name string, perm os.FileMode) error {
	rel, inLib := route(name)
	if inLib {
		if _, err := m.lib.lookup(rel); err == nil {
			return &iofs.PathError{Op: "mkdir", Path: name, Err: iofs.ErrExist}
		}
		return readOnly("mkdir", name)
	}
	return m.upper.Mkdir(rel, perm)
}

func (m *stdlibMountFS) Remove(name string) error {
	rel, inLib := route(name)
	if inLib || isMountParent(rel) {
		return readOnly("remove", name)
	}
	return m.upper.Remove(rel)
}

func (m *stdlibMountFS) Rename(oldName, newName string) error {
	oldRel, oldInLib := route(oldName)
	newRel, newInLib := route(newName)
	if oldInLib || newInLib || isMountParent(oldRel) || isMountParent(newRel) {
		return readOnly("rename", oldName)
	}
	return m.upper.Rename(oldRel, newRel)
}

func (m *stdlibMountFS) Stat(name string) (os.FileInfo, error) {
	rel, inLib := route(name)
	if inLib {
		n, err := m.lib.lookup(rel)
		if err != nil {
			return nil, &iofs.PathError{Op: "stat", Path: name, Err: err}
		}
		return n.info, nil
	}
	return m.upper.Stat(rel)
}

// The library has no links, so Lstat is Stat there.
func (m *stdlibMountFS) Lstat(name string) (os.FileInfo, error) {
	rel, inLib := route(name)
	if inLib {
		return m.Stat(name)
	}
	return m.upper.Lstat(rel)
}

func (m *stdlibMountFS) Symlink(oldName, newName string) error {
	rel, inLib := route(newName)
	if inLib {
		return readOnly("symlink", newName)
	}
	return m.upper.Symlink(oldName, rel)
}

func (m *stdlibMountFS) Link(oldName, newName string) error {
	oldRel, oldInLib := route(oldName)
	newRel, newInLib := route(newName)
	if oldInLib || newInLib {
		return readOnly("link", newName)
	}
	return m.upper.Link(oldRel, newRel)
}

func (m *stdlibMountFS) Readlink(name string) (string, error) {
	rel, inLib := route(name)
	if inLib {
		return "", &iofs.PathError{Op: "readlink", Path: name, Err: iofs.ErrInvalid}
	}
	return m.upper.Readlink(rel)
}

// Chtimes keeps the runtime's optional utimes capability that MemFS has.
func (m *stdlibMountFS) Chtimes(name string, atime, mtime time.Time) error {
	rel, inLib := route(name)
	if inLib {
		return readOnly("chtimes", name)
	}
	return m.upper.Chtimes(rel, atime, mtime)
}

// stdlibTree is the decompressed standard library. It is built once per
// process and never modified afterwards, so every instance reads it without
// locking.
type stdlibTree struct {
	root *stdlibNode
}

type stdlibNode struct {
	info     stdlibInfo
	data     []byte
	children map[string]*stdlibNode
	names    []string // children's names, sorted, for ReadDir
}

func (t *stdlibTree) lookup(name string) (*stdlibNode, error) {
	n := t.root
	if name == "" {
		return n, nil
	}
	for _, part := range strings.Split(name, "/") {
		if !n.info.IsDir() {
			return nil, iofs.ErrNotExist
		}
		c, ok := n.children[part]
		if !ok {
			return nil, iofs.ErrNotExist
		}
		n = c
	}
	return n, nil
}

func (t *stdlibTree) open(rel, name string, flag int) (File, error) {
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		return nil, readOnly("open", name)
	}
	n, err := t.lookup(rel)
	if err != nil {
		return nil, &iofs.PathError{Op: "open", Path: name, Err: err}
	}
	return &stdlibFile{node: n}, nil
}

var (
	stdlibTreeOnce sync.Once
	stdlibTreeVal  *stdlibTree
	stdlibTreeErr  error
)

// loadStdlibTree decompresses stdlib.zip once per process.
func loadStdlibTree() (*stdlibTree, error) {
	stdlibTreeOnce.Do(func() {
		stdlibTreeVal, stdlibTreeErr = buildStdlibTree()
	})
	return stdlibTreeVal, stdlibTreeErr
}

func buildStdlibTree() (*stdlibTree, error) {
	zr, err := zip.NewReader(bytes.NewReader(stdlibZip), int64(len(stdlibZip)))
	if err != nil {
		return nil, fmt.Errorf("open embedded stdlib: %w", err)
	}
	newDir := func(name string, modTime time.Time) *stdlibNode {
		return &stdlibNode{
			info:     stdlibInfo{name: name, mode: os.ModeDir | 0o555, modTime: modTime},
			children: map[string]*stdlibNode{},
		}
	}
	root := newDir(path.Base(StdlibDir), time.Unix(0, 0))
	// dir returns the directory node for the slash-separated parts, creating
	// any the zip does not list explicitly.
	dir := func(parts []string, modTime time.Time) (*stdlibNode, error) {
		n := root
		for _, part := range parts {
			c, ok := n.children[part]
			if !ok {
				c = newDir(part, modTime)
				n.children[part] = c
			} else if !c.info.IsDir() {
				return nil, fmt.Errorf("embedded stdlib: %q is both a file and a directory", part)
			}
			n = c
		}
		return n, nil
	}
	for _, f := range zr.File {
		parts := strings.Split(strings.Trim(f.Name, "/"), "/")
		for _, p := range parts {
			if p == "" || p == "." || p == ".." {
				return nil, fmt.Errorf("illegal path in stdlib zip: %q", f.Name)
			}
		}
		if f.FileInfo().IsDir() {
			if _, err := dir(parts, f.Modified); err != nil {
				return nil, err
			}
			continue
		}
		parent, err := dir(parts[:len(parts)-1], f.Modified)
		if err != nil {
			return nil, err
		}
		data, err := readZipFile(f)
		if err != nil {
			return nil, err
		}
		base := parts[len(parts)-1]
		parent.children[base] = &stdlibNode{
			info: stdlibInfo{name: base, size: int64(len(data)), mode: 0o444, modTime: f.Modified},
			data: data,
		}
	}
	var index func(n *stdlibNode)
	index = func(n *stdlibNode) {
		for name, c := range n.children {
			n.names = append(n.names, name)
			if c.info.IsDir() {
				index(c)
			}
		}
		sort.Strings(n.names)
	}
	index(root)
	return &stdlibTree{root: root}, nil
}

func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

type stdlibInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
}

func (fi stdlibInfo) Name() string       { return fi.name }
func (fi stdlibInfo) Size() int64        { return fi.size }
func (fi stdlibInfo) Mode() os.FileMode  { return fi.mode }
func (fi stdlibInfo) ModTime() time.Time { return fi.modTime }
func (fi stdlibInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi stdlibInfo) Sys() any           { return nil }

type stdlibDirEntry struct{ info stdlibInfo }

func (e stdlibDirEntry) Name() string               { return e.info.name }
func (e stdlibDirEntry) IsDir() bool                { return e.info.IsDir() }
func (e stdlibDirEntry) Type() os.FileMode          { return e.info.mode.Type() }
func (e stdlibDirEntry) Info() (os.FileInfo, error) { return e.info, nil }

// stdlibFile is an open, read-only handle on a library node. The node is
// shared and immutable; only the handle's own offsets change.
type stdlibFile struct {
	node   *stdlibNode
	mu     sync.Mutex
	off    int64
	dirOff int
}

func (f *stdlibFile) Name() string               { return f.node.info.name }
func (f *stdlibFile) Close() error               { return nil }
func (f *stdlibFile) Sync() error                { return nil }
func (f *stdlibFile) Stat() (os.FileInfo, error) { return f.node.info, nil }

func (f *stdlibFile) Write([]byte) (int, error) { return 0, readOnly("write", f.Name()) }
func (f *stdlibFile) WriteAt([]byte, int64) (int, error) {
	return 0, readOnly("write", f.Name())
}
func (f *stdlibFile) Truncate(int64) error { return readOnly("truncate", f.Name()) }

func (f *stdlibFile) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.node.info.IsDir() {
		return 0, &iofs.PathError{Op: "read", Path: f.Name(), Err: iofs.ErrInvalid}
	}
	if f.off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.node.data[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *stdlibFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.node.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *stdlibFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch whence {
	case io.SeekStart:
		f.off = offset
	case io.SeekCurrent:
		f.off += offset
	case io.SeekEnd:
		f.off = int64(len(f.node.data)) + offset
	}
	return f.off, nil
}

func (f *stdlibFile) ReadDir(n int) ([]os.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.node.info.IsDir() {
		return nil, &iofs.PathError{Op: "readdir", Path: f.Name(), Err: iofs.ErrInvalid}
	}
	names := f.node.names
	if f.dirOff >= len(names) {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	end := len(names)
	if n > 0 && f.dirOff+n < end {
		end = f.dirOff + n
	}
	out := make([]os.DirEntry, 0, end-f.dirOff)
	for _, name := range names[f.dirOff:end] {
		out = append(out, stdlibDirEntry{f.node.children[name].info})
	}
	f.dirOff = end
	return out, nil
}
