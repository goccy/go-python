package fs

import (
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemFS is an in-memory read/write FS. Separate MemFS values are fully
// isolated from one another. Safe for concurrent use.
//
// A file's contents may be backed by bytes shared with other MemFS values
// (NewStdlibMemFS backs every instance's standard library with one
// process-wide copy). Shared contents are never written through: the first
// write, truncation or O_TRUNC open of such a file gives this MemFS a
// private copy, so sharing is invisible to everything but memory use.
type MemFS struct {
	mu   sync.Mutex
	root *memNode
}

// NewMemFS returns an empty in-memory filesystem.
func NewMemFS() *MemFS {
	return &MemFS{root: newMemDir("", 0o755, time.Unix(0, 0))}
}

// memNode is a file or directory in a MemFS tree.
type memNode struct {
	name    string
	dir     bool
	mode    os.FileMode
	modTime time.Time
	data    []byte
	// shared reports that data is borrowed from outside this MemFS and
	// must be copied before it is modified (see own).
	shared   bool
	children map[string]*memNode
}

func newMemDir(name string, perm os.FileMode, modTime time.Time) *memNode {
	return &memNode{name: name, dir: true, mode: os.ModeDir | (perm & 0o777), modTime: modTime, children: map[string]*memNode{}}
}

// own makes n.data private to this MemFS so it can be modified in place.
// Caller holds the lock.
func (n *memNode) own() {
	if !n.shared {
		return
	}
	cp := make([]byte, len(n.data))
	copy(cp, n.data)
	n.data = cp
	n.shared = false
}

func memSplit(name string) []string {
	name = strings.Trim(name, "/")
	if name == "" {
		return nil
	}
	raw := strings.Split(name, "/")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		switch p {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, p)
		}
	}
	return out
}

// lookup resolves name to a node. Caller holds fsys.mu.
func (fsys *MemFS) lookup(name string) (*memNode, error) {
	n := fsys.root
	for _, part := range memSplit(name) {
		if !n.dir {
			return nil, fs.ErrNotExist
		}
		c, ok := n.children[part]
		if !ok {
			return nil, fs.ErrNotExist
		}
		n = c
	}
	return n, nil
}

// lookupParent resolves the parent dir of name. Caller holds fsys.mu.
func (fsys *MemFS) lookupParent(name string) (*memNode, string, error) {
	parts := memSplit(name)
	if len(parts) == 0 {
		return nil, "", fs.ErrInvalid
	}
	n := fsys.root
	for _, part := range parts[:len(parts)-1] {
		c, ok := n.children[part]
		if !ok || !c.dir {
			return nil, "", fs.ErrNotExist
		}
		n = c
	}
	return n, parts[len(parts)-1], nil
}

func (fsys *MemFS) OpenFile(name string, flag int, perm os.FileMode) (File, error) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	node, err := fsys.lookup(name)
	if err != nil {
		if flag&os.O_CREATE == 0 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		parent, base, perr := fsys.lookupParent(name)
		if perr != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: perr}
		}
		node = &memNode{name: base, mode: perm & 0o777, modTime: time.Now()}
		parent.children[base] = node
	} else {
		if flag&os.O_EXCL != 0 && flag&os.O_CREATE != 0 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrExist}
		}
		if flag&os.O_TRUNC != 0 && !node.dir {
			if node.shared {
				node.data, node.shared = nil, false
			} else {
				node.data = node.data[:0]
			}
			node.modTime = time.Now()
		}
	}
	f := &memFile{fsys: fsys, node: node}
	if flag&os.O_APPEND != 0 {
		f.off = int64(len(node.data))
	}
	return f, nil
}

func (fsys *MemFS) Mkdir(name string, perm os.FileMode) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	parent, base, err := fsys.lookupParent(name)
	if err != nil {
		return &fs.PathError{Op: "mkdir", Path: name, Err: err}
	}
	if _, ok := parent.children[base]; ok {
		return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
	}
	parent.children[base] = newMemDir(base, perm, time.Now())
	return nil
}

func (fsys *MemFS) Remove(name string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	parent, base, err := fsys.lookupParent(name)
	if err != nil {
		return &fs.PathError{Op: "remove", Path: name, Err: err}
	}
	n, ok := parent.children[base]
	if !ok {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	if n.dir && len(n.children) > 0 {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrInvalid}
	}
	delete(parent.children, base)
	return nil
}

func (fsys *MemFS) Rename(oldName, newName string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	op, ob, err := fsys.lookupParent(oldName)
	if err != nil {
		return &fs.PathError{Op: "rename", Path: oldName, Err: err}
	}
	node, ok := op.children[ob]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrNotExist}
	}
	np, nb, err := fsys.lookupParent(newName)
	if err != nil {
		return &fs.PathError{Op: "rename", Path: newName, Err: err}
	}
	delete(op.children, ob)
	node.name = nb
	np.children[nb] = node
	return nil
}

func (fsys *MemFS) Stat(name string) (os.FileInfo, error) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	n, err := fsys.lookup(name)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	return n.info(), nil
}

func (fsys *MemFS) Lstat(name string) (os.FileInfo, error) { return fsys.Stat(name) }

// MemFS has no symlinks or hard links.
func (fsys *MemFS) Symlink(_, _ string) error { return fs.ErrPermission }
func (fsys *MemFS) Link(_, _ string) error    { return fs.ErrPermission }
func (fsys *MemFS) Readlink(name string) (string, error) {
	return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
}

// Chtimes sets name's modification time (the runtime's optional utimes
// capability).
func (fsys *MemFS) Chtimes(name string, _ time.Time, mtime time.Time) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	n, err := fsys.lookup(name)
	if err != nil {
		return &fs.PathError{Op: "chtimes", Path: name, Err: err}
	}
	n.modTime = mtime
	return nil
}

