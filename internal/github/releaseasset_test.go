package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReleaseAsset(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/showmino/forge/releases/tags/v1.2.3", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"assets":[{"id":7,"name":"checksums.txt"}]}`)
	})
	mux.HandleFunc("GET /repos/showmino/forge/releases/assets/7", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "application/octet-stream" {
			http.Error(w, "want octet-stream", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/signed/checksums.txt", http.StatusFound)
	})
	mux.HandleFunc("GET /signed/checksums.txt", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "abc  forge.tar.gz\n") })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), Token: StaticToken("t")}

	body, err := c.ReleaseAsset(context.Background(), "showmino", "forge", "v1.2.3", "checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()
	if string(data) != "abc  forge.tar.gz\n" {
		t.Errorf("asset %q", data)
	}
	if _, err := c.ReleaseAsset(context.Background(), "showmino", "forge", "v1.2.3", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing asset: %v", err)
	}
	if _, err := c.ReleaseAsset(context.Background(), "showmino", "forge", "v9.9.9", "checksums.txt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing release: %v", err)
	}
}
