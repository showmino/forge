package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIDownload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "forge-linux-amd64"), []byte("amd64 build"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("not served"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{Authenticator: StaticAuthenticator{}, CLIDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/cli/forge-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "amd64 build" || resp.Header.Get("Content-Type") != "application/octet-stream" {
		t.Errorf("amd64: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}

	for _, path := range []string{
		"/cli/forge-linux-arm64", // allowed name, not built
		"/cli/secret.txt",
		"/cli/..%2fsecret.txt",
		"/cli/../cli/secret.txt",
		"/cli/forge-darwin-amd64",
		"/cli/",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK || string(body) == "not served" {
			t.Errorf("%s: %d %q", path, resp.StatusCode, body)
		}
	}
	resp, err = http.Post(srv.URL+"/cli/forge-linux-amd64", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", resp.StatusCode)
	}
}

func TestCLIDownloadOff(t *testing.T) {
	t.Parallel()
	h, err := New(Options{Authenticator: StaticAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/cli/forge-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("no CLI dir: %d", resp.StatusCode)
	}
}
