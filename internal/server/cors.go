package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/showmino/forge/pkg/client"
)

// What a browser may send to and read from the API across origins: the
// Connect protocol's headers, the bearer token and Forge's own headers.
var (
	corsAllowHeaders = strings.Join([]string{
		"Authorization", "Content-Type", "Connect-Protocol-Version", "Connect-Timeout-Ms",
		"Connect-Accept-Encoding", "Connect-Content-Encoding",
		client.HeaderClientVersion, client.HeaderRequestID,
	}, ", ")
	corsExposeHeaders = strings.Join([]string{
		client.HeaderServerVersion, client.HeaderMinClientVersion, client.HeaderRequestID,
	}, ", ")
)

// ParseOrigins reads a comma-separated list of browser origins such as
// "http://forge-web.production.example.com". Each must be written as a
// browser sends it, because it is compared as text: a lower-case scheme and
// host, a port only when it is not the scheme's own, and nothing else (no
// path, no wildcard).
func ParseOrigins(list string) ([]string, error) {
	var origins []string
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		u, err := url.Parse(item)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || strings.Contains(u.Host, "*") ||
			u.Scheme+"://"+u.Host != item || item != strings.ToLower(item) || !originPort(u) {
			return nil, fmt.Errorf("invalid origin %q: write it as a browser sends it, such as https://forge.example.com (lower case, no path, no default port)", item)
		}
		origins = append(origins, item)
	}
	return origins, nil
}

// originPort reports whether u has no port, or one a browser would keep in
// its Origin header: a number that is not the scheme's own.
func originPort(u *url.URL) bool {
	if u.Port() == "" {
		return !strings.HasSuffix(u.Host, ":")
	}
	n, err := strconv.Atoi(u.Port())
	if err != nil || n < 1 || n > 65535 {
		return false
	}
	return (u.Scheme != "http" || n != 80) && (u.Scheme != "https" || n != 443)
}

// CORS lets pages served from origins call the API from a browser. Without
// origins it returns next unchanged. It never allows credentials: the
// dashboard sends a bearer token, not a cookie, so a page on another origin
// has nothing to ride on. Requests from other origins are still served, as
// before; the browser keeps the answer from the page that asked.
func CORS(origins []string, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if !allowed[origin] {
			if preflight {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		if preflight {
			h.Set("Access-Control-Allow-Methods", "GET, POST")
			h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			h.Set("Access-Control-Max-Age", "7200")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		next.ServeHTTP(w, r)
	})
}
