package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/server"
)

// challenger answers every request the way Cloudflare does when it wants a
// browser to solve a challenge first.
func challenger(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cf-Mitigated", "challenge")
		w.Header().Set("Cf-Ray", "a4b71875bb978de0-ARN")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html>Just a moment...</html>"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCloudflareChallenge(t *testing.T) {
	t.Parallel()
	check := func(r result) {
		t.Helper()
		for _, want := range []string{"Cloudflare", "a4b71875bb978de0-ARN", "hint:"} {
			if !strings.Contains(r.err, want) {
				t.Errorf("stderr lacks %q:\n%s", want, r.err)
			}
		}
		if strings.Contains(r.err, "permission denied") || strings.Contains(r.err, "Just a moment") {
			t.Errorf("must not read as Forge's refusal or show the page:\n%s", r.err)
		}
	}

	// At login, on the identity provider.
	h := newHarness(t)
	h.issuer = challenger(t).URL
	h.startAPI(server.StaticAuthenticator{})
	delete(h.env, "FORGE_TOKEN")
	check(h.fails(1, "auth", "login"))

	// On the API.
	h = newHarness(t)
	h.env["FORGE_API_URL"] = challenger(t).URL
	check(h.fails(1, "service", "list"))
}
