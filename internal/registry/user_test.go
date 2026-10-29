package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func digestOf(b string) string {
	s := sha256.Sum256([]byte(b))
	return "sha256:" + hex.EncodeToString(s[:])
}

// A registry with nginx (an index whose linux/amd64 image runs as root) and
// nginx-unprivileged (a single image whose USER is 101), behind a token.
func userRegistry(t *testing.T, badConfig bool) *Client {
	t.Helper()
	rootCfg := `{"architecture":"amd64","config":{"Cmd":["nginx"]}}`
	userCfg := `{"architecture":"amd64","config":{"User":"101"}}`
	armCfg := `{"architecture":"arm64","config":{"User":"999"}}`
	served := map[string]string{digestOf(rootCfg): rootCfg, digestOf(userCfg): userCfg, digestOf(armCfg): armCfg}
	if badConfig {
		served[digestOf(userCfg)] = `{"config":{"User":"0"}}`
	}
	image := func(cfg string) string {
		return `{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + digestOf(cfg) + `"}}`
	}
	amd, arm := image(rootCfg), image(armCfg)
	index := `{"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"digest":"` + digestOf(arm) + `","platform":{"os":"linux","architecture":"arm64"}},` +
		`{"digest":"` + digestOf(amd) + `","platform":{"os":"linux","architecture":"amd64"}}]}`
	manifests := map[string]string{
		"library/nginx/1.27": index, "library/nginx/" + digestOf(index): index,
		"library/nginx/" + digestOf(amd): amd, "library/nginx/" + digestOf(arm): arm,
		"nginxinc/nginx-unprivileged/1.27": image(userCfg), "nginxinc/nginx-unprivileged/" + digestOf(image(userCfg)): image(userCfg),
	}
	var srv *httptest.Server
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer anon" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="registry.docker.io"`)
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"token":"anon"}`)) })
	mux.HandleFunc("/v2/{ns}/{name}/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		body, ok := manifests[r.PathValue("ns")+"/"+r.PathValue("name")+"/"+r.PathValue("ref")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Docker-Content-Digest", digestOf(body))
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(body))
		}
	})
	// Blobs are served from elsewhere, as Docker Hub's are: the redirect is followed.
	mux.HandleFunc("GET /v2/{ns}/{name}/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if authed(w, r) {
			http.Redirect(w, r, "/cdn/"+r.PathValue("digest"), http.StatusTemporaryRedirect)
		}
	})
	mux.HandleFunc("GET /cdn/{digest}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(served[r.PathValue("digest")]))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL}
}

func TestUser(t *testing.T) {
	t.Parallel()
	c := userRegistry(t, false)
	ctx := context.Background()
	// The index's linux/amd64 image, not the first one listed.
	if u, err := c.User(ctx, "docker.io/library/nginx:1.27"); err != nil || u != "" {
		t.Errorf("nginx: %q %v, want no user", u, err)
	}
	if u, err := c.User(ctx, "docker.io/nginxinc/nginx-unprivileged:1.27"); err != nil || u != "101" {
		t.Errorf("nginx-unprivileged: %q %v, want 101", u, err)
	}
	if _, err := c.User(ctx, "docker.io/library/nginx:9"); err == nil {
		t.Error("a missing image has a user")
	}
}

func TestUserChecksTheConfigDigest(t *testing.T) {
	t.Parallel()
	c := userRegistry(t, true)
	if _, err := c.User(context.Background(), "docker.io/nginxinc/nginx-unprivileged:1.27"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a config that is not its digest: %v", err)
	}
}
