package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/server"
)

func TestServiceAddress(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.publicDomains = []server.PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}, {Name: "deogratias.cloud", Zone: "deogratias-cloud"}}
	h.hostTaken = func(_ context.Context, host string) (bool, error) { return host == "deogratias.cloud", nil }
	h.serveDeploys(map[string]string{})
	h.ok("service", "register", "deogratias", "--team", "mino", "--repo", "https://github.com/showmino/deogratias-cloud")

	r := h.ok("service", "address", "deogratias")
	if !strings.Contains(r.err, "comes from its forge.yaml") || !strings.Contains(r.out, "https://deogratias.showmino.com") {
		t.Errorf("show: %+v", r)
	}

	r = h.ok("service", "address", "deogratias", "--host", "@", "--domain", "deogratias.cloud", "--dry-run")
	golden(t, "service_address_dry_run", r.out)
	if !strings.Contains(r.err, "dry run, nothing changed") {
		t.Errorf("dry run stderr: %q", r.err)
	}

	// No terminal to type the name into: refused without --yes.
	h.fails(int(exitcode.Usage), "service", "address", "deogratias", "--host", "@", "--domain", "deogratias.cloud")
	// The root of deogratias.cloud is in use: it must be claimed.
	if r := h.fails(int(exitcode.Conflict), "service", "address", "deogratias", "--host", "@", "--domain", "deogratias.cloud", "--yes"); !strings.Contains(r.err, "deogratias.cloud is already in use") {
		t.Errorf("unclaimed: %q", r.err)
	}
	h.ok("service", "address", "deogratias", "--host", "@", "--domain", "deogratias.cloud", "--claim", "deogratias.cloud", "--yes")
	if r := h.ok("service", "address", "deogratias"); !strings.Contains(r.err, `host "@", domain "deogratias.cloud"`) {
		t.Errorf("after the change: %q", r.err)
	}
	if r := h.ok("service", "get", "deogratias", "-o", "json"); !strings.Contains(r.out, `"claimed"`) {
		t.Errorf("service get lacks the address:\n%s", r.out)
	}

	h.fails(int(exitcode.Usage), "service", "address", "deogratias", "--claim", "x")
	h.fails(int(exitcode.Usage), "service", "address", "deogratias", "--host", "@", "--yes")
	h.fails(int(exitcode.Usage), "service", "address", "deogratias", "--clear", "--host", "x", "--yes")
	h.ok("service", "address", "deogratias", "--clear", "--yes")
	if r := h.ok("service", "address", "deogratias"); !strings.Contains(r.err, "comes from its forge.yaml") {
		t.Errorf("after clear: %q", r.err)
	}
	// A reader may look, not change.
	h.env["FORGE_TOKEN"] = readerToken
	h.ok("service", "address", "deogratias")
	h.fails(int(exitcode.PermissionDenied), "service", "address", "deogratias", "--host", "shop", "--domain", "showmino.com", "--yes")
}
