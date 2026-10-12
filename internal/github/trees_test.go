package github

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTreeBlobLastChange(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/payments/git/trees/{sha}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sha") != "c1" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("recursive") == "1" {
			_, _ = w.Write([]byte(`{"sha":"t1","truncated":true,"tree":[{"path":"docs","type":"tree","sha":"t2"},{"path":"docs/index.md","type":"blob","sha":"b1","size":12}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"sha":"t1","tree":[{"path":"docs","type":"tree","sha":"t2"}]}`))
	})
	mux.HandleFunc("GET /repos/showmino/payments/git/blobs/b1", func(w http.ResponseWriter, _ *http.Request) {
		enc := base64.StdEncoding.EncodeToString([]byte("# Payments\n"))
		_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + enc[:6] + `\n` + enc[6:] + `"}`))
	})
	mux.HandleFunc("GET /repos/showmino/payments/commits", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("sha") != "main" || q.Get("per_page") != "1" {
			http.Error(w, "bad query", http.StatusBadRequest)
			return
		}
		if q.Get("path") != "docs" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`[{"sha":"c0","commit":{"message":"docs: runbook","committer":{"date":"2026-10-11T09:30:00Z"}}}]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()

	tr, err := c.Tree(ctx, "showmino", "payments", "c1", false)
	if err != nil || len(tr.Entries) != 1 || tr.Entries[0] != (TreeEntry{Path: "docs", Type: "tree", SHA: "t2"}) || tr.Truncated {
		t.Errorf("Tree = %+v, %v", tr, err)
	}
	tr, err = c.Tree(ctx, "showmino", "payments", "c1", true)
	if err != nil || len(tr.Entries) != 2 || tr.Entries[1].Size != 12 || !tr.Truncated {
		t.Errorf("Tree recursive = %+v, %v", tr, err)
	}
	if _, err := c.Tree(ctx, "showmino", "payments", "nope", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("Tree(nope): %v, want ErrNotFound", err)
	}

	data, err := c.Blob(ctx, "showmino", "payments", "b1")
	if err != nil || string(data) != "# Payments\n" {
		t.Errorf("Blob = %q, %v", data, err)
	}

	got, err := c.LastChange(ctx, "showmino", "payments", "main", "docs")
	want := CommitInfo{SHA: "c0", Message: "docs: runbook", Time: time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)}
	if err != nil || got != want {
		t.Errorf("LastChange = %+v, %v", got, err)
	}
	if _, err := c.LastChange(ctx, "showmino", "payments", "main", "nothing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LastChange(nothing): %v, want ErrNotFound", err)
	}
}
