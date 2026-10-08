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
	tagsDown atomic.Bool              // the tag list answers 503
	hold     chan struct{}            // tarball downloads of "v0.9.0" wait for it
}

func newFakeRepo(t *testing.T, tags ...string) *fakeRepo {
	t.Helper()
	f := &fakeRepo{hold: make(chan struct{})}
	f.tags.Store(&tags)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/forge-templates/tags", func(w http.ResponseWriter, _ *http.Request) {
		f.tagCalls.Add(1)
		if f.tagsDown.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		var items []string
		for _, tag := range *f.tags.Load() {
			items = append(items, fmt.Sprintf(`{"name":%q}`, tag))
		}
		fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
	})
	mux.HandleFunc("GET /repos/showmino/forge-templates/tarball/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.tarCalls.Add(1)
		if r.PathValue("ref") == "v0.9.0" {
			<-f.hold
		}
		_, _ = w.Write(tarball(t, entry{name: top + "VERSION", typ: tar.TypeReg, mode: 0o644, data: r.PathValue("ref")}))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	t.Cleanup(func() { // runs first: release held handlers so Close can return
		select {
		case <-f.hold:
		default:
			close(f.hold)
		}
	})
	return f
}

// A slow download must not stall requests that the cache can answer.
func TestGitHubSourceSlowDownloadBlocksNoOne(t *testing.T) {
	t.Parallel()
	f := newFakeRepo(t, "v1.0.0", "v0.9.0")
	s := f.source(t)
	if _, err := s.Fetch(context.Background(), "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = s.Fetch(context.Background(), "v0.9.0") }() // hangs in GitHub
	for f.tarCalls.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := s.Versions(ctx); err != nil {
		t.Errorf("cached versions while a download hangs: %v", err)
	}
	if _, err := s.Fetch(ctx, "v1.0.0"); err != nil {
		t.Errorf("cached tree while a download hangs: %v", err)
	}
	// A caller waiting on the hung download gives up with its own context.
	short, cancelShort := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelShort()
	if _, err := s.Fetch(short, "v0.9.0"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting caller: %v", err)
	}
}

// Concurrent requests for the same uncached tag share one download.
func TestGitHubSourceSharesDownloads(t *testing.T) {
	t.Parallel()
	f := newFakeRepo(t, "v0.9.0")
	s := f.source(t)
	errs := make(chan error, 5)
	for range 5 {
		go func() {
			_, err := s.Fetch(context.Background(), "v0.9.0")
			errs <- err
		}()
	}
	for f.tarCalls.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the others queue up
	close(f.hold)
	for range 5 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if n := f.tarCalls.Load(); n != 1 {
		t.Errorf("%d downloads for 5 concurrent requests, want 1", n)
	}
}

// When GitHub is down, the last known tags keep template list working.
func TestGitHubSourceServesStaleTags(t *testing.T) {
	t.Parallel()
	f := newFakeRepo(t, "v1.0.0")
	s := f.source(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	if _, err := s.Versions(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.tagsDown.Store(true)
	now = now.Add(5 * time.Minute)
	vs, err := s.Versions(context.Background())
	if err != nil || len(vs) != 1 || vs[0] != "v1.0.0" {
		t.Errorf("stale tags during an outage: %v %v", vs, err)
	}
	calls := f.tagCalls.Load()
	if _, err := s.Versions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.tagCalls.Load() != calls {
		t.Error("a failed refresh must back off instead of calling GitHub on every request")
	}
	fresh := newFakeRepo(t, "v1.0.0")
	fresh.tagsDown.Store(true)
	if _, err := fresh.source(t).Versions(context.Background()); err == nil {
		t.Error("no tags known and GitHub down: want an error")
	}
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
