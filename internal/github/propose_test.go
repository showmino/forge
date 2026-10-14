package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLZ is a repo with a main branch, one existing file and pull requests.
type fakeLZ struct {
	mu       sync.Mutex
	refs     map[string]string // branch -> commit
	pulls    map[string]string // head branch -> URL
	failPull bool
	tree     map[string]any // body of the last tree
	commit   map[string]any // body of the last commit
	pull     map[string]any // body of the last pull request
}

func newFakeLZ(t *testing.T) (*fakeLZ, *Client) {
	t.Helper()
	f := &fakeLZ{refs: map[string]string{"main": "c0"}, pulls: map[string]string{}}
	decode := func(r *http.Request, v any) { _ = json.NewDecoder(r.Body).Decode(v) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/lz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"default_branch":"main"}`)
	})
	mux.HandleFunc("GET /repos/showmino/lz/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"object":{"sha":"c0"}}`)
	})
	mux.HandleFunc("GET /repos/showmino/lz/git/commits/c0", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"tree":{"sha":"t0"}}`)
	})
	mux.HandleFunc("GET /repos/showmino/lz/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("path") == "zones/taken.txt" {
			fmt.Fprint(w, `{"content":"","encoding":"base64"}`)
			return
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	mux.HandleFunc("POST /repos/showmino/lz/git/trees", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		decode(r, &f.tree)
		fmt.Fprint(w, `{"sha":"t1"}`)
	})
	mux.HandleFunc("POST /repos/showmino/lz/git/commits", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		decode(r, &f.commit)
		fmt.Fprint(w, `{"sha":"c1"}`)
	})
	mux.HandleFunc("POST /repos/showmino/lz/git/refs", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Ref, SHA string }
		decode(r, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		branch := in.Ref[len("refs/heads/"):]
		if _, ok := f.refs[branch]; ok {
			http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
			return
		}
		f.refs[branch] = in.SHA
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("POST /repos/showmino/lz/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		decode(r, &f.pull)
		if f.failPull {
			http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
			return
		}
		url := fmt.Sprintf("https://github.com/showmino/lz/pull/%d", 3+len(f.pulls))
		f.pulls[f.pull["head"].(string)] = url
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"html_url":%q}`, url)
	})
	mux.HandleFunc("GET /repos/showmino/lz/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if url, ok := f.pulls[strings.TrimPrefix(r.URL.Query().Get("head"), "showmino:")]; ok && r.URL.Query().Get("state") == "open" {
			fmt.Fprintf(w, `[{"html_url":%q}]`, url)
			return
		}
		fmt.Fprint(w, `[]`)
	})
	mux.HandleFunc("DELETE /repos/showmino/lz/git/refs/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.refs, r.PathValue("branch"))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, &Client{BaseURL: srv.URL, HTTP: srv.Client(), Token: StaticToken("t")}
}

func TestProposeFiles(t *testing.T) {
	t.Parallel()
	f, c := newFakeLZ(t)
	p := Proposal{Branch: "forge/lz-a", Title: "Add a", Body: "From Forge.", Message: "Add a", Files: []File{{Path: "zones/a.txt", Content: []byte("a\n")}}}
	url, err := c.ProposeFiles(context.Background(), "showmino", "lz", p)
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/showmino/lz/pull/3" {
		t.Errorf("url %q", url)
	}
	f.mu.Lock()
	if f.tree["base_tree"] != "t0" || f.commit["parents"].([]any)[0] != "c0" || f.refs["forge/lz-a"] != "c1" {
		t.Errorf("tree %v commit %v refs %v", f.tree, f.commit, f.refs)
	}
	if f.pull["head"] != "forge/lz-a" || f.pull["base"] != "main" || f.pull["title"] != "Add a" {
		t.Errorf("pull %v", f.pull)
	}
	f.mu.Unlock()

	// Nothing is written when a file is there already.
	p.Files = []File{{Path: "zones/taken.txt", Content: []byte("x")}}
	if _, err := c.ProposeFiles(context.Background(), "showmino", "lz", p); !errors.Is(err, ErrFileExists) {
		t.Errorf("existing file: %v", err)
	}
	// A retry finds the pull request it opened.
	p.Files = []File{{Path: "zones/a.txt", Content: []byte("a\n")}}
	if url, err := c.ProposeFiles(context.Background(), "showmino", "lz", p); err != nil || url != "https://github.com/showmino/lz/pull/3" {
		t.Errorf("retry: %q %v", url, err)
	}
	// A branch without a pull request is someone else's: refused.
	f.mu.Lock()
	f.refs["forge/stale"] = "cx"
	f.mu.Unlock()
	p.Branch = "forge/stale"
	if _, err := c.ProposeFiles(context.Background(), "showmino", "lz", p); !errors.Is(err, ErrBranchExists) {
		t.Errorf("existing branch: %v", err)
	}
	// A pull request GitHub refuses takes its branch with it.
	f.mu.Lock()
	f.failPull = true
	f.mu.Unlock()
	p.Branch = "forge/lz-b"
	if _, err := c.ProposeFiles(context.Background(), "showmino", "lz", p); err == nil {
		t.Fatal("refused pull request: no error")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.refs["forge/lz-b"]; ok {
		t.Error("the branch of a refused pull request is left behind")
	}
}
