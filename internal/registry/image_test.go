package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const someDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNormalize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"nginx:1.27":                          "docker.io/library/nginx:1.27",
		"nginx":                               "docker.io/library/nginx:latest",
		"grafana/grafana:11.2.0":              "docker.io/grafana/grafana:11.2.0",
		"docker.io/library/redis:7":           "docker.io/library/redis:7",
		"index.docker.io/library/redis:7":     "docker.io/library/redis:7",
		"ghcr.io/acme/tool:2.3":               "ghcr.io/acme/tool:2.3",
		"registry.example:5000/team/app:v1":   "registry.example:5000/team/app:v1",
		"localhost/app:dev":                   "localhost/app:dev",
		"ghcr.io/acme/tool@" + someDigest:     "ghcr.io/acme/tool@" + someDigest,
		"ghcr.io/acme/tool:2.3@" + someDigest: "ghcr.io/acme/tool:2.3@" + someDigest,
	} {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "Nginx:1", "nginx:", "nginx:-x", "ghcr.io/", "ghcr.io/acme/tool@sha256:abc", "https://ghcr.io/acme/tool:1", "a b:1", "nginx:" + strings.Repeat("x", 129)} {
		if got, err := Normalize(bad); err == nil {
			t.Errorf("Normalize(%q) = %q, accepted", bad, got)
		}
	}
}

func TestParseRefWithDigest(t *testing.T) {
	t.Parallel()
	r, err := ParseRef("ghcr.io/acme/tool:2.3@" + someDigest)
	if err != nil || r != (Ref{Host: "ghcr.io", Repo: "acme/tool", Tag: "2.3", Digest: someDigest}) {
		t.Errorf("tag and digest: %+v, %v", r, err)
	}
	r, err = ParseRef("ghcr.io/acme/tool@" + someDigest)
	if err != nil || r != (Ref{Host: "ghcr.io", Repo: "acme/tool", Digest: someDigest}) {
		t.Errorf("digest only: %+v, %v", r, err)
	}
	if r.Reference() != someDigest || (Ref{Tag: "1"}).Reference() != "1" {
		t.Errorf("Reference: %q", r.Reference())
	}
	if got := (Ref{Host: "docker.io", Repo: "library/nginx", Tag: "1.27"}).Pinned(someDigest); got != "docker.io/library/nginx@"+someDigest {
		t.Errorf("Pinned = %q", got)
	}
}

func TestAPIBase(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]string{
		"docker.io":             "https://registry-1.docker.io",
		"ghcr.io":               "https://ghcr.io",
		"registry.example:5000": "https://registry.example:5000",
	} {
		if got := apiBase(host); got != want {
			t.Errorf("apiBase(%q) = %q, want %q", host, got, want)
		}
	}
}

// Docker Hub: a public image needs a token, which anyone gets without a
// login; the manifest answer carries the digest.
func TestDigestOfAPublicImage(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); ok || r.URL.Query().Get("scope") != "repository:library/nginx:pull" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token":"anon"}`))
	})
	mux.HandleFunc("HEAD /v2/library/nginx/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="registry.docker.io"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if ref := r.PathValue("ref"); ref == "1.27" || ref == someDigest {
			w.Header().Set("Docker-Content-Digest", someDigest)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// The install's pull secret has no login for Docker Hub.
	c := &Client{BaseURL: srv.URL, Creds: func(context.Context, string) (string, string, error) {
		return "", "", fmt.Errorf("pull secret has no login for docker.io: %w", ErrNoLogin)
	}}
	ctx := context.Background()
	if d, err := c.Digest(ctx, "docker.io/library/nginx:1.27"); err != nil || d != someDigest {
		t.Errorf("by tag: %q, %v", d, err)
	}
	if d, err := c.Digest(ctx, "docker.io/library/nginx@"+someDigest); err != nil || d != someDigest {
		t.Errorf("by digest: %q, %v", d, err)
	}
	if _, err := c.Digest(ctx, "docker.io/library/nginx:0.0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("absent: %v, want ErrNotFound", err)
	}
	if ok, err := c.Exists(ctx, "docker.io/library/nginx:1.27"); err != nil || !ok {
		t.Errorf("Exists without a login: %v %v", ok, err)
	}
}

// A registry that answers without the digest header is an error, not a
// digest made up from nothing.
func TestDigestMissingHeader(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	if d, err := c.Digest(context.Background(), "registry.example/org/app:1"); err == nil {
		t.Errorf("digest %q without a header", d)
	}
}

// Only "no login for this registry" makes the check anonymous. A pull secret
// that cannot be read is reported, not turned into "not found".
func TestBrokenLoginIsReported(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"token":"anon"}`)) })
	mux.HandleFunc("HEAD /v2/acme/private/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="ghcr.io"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotFound) // what ghcr.io tells an anonymous reader of a private image
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	ctx := context.Background()
	broken := &Client{BaseURL: srv.URL, Creds: func(context.Context, string) (string, string, error) {
		return "", "", errors.New("pull secret forge/ghcr: forbidden")
	}}
	if ok, err := broken.Exists(ctx, "ghcr.io/acme/private:1a2b3c4"); err == nil || ok || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("broken login: %v %v, want the login's error", ok, err)
	}
	// With no login at all, a 404 is a 404.
	none := &Client{BaseURL: srv.URL, Creds: func(context.Context, string) (string, string, error) { return "", "", ErrNoLogin }}
	if ok, err := none.Exists(ctx, "ghcr.io/acme/private:1a2b3c4"); err != nil || ok {
		t.Errorf("no login: %v %v, want not found", ok, err)
	}
}

// An anonymous reader refused outright is told that no login could read it.
func TestAnonymousRefusalNamesTheLogin(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"token":"anon"}`)) })
	mux.HandleFunc("HEAD /v2/acme/private/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="ghcr.io"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	if _, err := c.Digest(context.Background(), "ghcr.io/acme/private:1"); err == nil || !errors.Is(err, ErrNoLogin) {
		t.Errorf("refused: %v, want ErrNoLogin in it", err)
	}
}

func TestDockerHubLoginKey(t *testing.T) {
	t.Parallel()
	data := []byte(`{"auths":{"https://index.docker.io/v1/":{"username":"u","password":"p"}}}`)
	if u, p, err := CredsFromDockerConfig(data, "docker.io"); err != nil || u != "u" || p != "p" {
		t.Errorf("docker login's key: %q %q %v", u, p, err)
	}
	if _, _, err := CredsFromDockerConfig(data, "ghcr.io"); !errors.Is(err, ErrNoLogin) {
		t.Errorf("missing host: %v, want ErrNoLogin", err)
	}
}

func TestNormalizeHostCase(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"Docker.io/nginx:1": "docker.io/library/nginx:1", "GHCR.IO/a/b:1": "ghcr.io/a/b:1"} {
		if got, err := Normalize(in); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"ghcr.io/a?x=1:t", "ghcr.io/../../x:t", "ghcr.io/A/b:1"} {
		if r, err := ParseRef(bad); err == nil {
			t.Errorf("ParseRef(%q) = %+v, accepted", bad, r)
		}
	}
}

// A digest the registry does not confirm is an error.
func TestDigestMustMatch(t *testing.T) {
	t.Parallel()
	other := "sha256:" + strings.Repeat("f", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Docker-Content-Digest", other)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL}
	if d, err := c.Digest(context.Background(), "registry.example/org/app@"+someDigest); err == nil {
		t.Errorf("digest %q accepted for another", d)
	}
}
