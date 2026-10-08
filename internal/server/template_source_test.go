package server

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/showmino/forge/internal/github"
)

func TestDirSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := DirSource{Dir: dir}
	vs, err := s.Versions(context.Background())
	if err != nil || len(vs) != 1 || vs[0] != "dev" {
		t.Errorf("versions %v %v", vs, err)
	}
	fsys, err := s.Fetch(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if data, err := fs.ReadFile(fsys, "README.md"); err != nil || string(data) != "hi" {
		t.Errorf("README.md %q %v", data, err)
	}
	if _, err := s.Fetch(context.Background(), "v1.0.0"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("other version: %v", err)
	}
}

type fakeRepo struct {
	srv      *httptest.Server
	tagCalls atomic.Int32
	tarCalls atomic.Int32
	tags     atomic.Pointer[[]string] // replaced by tests while the server runs
}

func newFakeRepo(t *testing.T, tags ...string) *fakeRepo {
	t.Helper()
	f := &fakeRepo{}
	f.tags.Store(&tags)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/forge-templates/tags", func(w http.ResponseWriter, _ *http.Request) {
		f.tagCalls.Add(1)
		var items []string
		for _, tag := range *f.tags.Load() {
			items = append(items, fmt.Sprintf(`{"name":%q}`, tag))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	})
	mux.HandleFunc("GET /repos/showmino/forge-templates/tarball/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.tarCalls.Add(1)
		_, _ = w.Write(tarball(t, entry{name: top + "VERSION", typ: tar.TypeReg, mode: 0o644, data: r.PathValue("ref")}))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRepo) source(t *testing.T) *GitHubSource {
	t.Helper()
	s, err := NewGitHubSource(&github.Client{BaseURL: f.srv.URL}, "showmino/forge-templates")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGitHubSourceVersions(t *testing.T) {
	t.Parallel()
	f := newFakeRepo(t, "v1.9.0", "latest", "v1.10.0", "v2.0.0-rc.1", "release-2", "v0.1.0")
	vs, err := f.source(t).Versions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(vs, ",") != "v1.10.0,v1.9.0,v0.1.0" {
		t.Errorf("versions %v: want semver tags only, newest first", vs)
	}
}

func TestGitHubSourceCaches(t *testing.T) {
	t.Parallel()
	f := newFakeRepo(t, "v1.0.0")
	s := f.source(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	ctx := context.Background()

	for range 3 {
		fsys, err := s.Fetch(ctx, "v1.0.0")
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := fs.ReadFile(fsys, "VERSION"); string(data) != "v1.0.0" {
			t.Errorf("VERSION = %q", data)
		}
	}
	if n := f.tarCalls.Load(); n != 1 {
		t.Errorf("tarball downloaded %d times, want once", n)
	}
	if n := f.tagCalls.Load(); n != 1 {
		t.Errorf("tags listed %d times within a minute, want once", n)
	}

	f.tags.Store(&[]string{"v1.0.0", "v1.1.0"}) // a new release appears
	now = now.Add(2 * time.Minute)
	vs, err := s.Versions(ctx)
	if err != nil || vs[0] != "v1.1.0" {
		t.Errorf("after a minute the new tag must show: %v %v", vs, err)
	}
	if _, err := s.Fetch(ctx, "v9.9.9"); !errors.Is(err, ErrVersionNotFound) {
		t.Errorf("unknown version: %v", err)
	}
}

func TestNewGitHubSourceRepoName(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "forge-templates", "a/b/c", "/x", "x/"} {
		if _, err := NewGitHubSource(&github.Client{}, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
