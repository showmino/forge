package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// gitServer is the slice of GitHub's git data API RepoFiles uses.
type gitServer struct {
	mu      sync.Mutex
	head    string
	trees   map[string]string // tree sha -> base tree sha (for asserting base_tree)
	commits map[string]map[string]any
	updates []map[string]any
	moved   bool     // answer the next ref update with 422
	removed []string // paths a tree was asked to drop
}

func (g *gitServer) handler() http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /repos/showmino/forge-deploys/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		write(w, map[string]any{"object": map[string]string{"sha": g.head}})
	})
	mux.HandleFunc("GET /repos/showmino/forge-deploys/git/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		write(w, map[string]any{"sha": r.PathValue("sha"), "tree": map[string]string{"sha": "tree-of-" + r.PathValue("sha")}})
	})
	mux.HandleFunc("POST /repos/showmino/forge-deploys/git/trees", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			BaseTree string           `json:"base_tree"`
			Tree     []map[string]any `json:"tree"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		// A removal: the entry names the path with a null sha and no content.
		if len(in.Tree) != 1 {
			http.Error(w, "bad tree", http.StatusBadRequest)
			return
		}
		if sha, has := in.Tree[0]["sha"]; has && sha == nil {
			if _, content := in.Tree[0]["content"]; content || in.Tree[0]["mode"] != "100644" || in.Tree[0]["type"] != "blob" {
				http.Error(w, "bad removal", http.StatusBadRequest)
				return
			}
			g.mu.Lock()
			g.trees["t3"] = in.BaseTree
			g.removed = append(g.removed, in.Tree[0]["path"].(string))
			g.mu.Unlock()
			write(w, map[string]string{"sha": "t3"})
			return
		}
		if len(in.Tree) != 1 || in.Tree[0]["path"] != "services/payments/staging/values.yaml" || in.Tree[0]["content"] != "image: x\n" {
			http.Error(w, "bad tree", http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.trees["t2"] = in.BaseTree
		g.mu.Unlock()
		write(w, map[string]string{"sha": "t2"})
	})
	mux.HandleFunc("POST /repos/showmino/forge-deploys/git/commits", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		g.commits["c2"] = in
		g.mu.Unlock()
		write(w, map[string]string{"sha": "c2"})
	})
	mux.HandleFunc("PATCH /repos/showmino/forge-deploys/git/refs/heads/main", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		g.mu.Lock()
		defer g.mu.Unlock()
		g.updates = append(g.updates, in)
		if g.moved {
			g.moved = false
			http.Error(w, `{"message":"Update is not a fast forward"}`, http.StatusUnprocessableEntity)
			return
		}
		g.head = in["sha"].(string)
		write(w, map[string]any{})
	})
	mux.HandleFunc("GET /repos/showmino/forge-deploys/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "services/payments/staging/values.yaml" || r.URL.Query().Get("per_page") != "2" {
			write(w, []any{})
			return
		}
		write(w, []map[string]any{
			{"sha": "c2", "commit": map[string]any{"message": "deploy b", "committer": map[string]string{"date": "2026-10-09T12:00:00Z"}}},
			{"sha": "c1", "commit": map[string]any{"message": "deploy a", "committer": map[string]string{"date": "2026-10-08T12:00:00Z"}}},
		})
	})
	return mux
}

func TestRepoFiles(t *testing.T) {
	t.Parallel()
	g := &gitServer{head: "c1", trees: map[string]string{}, commits: map[string]map[string]any{}}
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)
	r := &RepoFiles{Client: &Client{BaseURL: srv.URL}, Owner: "showmino", Name: "forge-deploys", Branch: "main"}
	ctx := context.Background()

	commit, tree, err := r.Head(ctx)
	if err != nil || commit != "c1" || tree != "tree-of-c1" {
		t.Fatalf("Head = %q %q %v", commit, tree, err)
	}
	sha, err := r.CommitFile(ctx, commit, tree, "services/payments/staging/values.yaml", []byte("image: x\n"), "deploy payments")
	if err != nil || sha != "c2" {
		t.Fatalf("CommitFile = %q %v", sha, err)
	}
	if g.trees["t2"] != "tree-of-c1" {
		t.Errorf("base_tree %q", g.trees["t2"])
	}
	if c := g.commits["c2"]; c["tree"] != "t2" || c["message"] != "deploy payments" || c["parents"].([]any)[0] != "c1" {
		t.Errorf("commit %v", c)
	}
	if u := g.updates[0]; u["sha"] != "c2" || u["force"] != false {
		t.Errorf("ref update %v (must not force)", u)
	}

	g.mu.Lock()
	g.moved = true
	g.mu.Unlock()
	if _, err := r.CommitFile(ctx, "c2", "tree-of-c2", "services/payments/staging/values.yaml", []byte("image: x\n"), "again"); !errors.Is(err, ErrNotFastForward) {
		t.Errorf("moved branch: %v, want ErrNotFastForward", err)
	}

	hist, err := r.History(ctx, "services/payments/staging/values.yaml", 2)
	if err != nil || len(hist) != 2 || hist[0].SHA != "c2" || hist[0].Message != "deploy b" || hist[0].Time.Day() != 9 {
		t.Errorf("History = %+v %v", hist, err)
	}
}

func TestRepoFilesDeleteFile(t *testing.T) {
	t.Parallel()
	g := &gitServer{head: "c1", trees: map[string]string{}, commits: map[string]map[string]any{}}
	srv := httptest.NewServer(g.handler())
	t.Cleanup(srv.Close)
	r := &RepoFiles{Client: &Client{BaseURL: srv.URL}, Owner: "showmino", Name: "forge-deploys", Branch: "main"}
	ctx := context.Background()

	sha, err := r.DeleteFile(ctx, "c1", "tree-of-c1", "services/payments/production/values.yaml", "remove payments from production")
	if err != nil || sha != "c2" {
		t.Fatalf("DeleteFile = %q %v", sha, err)
	}
	if len(g.removed) != 1 || g.removed[0] != "services/payments/production/values.yaml" || g.trees["t3"] != "tree-of-c1" {
		t.Errorf("removed %v on base %q", g.removed, g.trees["t3"])
	}
	if c := g.commits["c2"]; c["tree"] != "t3" || c["message"] != "remove payments from production" || c["parents"].([]any)[0] != "c1" {
		t.Errorf("commit %v", c)
	}
	if u := g.updates[0]; u["sha"] != "c2" || u["force"] != false {
		t.Errorf("ref update %v (must not force)", u)
	}

	g.mu.Lock()
	g.moved = true
	g.mu.Unlock()
	if _, err := r.DeleteFile(ctx, "c2", "tree-of-c2", "services/payments/staging/values.yaml", "again"); !errors.Is(err, ErrNotFastForward) {
		t.Errorf("moved branch: %v, want ErrNotFastForward", err)
	}
}
