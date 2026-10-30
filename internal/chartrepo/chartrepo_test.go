package chartrepo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

type fakeRegistry map[string]bool

func (f fakeRegistry) Exists(_ context.Context, image string) (bool, error) { return f[image], nil }

func TestHasOCI(t *testing.T) {
	t.Parallel()
	c := &Client{Registry: fakeRegistry{"ghcr.io/stefanprodan/charts/podinfo:6.7.1": true, "ghcr.io/acme/charts/app:1.0.0_build.1": true}}
	ctx := context.Background()
	if err := c.Has(ctx, "oci://ghcr.io/stefanprodan/charts", "podinfo", "6.7.1"); err != nil {
		t.Errorf("podinfo 6.7.1: %v", err)
	}
	if err := c.Has(ctx, "oci://ghcr.io/stefanprodan/charts", "podinfo", "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("podinfo 9.9.9: %v", err)
	}
	// Helm writes '+' as '_' in OCI tags.
	if err := c.Has(ctx, "oci://ghcr.io/acme/charts", "app", "1.0.0+build.1"); err != nil {
		t.Errorf("build metadata: %v", err)
	}
	if err := (&Client{}).Has(ctx, "oci://ghcr.io/x", "a", "1.0.0"); err == nil {
		t.Error("no registry client, no error")
	}
}

func TestHasIndex(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/podinfo/index.yaml":
			_, _ = w.Write([]byte("apiVersion: v1\nentries:\n  podinfo:\n    - version: 6.7.1\n    - version: 6.8.0\n  other:\n    - version: 1.0.0\n"))
		case "/broken/index.yaml":
			_, _ = w.Write([]byte("entries: [\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := &Client{HTTP: srv.Client()}
	ctx := context.Background()
	if err := c.Has(ctx, srv.URL+"/podinfo", "podinfo", "6.8.0"); err != nil {
		t.Errorf("6.8.0: %v", err)
	}
	if err := c.Has(ctx, srv.URL+"/podinfo", "podinfo", "v6.7.1"); err != nil {
		t.Errorf("v6.7.1: %v", err)
	}
	for name, tc := range map[string]struct{ repo, chart, version string }{
		"no such version": {srv.URL + "/podinfo", "podinfo", "7.0.0"},
		"no such chart":   {srv.URL + "/podinfo", "nope", "1.0.0"},
		"no index":        {srv.URL + "/missing", "podinfo", "6.7.1"},
	} {
		if err := c.Has(ctx, tc.repo, tc.chart, tc.version); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
	if err := c.Has(ctx, srv.URL+"/broken", "podinfo", "6.7.1"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("broken index: %v", err)
	}
	if err := c.Has(ctx, "http://charts.example.com", "a", "1.0.0"); err == nil {
		t.Error("http repository accepted")
	}
}

func TestPublicOnly(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"140.82.112.3": true, "2606:4700::1": true,
		"10.0.0.5": false, "192.168.1.2": false, "172.16.0.1": false, "127.0.0.1": false, "169.254.169.254": false,
		"100.64.0.10": false, "::1": false, "fd00::1": false, "fe80::1": false, "0.0.0.0": false, "::ffff:10.0.0.1": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %v, want %v", addr, got, want)
		}
	}
	// The default client refuses to connect to a test server on loopback.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("entries: {}")) }))
	t.Cleanup(srv.Close)
	err := (&Client{}).Has(context.Background(), srv.URL, "a", "1.0.0")
	if err == nil || !strings.Contains(err.Error(), "not a public address") {
		t.Errorf("loopback repository: %v", err)
	}
}

func TestIndexLimits(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big/index.yaml":
			_, _ = w.Write([]byte("entries:\n" + strings.Repeat("#", maxIndex+1)))
		case "/away/index.yaml":
			http.Redirect(w, r, "http://charts.example.com/index.yaml", http.StatusFound)
		case "/secret/index.yaml":
			_, _ = w.Write([]byte("password: hunter2\nentries: [\n"))
		}
	}))
	t.Cleanup(srv.Close)
	base := srv.Client()
	c := &Client{HTTP: &http.Client{Transport: base.Transport, CheckRedirect: (&Client{}).http("").CheckRedirect}}
	ctx := context.Background()
	if err := c.Has(ctx, srv.URL+"/big", "a", "1.0.0"); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Errorf("big index: %v", err)
	}
	if err := c.Has(ctx, srv.URL+"/away", "a", "1.0.0"); err == nil || !strings.Contains(err.Error(), "off https") {
		t.Errorf("redirect to http: %v", err)
	}
	if err := c.Has(ctx, srv.URL+"/secret", "a", "1.0.0"); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("a bad index is quoted: %v", err)
	}
}
