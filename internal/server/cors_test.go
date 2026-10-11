package server

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestParseOrigins(t *testing.T) {
	t.Parallel()
	got, err := ParseOrigins(" http://forge-web.staging.10.150.1.222.nip.io, https://forge.example.com:8443 ,")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"http://forge-web.staging.10.150.1.222.nip.io", "https://forge.example.com:8443"}; !slices.Equal(got, want) {
		t.Errorf("origins: %q, want %q", got, want)
	}
	if got, err := ParseOrigins(""); err != nil || got != nil {
		t.Errorf("empty list: %q, %v", got, err)
	}
	for _, bad := range []string{"*", "https://*.example.com", "forge.example.com", "https://forge.example.com/", "https://forge.example.com/app", "ftp://forge.example.com", "https://"} {
		if _, err := ParseOrigins(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestCORS(t *testing.T) {
	t.Parallel()
	const web = "http://forge-web.production.example.com"
	served := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusOK)
	})
	do := func(h http.Handler, method, origin string, preflight bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/forge.v1.ServiceCatalogService/ListServices", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if preflight {
			r.Header.Set("Access-Control-Request-Method", http.MethodPost)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	t.Run("off without origins", func(t *testing.T) {
		res := do(CORS(nil, next), http.MethodPost, web, false)
		if res.Header().Get("Access-Control-Allow-Origin") != "" || res.Header().Get("Vary") != "" {
			t.Errorf("headers: %v", res.Header())
		}
	})

	h := CORS([]string{web}, next)
	t.Run("the dashboard's request", func(t *testing.T) {
		before := served
		res := do(h, http.MethodPost, web, false)
		if res.Code != http.StatusOK || served != before+1 {
			t.Fatalf("status %d, served %d", res.Code, served-before)
		}
		if got := res.Header().Get("Access-Control-Allow-Origin"); got != web {
			t.Errorf("allow origin: %q", got)
		}
		if res.Header().Get("Access-Control-Expose-Headers") == "" || res.Header().Get("Vary") != "Origin" {
			t.Errorf("headers: %v", res.Header())
		}
		if res.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Error("credentials must never be allowed")
		}
	})
	t.Run("the dashboard's preflight", func(t *testing.T) {
		before := served
		res := do(h, http.MethodOptions, web, true)
		if res.Code != http.StatusNoContent || served != before {
			t.Fatalf("status %d, served %d", res.Code, served-before)
		}
		if res.Header().Get("Access-Control-Allow-Origin") != web || res.Header().Get("Access-Control-Allow-Methods") == "" {
			t.Errorf("headers: %v", res.Header())
		}
		for _, name := range []string{"Authorization", "Connect-Protocol-Version", "Content-Type"} {
			if !slices.Contains(splitList(res.Header().Get("Access-Control-Allow-Headers")), name) {
				t.Errorf("%s is not an allowed header", name)
			}
		}
	})
	t.Run("another origin", func(t *testing.T) {
		before := served
		res := do(h, http.MethodPost, "http://evil.example.com", false)
		if res.Code != http.StatusOK || served != before+1 {
			t.Fatalf("status %d: a request without CORS is served as before", res.Code)
		}
		if res.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("headers: %v", res.Header())
		}
		res = do(h, http.MethodOptions, "http://evil.example.com", true)
		if res.Code != http.StatusForbidden || res.Header().Get("Access-Control-Allow-Origin") != "" || served != before+1 {
			t.Errorf("preflight: status %d, headers %v", res.Code, res.Header())
		}
	})
	t.Run("no origin, as the CLI", func(t *testing.T) {
		res := do(h, http.MethodPost, "", false)
		if res.Code != http.StatusOK || res.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("status %d, headers %v", res.Code, res.Header())
		}
	})
}

func splitList(s string) []string { return strings.Split(s, ", ") }
