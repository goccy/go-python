package python

// Config configures a new Python instance's sandbox.

import "io"

// Config configures a new Python instance created by New. The zero value is
// a usable, FULLY SANDBOXED interpreter: a private in-memory filesystem
// pre-loaded with the embedded standard library, an empty environment (the
// host process env is not leaked), sandboxed stdio, and every capability
// hook denying — no outbound connections, no name resolution, no subprocess
// spawns. Each capability is granted explicitly by setting its field.
type Config struct {
	// StdlibDir is the path INSIDE the instance's filesystem (Config.FS)
	// holding the Python standard library (the Lib/ tree); it becomes the
	// sole module search path via py_new. Empty defaults to "/" — right for
	// the library default and for NewStdlibMemFS. An instance on the host
	// filesystem (fs.NewHostFS) points this at a host directory, e.g. the
	// one ExtractStdlib returns.
	StdlibDir string
	// Env is the environment the guest sees (os.environ). nil means an empty
	// environment — the host process os.Environ() is NOT leaked.
	Env []string
	// Dial is the outbound-connection whitelist. It is called before each
	// connect with the network ("tcp"), the host NAME the guest asked for
	// (empty when it connected to a literal address), the resolved
	// dotted-quad IP, and the port; returning false denies the connection.
	// Receiving both the name and the IP lets policy match them jointly, and
	// the connection is made to the exact approved IP (no DNS rebinding).
	// FAIL-CLOSED: a nil Dial denies ALL outbound connections.
	Dial func(network, host, ip string, port int) bool
	// Resolve is the name-resolution whitelist. It is called with the host
	// being resolved before each lookup; returning false denies it.
	// FAIL-CLOSED: a nil Resolve denies ALL lookups (so a hostname
	// connection needs both Resolve and Dial; a literal-IP connection needs
	// only Dial).
	Resolve func(host string) bool
	// Stdin, when non-nil, backs the guest's fd 0 (sys.stdin / input()).
	// Defaults to an empty stream (the host process stdin is NOT used).
	Stdin io.Reader
	// Stdout, when non-nil, receives the guest's fd 1 writes (os-level
	// stdout). Note: print() output during Eval is captured into
	// Result.Stdout by the bridge; Stdout here is the raw fd sink, which
	// RunFile and code run through Call write to directly.
	Stdout io.Writer
	// Stderr, when non-nil, receives the guest's fd 2 writes.
	Stderr io.Writer
	// MaxMemoryBytes, when > 0, caps this instance's wasm linear memory.
	// A guest allocation that would grow memory past this limit fails
	// (memory.grow returns -1 -> the C allocator sees ENOMEM -> Python raises
	// MemoryError) instead of growing the host process unbounded. Rounded
	// down to a multiple of the 64 KiB wasm page size; values below the
	// module's initial memory are ignored.
	MaxMemoryBytes int
	// MemoryReserveBytes, when > 0, is the initial linear-memory slice
	// capacity reserved for this instance. CPython's wasm linear memory
	// grows during boot; reserving capacity up front makes those grows
	// zero-copy reslices, dropping a freshly-booted instance's resident
	// memory. The reservation is virtual address space, not resident memory,
	// so a generous value is cheap. When 0, a default headroom is used.
	MemoryReserveBytes int
	// Exec is the subprocess whitelist. It is called before every process
	// spawn (subprocess.run/Popen via posix_spawn) with the executable path
	// and full argv; returning false denies the spawn (the guest sees a
	// PermissionError). Spawning runs a HOST binary, so a sandbox that allows
	// subprocess should set this to a strict allow-list. FAIL-CLOSED: a nil
	// Exec denies ALL spawns.
	Exec func(path string, argv []string) bool
	// FS, when non-nil, is the filesystem backend this instance sees as its
	// entire guest filesystem ("/"). Every file operation (open/read/write/
	// stat/mkdir/readdir/...) is routed to it, so giving two instances
	// separate FS values isolates them completely — one cannot see another's
	// files.
	//
	// The go-python/fs package provides the backends: fs.NewMemFS (private
	// in-memory), fs.NewHostFS (pass-through to the operating system's
	// filesystem - how the python command behaves), fs.DirFS (the host
	// filesystem scoped to one directory), or any other fs.FS
	// implementation.
	//
	// When nil, the instance gets a PRIVATE in-memory filesystem pre-loaded
	// with the standard library (NewStdlibMemFS), so a library embedding
	// never touches the host disk unless explicitly asked to.
	FS FS
}
