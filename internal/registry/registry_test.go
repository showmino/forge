package registry

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRef(t *testing.T) {
	t.Parallel()
	r, err := ParseRef("ghcr.io/showmino/payments:1a2b3c4")
	if err != nil || r != (Ref{Host: "ghcr.io", Repo: "showmino/payments", Tag: "1a2b3c4"}) {
		t.Errorf("ParseRef = %+v, %v", r, err)
	}
	for _, bad := range []string{"payments:1", "ghcr.io/showmino/payments", "ghcr.io/:x", ""} {
		if _, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) accepted", bad)
		}
	}
}

func TestCredsFromDockerConfig(t *testing.T) {
	t.Parallel()
	auth := base64.StdEncoding.EncodeToString([]byte("bot:s3cret"))
	data := []byte(`{"auths":{"ghcr.io":{"auth":"` + auth + `"},"docker.io":{"username":"u","password":"p"}}}`)
	if u, p, err := CredsFromDockerConfig(data, "ghcr.io"); err != nil || u != "bot" || p != "s3cret" {
		t.Errorf("auth: %q %q %v", u, p, err)
	}
	if u, p, err := CredsFromDockerConfig(data, "docker.io"); err != nil || u != "u" || p != "p" {
		t.Errorf("username/password: %q %q %v", u, p, err)
	}
	scheme := []byte(`{"auths":{"https://ghcr.io":{"auth":"` + auth + `"}}}`)
	if u, p, err := CredsFromDockerConfig(scheme, "ghcr.io"); err != nil || u != "bot" || p != "s3cret" {
		t.Errorf("https:// key: %q %q %v", u, p, err)
	}
	if _, _, err := CredsFromDockerConfig(data, "quay.io"); err == nil {
		t.Error("missing host accepted")
	}
}

func TestExists(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "bot" || p != "s3cret" || r.URL.Query().Get("scope") != "repository:showmino/payments:pull" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token":"t0k"}`))
	})
	mux.HandleFunc("HEAD /v2/showmino/payments/manifests/{tag}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t0k" || !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.PathValue("tag") == "1a2b3c4" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Creds: func(_ context.Context, host string) (string, string, error) {
		if host != "ghcr.io" {
			t.Errorf("creds for %q", host)
		}
		return "bot", "s3cret", nil
	}}
	ctx := context.Background()
	if ok, err := c.Exists(ctx, "ghcr.io/showmino/payments:1a2b3c4"); err != nil || !ok {
		t.Errorf("present: %v %v", ok, err)
	}
	if ok, err := c.Exists(ctx, "ghcr.io/showmino/payments:0000000"); err != nil || ok {
		t.Errorf("absent: %v %v", ok, err)
	}
	bad := &Client{BaseURL: srv.URL, Creds: func(context.Context, string) (string, string, error) { return "bot", "wrong", nil }}
	if _, err := bad.Exists(ctx, "ghcr.io/showmino/payments:1a2b3c4"); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Errorf("bad creds: %v (must fail without echoing the password)", err)
	}
}
