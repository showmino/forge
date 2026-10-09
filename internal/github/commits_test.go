package github

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveRefFileAtRunsFor(t *testing.T) {
	t.Parallel()
	const sha = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/payments/commits/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("ref") {
		case "main", "feature/x", sha:
			_, _ = w.Write([]byte(`{"sha":"` + sha + `"}`))
		case "deadbeef":
			http.Error(w, `{"message":"No commit found for SHA: deadbeef"}`, http.StatusUnprocessableEntity)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /repos/showmino/payments/contents/forge.yaml", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != sha {
			http.NotFound(w, r)
			return
		}
		enc := base64.StdEncoding.EncodeToString([]byte("run:\n  port: 9090\n"))
		_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + enc[:8] + `\n` + enc[8:] + `"}`))
	})
	mux.HandleFunc("GET /repos/showmino/payments/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("head_sha") != sha {
			_, _ = w.Write([]byte(`{"workflow_runs":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"workflow_runs":[{"name":"ci","status":"completed","conclusion":"failure","html_url":"https://github.com/showmino/payments/actions/runs/1"}]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	ctx := context.Background()

	for _, ref := range []string{"main", "feature/x", sha} {
		got, err := c.ResolveRef(ctx, "showmino", "payments", ref)
		if err != nil || got != sha {
			t.Errorf("ResolveRef(%q) = %q, %v", ref, got, err)
		}
	}
	for _, ref := range []string{"nope", "deadbeef"} {
		if _, err := c.ResolveRef(ctx, "showmino", "payments", ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("ResolveRef(%q): %v, want ErrNotFound", ref, err)
		}
	}

	data, err := c.FileAt(ctx, "showmino", "payments", "forge.yaml", sha)
	if err != nil || string(data) != "run:\n  port: 9090\n" {
		t.Errorf("FileAt = %q, %v", data, err)
	}
	if _, err := c.FileAt(ctx, "showmino", "payments", "forge.yaml", "other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FileAt missing: %v", err)
	}

	runs, err := c.RunsFor(ctx, "showmino", "payments", sha)
	if err != nil || len(runs) != 1 || runs[0].Name != "ci" || runs[0].Conclusion != "failure" || runs[0].HTMLURL == "" {
		t.Errorf("RunsFor = %+v, %v", runs, err)
	}
}
