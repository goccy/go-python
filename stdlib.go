package python

// Self-contained Python standard library.
//
// stdlib.zip is a trimmed CPython Lib/ tree (no test suites, idlelib, tkinter,
// distutils, ...) embedded into this module so an embedding application does
// not have to ship the Lib/ directory alongside its binary. The library
// default serves it from memory (NewStdlibMemFS, what a nil Config.FS gets);
// ExtractStdlib is the host-filesystem alternative: it unpacks the tree once
// per process into a temp directory and returns that path, suitable for
// Config.StdlibDir when Config.FS is a host backend (fs.NewHostFS).

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

var (
	stdlibOnce sync.Once
	stdlibPath string
	stdlibErr  error
)

// ExtractStdlib unpacks the embedded standard library into a temporary
// directory (once per process) and returns its path. The result is cached, so
// repeated calls — e.g. one per Instance — are cheap. The returned directory
// is the value to pass as Config.StdlibDir.
func ExtractStdlib() (string, error) {
	stdlibOnce.Do(func() {
		stdlibPath, stdlibErr = extractStdlibTo("")
	})
	return stdlibPath, stdlibErr
}

// extractStdlibTo unpacks the embedded zip under parent (os.MkdirTemp default
// when empty). Exposed separately so tests can target an explicit location.
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
