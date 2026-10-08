package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeOrg is a GitHub org with just enough of the repos and Git Data APIs.
type fakeOrg struct {
	mu       sync.Mutex
	repos    map[string]bool
	blobs    []string         // base64 contents uploaded as blobs
	tree     []map[string]any // entries of the last tree
	commit   map[string]any   // body of the last commit
	ref      map[string]any   // body of the last ref update
	refMiss  atomic.Int32     // answer 409 to this many ref reads first
	refReads atomic.Int32
}

func newFakeOrg(t *testing.T) (*fakeOrg, *Client) {
	t.Helper()
	o := &fakeOrg{repos: map[string]bool{"taken": true}}
	decode := func(r *http.Request, v any) { _ = json.NewDecoder(r.Body).Decode(v) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/{name}", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if !o.repos[r.PathValue("name")] {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("POST /orgs/showmino/repos", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name     string `json:"name"`
			Private  bool   `json:"private"`
			AutoInit bool   `json:"auto_init"`
		}
		decode(r, &in)
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.repos[in.Name] {
			http.Error(w, `{"message":"Repository creation failed.","errors":[{"message":"name already exists on this account"}]}`, http.StatusUnprocessableEntity)
			return
		}
		if !in.Private || !in.AutoInit {
			http.Error(w, "want private and auto_init", http.StatusBadRequest)
			return
		}
		o.repos[in.Name] = true
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"full_name":"showmino/%s","html_url":"https://github.com/showmino/%s","default_branch":"main"}`, in.Name, in.Name)
	})
	mux.HandleFunc("DELETE /repos/showmino/{name}", func(w http.ResponseWriter, r *http.Request) {
		o.mu.Lock()
		defer o.mu.Unlock()
		delete(o.repos, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /repos/showmino/svc/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		o.refReads.Add(1)
		if o.refMiss.Add(-1) >= 0 {
			http.Error(w, `{"message":"Git Repository is empty."}`, http.StatusConflict)
			return
		}
		fmt.Fprint(w, `{"object":{"sha":"init"}}`)
	})
	mux.HandleFunc("POST /repos/showmino/svc/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Content, Encoding string }
		decode(r, &in)
		o.mu.Lock()
		o.blobs = append(o.blobs, in.Content)
		o.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sha":"blob1"}`)
	})
	mux.HandleFunc("POST /repos/showmino/svc/git/trees", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Tree []map[string]any `json:"tree"`
		}
		decode(r, &in)
		o.mu.Lock()
		o.tree = in.Tree
		o.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sha":"tree1"}`)
	})
	mux.HandleFunc("POST /repos/showmino/svc/git/commits", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		decode(r, &in)
		o.mu.Lock()
		o.commit = in
		o.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"sha":"commit1abcdef"}`)
	})
	mux.HandleFunc("PATCH /repos/showmino/svc/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		decode(r, &in)
		o.mu.Lock()
		o.ref = in
		o.mu.Unlock()
		fmt.Fprint(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return o, &Client{BaseURL: srv.URL}
}

func TestCreateAndDeleteRepo(t *testing.T) {
	t.Parallel()
	o, c := newFakeOrg(t)
	ctx := context.Background()
	if ok, err := c.RepoExists(ctx, "showmino", "svc"); err != nil || ok {
		t.Errorf("exists before create: %v %v", ok, err)
	}
	r, err := c.CreateOrgRepo(ctx, "showmino", "svc", "Go service")
	if err != nil || r.DefaultBranch != "main" || r.HTMLURL != "https://github.com/showmino/svc" {
		t.Fatalf("create: %+v %v", r, err)
	}
	if ok, _ := c.RepoExists(ctx, "showmino", "svc"); !ok {
		t.Error("created repo does not exist")
	}
	if _, err := c.CreateOrgRepo(ctx, "showmino", "taken", ""); !errors.Is(err, ErrRepoExists) {
		t.Errorf("taken name: %v", err)
	}
	if err := c.DeleteRepo(ctx, "showmino", "svc"); err != nil {
		t.Fatal(err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.repos["svc"] {
		t.Error("repo still exists after delete")
	}
}

func TestPushFiles(t *testing.T) {
	t.Parallel()
	o, c := newFakeOrg(t)
	o.refMiss.Store(2) // a new repo's branch is not readable at first
	binary := []byte{0xff, 0x00, 0xfe}
	sha, err := c.PushFiles(context.Background(), "showmino", "svc", "main", "Scaffold svc", []File{
		{Path: "README.md", Content: []byte("# svc\n")},
		{Path: "run.sh", Executable: true, Content: []byte("#!/bin/sh\n")},
		{Path: "logo.bin", Content: binary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sha != "commit1abcdef" || o.refReads.Load() != 3 {
		t.Errorf("sha %q after %d ref reads", sha, o.refReads.Load())
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	byPath := map[string]map[string]any{}
	for _, e := range o.tree {
		byPath[e["path"].(string)] = e
	}
	if e := byPath["README.md"]; e["content"] != "# svc\n" || e["mode"] != "100644" {
		t.Errorf("README.md entry: %v", e)
	}
	if e := byPath["run.sh"]; e["mode"] != "100755" {
		t.Errorf("run.sh must be executable: %v", e)
	}
	if e := byPath["logo.bin"]; e["sha"] != "blob1" || e["content"] != nil {
		t.Errorf("logo.bin must be a blob: %v", e)
	}
	if len(o.blobs) != 1 || o.blobs[0] != base64.StdEncoding.EncodeToString(binary) {
		t.Errorf("blobs: %v", o.blobs)
	}
	if parents, _ := o.commit["parents"].([]any); len(parents) != 0 || o.commit["tree"] != "tree1" || o.commit["message"] != "Scaffold svc" {
		t.Errorf("commit: %v", o.commit)
	}
	if o.ref["sha"] != "commit1abcdef" || o.ref["force"] != true {
		t.Errorf("ref update: %v", o.ref)
	}
}
