package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/showmino/forge/internal/envtest"
)

func TestCLIRelease(t *testing.T) {
	t.Parallel()
	rel := envtest.ReleaseAssets{
		"showmino/forge/v1.2.3/checksums.txt":                     []byte("sums"),
		"showmino/forge/v1.2.3/forge_1.2.3_darwin_arm64.tar.gz":   []byte("mac build"),
		"showmino/forge/v1.2.3/forge_1.2.3_windows_amd64.zip":     []byte("windows build"),
		"showmino/forge/v1.2.3/forge_1.2.3_linux_amd64.sbom.json": []byte("not served"),
	}
	h, err := New(Options{Authenticator: StaticAuthenticator{}, CLIReleases: rel, CLIRepo: "showmino/forge"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	for path, want := range map[string]string{
		"/cli/releases/v1.2.3/checksums.txt":                   "sums",
		"/cli/releases/v1.2.3/forge_1.2.3_darwin_arm64.tar.gz": "mac build",
		"/cli/releases/v1.2.3/forge_1.2.3_windows_amd64.zip":   "windows build",
	} {
		if code, body := get(path); code != http.StatusOK || body != want {
			t.Errorf("%s: %d %q", path, code, body)
		}
	}
	for _, path := range []string{
		"/cli/releases/v1.2.3/forge_1.2.3_linux_amd64.sbom.json", // an asset, but not a CLI archive
		"/cli/releases/v1.2.3/forge_1.2.3_linux_amd64.tar.gz",    // allowed, not published
		"/cli/releases/latest/checksums.txt",
		"/cli/releases/v1.2.3/..%2Fsecret",
	} {
		if code, _ := get(path); code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, code)
		}
	}
	// Without a release source nothing is served.
	h, _ = New(Options{Authenticator: StaticAuthenticator{}})
	off := httptest.NewServer(h)
	t.Cleanup(off.Close)
	resp, err := http.Get(off.URL + "/cli/releases/v1.2.3/checksums.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no source: %d", resp.StatusCode)
	}
}
