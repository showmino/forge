package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io/fs"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/template"
)

type entry struct {
	name     string
	typ      byte
	mode     int64
	data     string
	size     int64 // when set, overrides len(data) in the header
	linkname string
}

// tarball builds a gzipped tarball shaped like GitHub's: a pax global header
// first, then everything under one "owner-repo-sha/" directory.
func tarball(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	all := append([]entry{
		{name: "pax_global_header", typ: tar.TypeXGlobalHeader},
		{name: "showmino-forge-templates-abc1234/", typ: tar.TypeDir, mode: 0o755},
	}, entries...)
	for _, e := range all {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.linkname}
		if e.typ == tar.TypeXGlobalHeader {
			h.PAXRecords = map[string]string{"comment": "abc1234"}
		}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.data))
			if e.size > 0 {
				h.Size = e.size
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg && e.size == 0 {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A declared size larger than the data makes Close fail; such tests only
	// need the header to be read, so ignore it.
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

const top = "showmino-forge-templates-abc1234/"

func TestUntar(t *testing.T) {
	t.Parallel()
	fsys, err := untar(bytes.NewReader(tarball(t,
		entry{name: top + "go-grpc/", typ: tar.TypeDir, mode: 0o755},
		entry{name: top + "go-grpc/template.yaml", typ: tar.TypeReg, mode: 0o644, data: "name: go-grpc\n"},
		entry{name: top + "go-grpc/skeleton/run.sh", typ: tar.TypeReg, mode: 0o755, data: "#!/bin/sh\n"},
	)))
	if err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(fsys, "go-grpc/template.yaml")
	if err != nil || string(data) != "name: go-grpc\n" {
		t.Errorf("template.yaml: %q %v", data, err)
	}
	info, err := fs.Stat(fsys, "go-grpc/skeleton/run.sh")
	if err != nil || info.Mode()&0o111 == 0 {
		t.Errorf("run.sh must stay executable: %v %v", info, err)
	}
	if _, err := fs.Stat(fsys, "pax_global_header"); err == nil {
		t.Error("the pax global header must not become a file")
	}
}

func TestUntarRejects(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		e    entry
		want string
	}{
		"symlink":    {entry{name: top + "link", typ: tar.TypeSymlink, linkname: "/etc/passwd"}, "links and special files"},
		"hard link":  {entry{name: top + "hard", typ: tar.TypeLink, linkname: top + "a"}, "links and special files"},
		"traversal":  {entry{name: top + "../escape", typ: tar.TypeReg, data: "x"}, "outside the repo directory"},
		"absolute":   {entry{name: "/etc/x", typ: tar.TypeReg, data: "x"}, "outside the repo directory"},
		"no top dir": {entry{name: "loose.txt", typ: tar.TypeReg, data: "x"}, "outside the repo directory"},
		"too large":  {entry{name: top + "big", typ: tar.TypeReg, size: template.MaxBytes + 1}, "larger than"},
		"fifo":       {entry{name: top + "pipe", typ: tar.TypeFifo}, "links and special files"},
	}
	for name, tt := range tests {
		_, err := untar(bytes.NewReader(tarball(t, tt.e)))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: got %v, want %q", name, err, tt.want)
		}
	}
	if _, err := untar(strings.NewReader("not gzip")); err == nil {
		t.Error("garbage accepted")
	}
}

// The whole-tree caps are the defence against a decompression bomb. Entry
// names count too: a tar entry's name can be up to a megabyte.
func TestUntarTreeLimits(t *testing.T) {
	t.Parallel()
	files := func(n, size int) []entry {
		var es []entry
		for i := range n {
			es = append(es, entry{name: top + "f" + strings.Repeat("x", i), typ: tar.TypeReg, data: strings.Repeat("a", size)})
		}
		return es
	}
	tests := map[string]struct {
		entries         []entry
		maxBytes, files int
	}{
		"too many files":   {files(3, 1), 1 << 20, 2},
		"too many bytes":   {files(3, 10), 25, 100},
		"long names count": {[]entry{{name: top + strings.Repeat("n", 100), typ: tar.TypeReg}}, 50, 100},
	}
	for name, tt := range tests {
		_, err := untarLimited(bytes.NewReader(tarball(t, tt.entries...)), tt.maxBytes, tt.files)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Errorf("%s: got %v", name, err)
		}
	}
	if _, err := untarLimited(bytes.NewReader(tarball(t, files(2, 10)...)), 1<<20, 2); err != nil {
		t.Errorf("exactly at the file limit: %v", err)
	}
}
