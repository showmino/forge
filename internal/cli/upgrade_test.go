package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/envtest"
)

// releaseArchive packs a forge binary the way goreleaser does for this OS.
func releaseArchive(t *testing.T, binary string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	name := fmt.Sprintf("forge_0.2.0_%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		zw := zip.NewWriter(&buf)
		w, _ := zw.Create("forge.exe")
		_, _ = w.Write([]byte(binary))
		_ = zw.Close()
		return name + ".zip", buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, data string }{{"README.md", "readme"}, {"forge", binary}} {
		_ = tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data))})
		_, _ = tw.Write([]byte(f.data))
	}
	_ = tw.Close()
	_ = gz.Close()
	return name + ".tar.gz", buf.Bytes()
}

func upgradeHarness(t *testing.T, sums func(name string, data []byte) string) *harness {
	t.Helper()
	h := newHarness(t)
	name, data := releaseArchive(t, "forge 0.2.0 binary")
	h.cliReleases = envtest.ReleaseAssets{
		"showmino/forge/v0.2.0/" + name:       data,
		"showmino/forge/v0.2.0/checksums.txt": []byte(sums(name, data)),
	}
	h.serverVersion = "0.2.0"
	h.startAPI(h.authn)
	h.executable = filepath.Join(t.TempDir(), "forge")
	if err := os.WriteFile(h.executable, []byte("forge 0.1.0 binary"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.executable, 0o750); err != nil {
		t.Fatal(err)
	}
	return h
}

func goodSums(name string, data []byte) string {
	return fmt.Sprintf("%x  other.tar.gz\n%x  %s\n", sha256.Sum256([]byte("x")), sha256.Sum256(data), name)
}

func TestUpgrade(t *testing.T) {
	t.Parallel()
	h := upgradeHarness(t, goodSums)
	binary := func() string { b, _ := os.ReadFile(h.executable); return string(b) }

	r := h.ok("upgrade", "--check")
	if !strings.Contains(r.out, "forge 0.2.0 is available (this is 0.1.0)") || binary() != "forge 0.1.0 binary" {
		t.Errorf("--check:\n%s", r)
	}
	r = h.ok("upgrade")
	if !strings.Contains(r.out, "forge upgraded from 0.1.0 to 0.2.0") || binary() != "forge 0.2.0 binary" {
		t.Errorf("upgrade:\n%s\nbinary %q", r, binary())
	}
	if info, err := os.Stat(h.executable); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("mode not kept: %v %v", info, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(h.executable), ".forge-upgrade-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}

	h.serverVersion = "0.1.0"
	h.startAPI(h.authn)
	if r := h.ok("upgrade"); !strings.Contains(r.out, "forge 0.1.0 is the server's version: nothing to do") {
		t.Errorf("up to date:\n%s", r)
	}

	// An older API does not downgrade this forge by itself.
	h.serverVersion = "0.0.9"
	h.startAPI(h.authn)
	if r := h.fails(2, "upgrade"); !strings.Contains(r.err, "older than this forge") || !strings.Contains(r.err, "--allow-downgrade") {
		t.Errorf("downgrade:\n%s", r)
	}
}

func TestChecksumsURL(t *testing.T) {
	t.Parallel()
	const gh = "https://github.com/showmino/forge/releases/download"
	for _, c := range []struct {
		api, releases, want string
		fails               bool
	}{
		// Over HTTPS, or on this machine, the API's own checksums are trusted.
		{api: "https://forge-api.example.com", releases: gh, want: ""},
		{api: "http://127.0.0.1:8080", releases: gh, want: ""},
		{api: "http://localhost:8080", releases: gh, want: ""},
		// Over plain HTTP they come from the release on GitHub.
		{api: "http://forge.10.150.1.221.nip.io", releases: gh, want: gh + "/v0.2.0/checksums.txt"},
		{api: "http://forge.10.150.1.221.nip.io", releases: "", fails: true},
		{api: "ftp://forge.example.com", releases: gh, fails: true},
	} {
		got, err := checksumsURL(c.api, c.releases, "0.2.0")
		if (err != nil) != c.fails || got != c.want {
			t.Errorf("%s (releases %q): got %q, %v", c.api, c.releases, got, err)
		}
	}
}

// Over plain HTTP the archive still comes from the API, but it must match the
// checksums from GitHub: the API's own list is not used.
func TestUpgradeChecksChecksumsFromGitHub(t *testing.T) {
	t.Parallel()
	name, data := releaseArchive(t, "forge 0.2.0 binary")
	tampered := func(name string, _ []byte) string {
		return fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("tampered")), name)
	}
	for _, c := range []struct {
		api, github func(string, []byte) string
		ok          bool
	}{
		{api: tampered, github: goodSums, ok: true},
		{api: goodSums, github: tampered, ok: false},
	} {
		h := upgradeHarness(t, c.api)
		gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v0.2.0/checksums.txt" {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprint(w, c.github(name, data))
		}))
		t.Cleanup(gh.Close)
		f := newFactory(Deps{IO: IOStreams{Out: io.Discard, ErrOut: io.Discard}})
		bin, err := f.downloadCLI(t.Context(), h.api.URL, "0.2.0", gh.URL+"/v0.2.0/checksums.txt")
		if c.ok && (err != nil || string(bin) != "forge 0.2.0 binary") {
			t.Errorf("GitHub's checksums match: %q, %v", bin, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "checksum")) {
			t.Errorf("GitHub's checksums do not match, want a refusal: %v", err)
		}
	}
}

func TestUpgradeRefusesABadChecksum(t *testing.T) {
	t.Parallel()
	h := upgradeHarness(t, func(name string, _ []byte) string {
		return fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("tampered")), name)
	})
	r := h.fails(1, "upgrade")
	if !strings.Contains(r.err, "checksum") {
		t.Errorf("bad checksum:\n%s", r)
	}
	if b, _ := os.ReadFile(h.executable); string(b) != "forge 0.1.0 binary" {
		t.Errorf("the binary was replaced: %q", b)
	}
}