// MkdirAll creates name and any missing parents. Exposed so embedders can
// populate the FS before handing it to an instance.
func (fsys *MemFS) MkdirAll(name string, perm os.FileMode) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	return fsys.mkdirAll(name, perm)
}

// mkdirAll is MkdirAll with fsys.mu held.
func (fsys *MemFS) mkdirAll(name string, perm os.FileMode) error {
	n := fsys.root
	for _, part := range memSplit(name) {
		c, ok := n.children[part]
		if !ok {
			c = newMemDir(part, perm, time.Now())
			n.children[part] = c
		} else if !c.dir {
			return &fs.PathError{Op: "mkdir", Path: name, Err: fs.ErrExist}
		}
		n = c
	}
	return nil
}

// WriteFile creates (or overwrites) a file with a copy of data, making
// parent directories as needed. Exposed for pre-populating the FS.
func (fsys *MemFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	return fsys.putFile(name, cp, false, perm)
}

// writeShared is WriteFile without the copy: the file borrows data, which
// the caller guarantees nothing will ever modify. This MemFS copies it on
// its first write (see memNode.own).
func (fsys *MemFS) writeShared(name string, data []byte, perm os.FileMode) error {
	return fsys.putFile(name, data, true, perm)
}

func (fsys *MemFS) putFile(name string, data []byte, shared bool, perm os.FileMode) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if parts := memSplit(name); len(parts) > 1 {
		if err := fsys.mkdirAll(strings.Join(parts[:len(parts)-1], "/"), 0o755); err != nil {
			return err
		}
	}
	parent, base, err := fsys.lookupParent(name)
	if err != nil {
		return &fs.PathError{Op: "writefile", Path: name, Err: err}
	}
	parent.children[base] = &memNode{name: base, mode: perm & 0o777, modTime: time.Now(), data: data, shared: shared}
	return nil
}

func (n *memNode) info() os.FileInfo {
	if n.dir {
		return memFileInfo{name: n.name, mode: os.ModeDir | (n.mode & 0o777), modTime: n.modTime}
	}
	return memFileInfo{name: n.name, size: int64(len(n.data)), mode: n.mode & 0o777, modTime: n.modTime}
}

type memFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
}

func (fi memFileInfo) Name() string       { return fi.name }
func (fi memFileInfo) Size() int64        { return fi.size }
func (fi memFileInfo) Mode() os.FileMode  { return fi.mode }
func (fi memFileInfo) ModTime() time.Time { return fi.modTime }
func (fi memFileInfo) IsDir() bool        { return fi.mode.IsDir() }
func (fi memFileInfo) Sys() any           { return nil }

type memDirEntry struct{ info os.FileInfo }

func (e memDirEntry) Name() string               { return e.info.Name() }
func (e memDirEntry) IsDir() bool                { return e.info.IsDir() }
func (e memDirEntry) Type() os.FileMode          { return e.info.Mode().Type() }
func (e memDirEntry) Info() (os.FileInfo, error) { return e.info, nil }

// memFile is an open handle into a MemFS node.
type memFile struct {
	fsys   *MemFS
	node   *memNode
	off    int64
	dirOff int
}

func (f *memFile) Name() string { return f.node.name }
func (f *memFile) Close() error { return nil }
func (f *memFile) Sync() error  { return nil }

func (f *memFile) Stat() (os.FileInfo, error) {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	return f.node.info(), nil
}

func (f *memFile) Read(p []byte) (int, error) {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	if f.node.dir {
		return 0, &fs.PathError{Op: "read", Path: f.node.name, Err: fs.ErrInvalid}
	}
	if f.off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.node.data[f.off:])
	f.off += int64(n)
	return n, nil
}

func (f *memFile) ReadAt(p []byte, off int64) (int, error) {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	if off >= int64(len(f.node.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.node.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// writeAt grows node.data as needed and writes p at off. Caller holds the
// lock.
func (f *memFile) writeAt(p []byte, off int64) int {
	end := off + int64(len(p))
	if end > int64(len(f.node.data)) {
		grown := make([]byte, end)
		copy(grown, f.node.data)
		f.node.data, f.node.shared = grown, false
	} else {
		f.node.own()
	}
	copy(f.node.data[off:], p)
	f.node.modTime = time.Now()
	return len(p)
}

func (f *memFile) Write(p []byte) (int, error) {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	n := f.writeAt(p, f.off)
	f.off += int64(n)
	return n, nil
}

func (f *memFile) WriteAt(p []byte, off int64) (int, error) {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	return f.writeAt(p, off), nil
}

func (f *memFile) Seek(offset int64, whence int) (int64, error) {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
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

func (f *memFile) Truncate(size int64) error {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	if size <= int64(len(f.node.data)) {
		// Shrinking a shared file can keep borrowing: the prefix is
		// unchanged, and a later write still copies first.
		f.node.data = f.node.data[:size]
	} else {
		grown := make([]byte, size)
		copy(grown, f.node.data)
		f.node.data, f.node.shared = grown, false
	}
	f.node.modTime = time.Now()
	return nil
}

func (f *memFile) ReadDir(n int) ([]os.DirEntry, error) {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	if !f.node.dir {
		return nil, &fs.PathError{Op: "readdir", Path: f.node.name, Err: fs.ErrInvalid}
	}
	names := make([]string, 0, len(f.node.children))
	for name := range f.node.children {
		names = append(names, name)
	}
	sort.Strings(names)
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
		out = append(out, memDirEntry{f.node.children[name].info()})
	}
	f.dirOff = end
	return out, nil
}
