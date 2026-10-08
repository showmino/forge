package server

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"testing/fstest"

	"github.com/showmino/forge/internal/template"
)

// Limits for an extracted template repo.
const (
	maxTreeBytes = 100 << 20
	maxTreeFiles = 20000
)

// untar reads a gzipped tarball as GitHub serves it (an optional pax global
// header, then everything under one "owner-repo-sha/" directory) into an
// in-memory tree. Links, special files, names that leave the repo directory
// and oversized content are errors, never skipped.
//
// fstest.MapFS is the standard library's in-memory fs.FS; the API's root
// filesystem is read-only, so the tree cannot go to disk.
func untar(r io.Reader) (fs.FS, error) {
	return untarLimited(r, maxTreeBytes, maxTreeFiles)
}

// untarLimited is untar with explicit whole-tree limits. Entry names count
// toward maxBytes: a tar name can be up to a megabyte long.
func untarLimited(r io.Reader, maxBytes, maxFiles int) (fs.FS, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("read tarball: %w", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := fstest.MapFS{}
	total := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read tarball: %w", err)
		}
		switch h.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeDir:
			continue
		case tar.TypeReg:
		default:
			return nil, fmt.Errorf("tarball entry %s: links and special files are not allowed", h.Name)
		}
		name, ok := inRepo(h.Name)
		if !ok {
			return nil, fmt.Errorf("tarball entry %q is outside the repo directory", h.Name)
		}
		if h.Size > template.MaxBytes {
			return nil, fmt.Errorf("tarball entry %s is larger than %d bytes", name, template.MaxBytes)
		}
		total += int(h.Size) + len(name)
		if total > maxBytes || len(out) == maxFiles {
			return nil, fmt.Errorf("the template repo is larger than %d bytes or %d files", maxBytes, maxFiles)
		}
		data, err := io.ReadAll(io.LimitReader(tr, h.Size))
		if err != nil {
			return nil, fmt.Errorf("read tarball entry %s: %w", name, err)
		}
		mode := fs.FileMode(0o644)
		if h.Mode&0o111 != 0 {
			mode = 0o755
		}
		out[name] = &fstest.MapFile{Data: data, Mode: mode}
	}
}

// inRepo drops the top-level directory GitHub puts around every entry. It
// rejects absolute names, entries outside a directory, and anything that is
// not a clean relative path afterwards.
func inRepo(name string) (string, bool) {
	if path.IsAbs(name) {
		return "", false
	}
	topDir, rest, ok := strings.Cut(name, "/")
	if !ok || topDir == "." || topDir == ".." || rest == "" {
		return "", false
	}
	if !fs.ValidPath(rest) {
		return "", false
	}
	return rest, true
}
