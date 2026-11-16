package cli

import (
	"strings"
	"testing"

	"github.com/showmino/forge/internal/exitcode"
)

// Services join an application with 'service update --application'; the
// application is then listed with its parts.
func TestApplication(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if r := h.ok("application", "list"); !strings.Contains(r.err, "No applications") {
		t.Errorf("empty list: %q", r.err)
	}
	if r := h.ok("application", "list", "-o", "json"); !strings.Contains(r.out, `"applications": []`) {
		t.Errorf("empty json: %q", r.out)
	}
	for _, s := range []string{"shop", "shop-api", "shop-worker", "store", "pay-api"} {
		h.ok("service", "register", s, "--team", "billing")
	}
	h.ok("service", "register", "ledger", "--team", "platform")
	h.ok("service", "update", "shop", "--application", "shop")
	h.ok("service", "update", "shop-api", "--application", "shop")
	h.ok("service", "update", "shop-worker", "--application", "shop")
	h.ok("service", "update", "pay-api", "--application", "pay")

	golden(t, "application_list_table", h.ok("application", "list").out)
	golden(t, "application_get_table", h.ok("application", "get", "shop").out)
	golden(t, "application_get_json", h.ok("applications", "get", "applications/shop", "-o", "json").out)
	if r := h.ok("application", "ls", "-o", "name"); r.out != "applications/pay\napplications/shop\n" {
		t.Errorf("names: %q", r.out)
	}
	if r := h.ok("service", "get", "shop-api", "-o", "json"); !strings.Contains(r.out, `"application": "shop"`) {
		t.Errorf("service get: %q", r.out)
	}

	// A reader sees applications.
	h.env["FORGE_TOKEN"] = readerToken
	h.ok("application", "get", "shop")
	h.env["FORGE_TOKEN"] = writerToken

	// Every part has the same team.
	if r := h.fails(int(exitcode.Conflict), "service", "update", "ledger", "--application", "shop"); !strings.Contains(r.err, "every part of an application has the same team") {
		t.Errorf("other team: %q", r.err)
	}
	// An application cannot take the name of a service outside it.
	if r := h.fails(int(exitcode.Conflict), "service", "update", "shop-api", "--application", "store"); !strings.Contains(r.err, "services/store is a service that is not a part of it") {
		t.Errorf("namesake: %q", r.err)
	}
	// A new service cannot take the name of an application it is not in.
	if r := h.fails(int(exitcode.Conflict), "service", "register", "pay", "--team", "billing"); !strings.Contains(r.err, "has the name of the application pay") {
		t.Errorf("register a namesake: %q", r.err)
	}
	if r := h.fails(int(exitcode.Usage), "service", "update", "shop-api", "--application", "Shop!"); !strings.Contains(r.err, "lowercase DNS label") {
		t.Errorf("bad name: %q", r.err)
	}

	// Leaving: the last part gone, the application is gone.
	h.ok("service", "update", "pay-api", "--application", "")
	if r := h.fails(int(exitcode.NotFound), "application", "get", "pay"); !strings.Contains(r.err, "applications/pay not found") {
		t.Errorf("gone: %q", r.err)
	}
}

// 'application get' shows what each part runs in each environment.
func TestApplicationShowsWhatPartsRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveDeploys(map[string]string{})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	h.ok("service", "update", "payments", "--application", "payments")
	h.ok("service", "register", "payments-worker", "--team", "billing")
	h.ok("service", "update", "payments-worker", "--application", "payments")
	h.ok("deploy", "start", "payments", "--env", "staging")

	r := h.ok("application", "get", "payments")
	if !strings.Contains(r.out, "payments          v0.1.0-rc.1") {
		t.Errorf("the part's staging version is missing:\n%s", r.out)
	}
	if !strings.Contains(r.err, "applications/payments, team billing") {
		t.Errorf("heading: %q", r.err)
	}
}

// The parts of an application go to production together, and the
// promotion is recorded as a release.
func TestApplicationPromote(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveDeploys(map[string]string{})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	h.ok("create", "web", "--image", "nginx:1.27", "--port", "80", "--health", "/", "--team", "billing")
	h.ok("service", "update", "payments", "--application", "shop")
	h.ok("service", "update", "web", "--application", "shop")
	h.ok("deploy", "start", "payments", "--env", "staging")

	if r := h.ok("application", "releases", "shop"); !strings.Contains(r.err, "No releases yet") {
		t.Errorf("no releases: %q", r.err)
	}
	h.fails(int(exitcode.Usage), "application", "promote", "shop") // asks for the name; not a terminal
	golden(t, "application_promote_dry_run", h.ok("application", "promote", "shop", "--dry-run").out)

	// A reader may not; a member of the team may.
	h.env["FORGE_TOKEN"] = readerToken
	h.fails(int(exitcode.PermissionDenied), "application", "promote", "shop", "--yes")
	h.env["FORGE_TOKEN"] = teamToken
	r := h.ok("application", "promote", "shop", "--yes")
	for _, want := range []string{"payments: ", "web: ", "applications/shop release 1"} {
		if !strings.Contains(r.err, want) {
			t.Errorf("the promotion's events lack %q:\n%s", want, r.err)
		}
	}
	if !strings.Contains(r.out, "payments   v0.1.0    v0.1.0") || !strings.Contains(r.out, "web        1.27      1.27") {
		t.Errorf("the application after the promotion:\n%s", r.out)
	}
	h.env["FORGE_TOKEN"] = writerToken

	rel := h.ok("application", "releases", "shop")
	if !strings.Contains(rel.out, "payments v0.1.0, web 1.27") || !strings.Contains(rel.out, "cy@example.com") {
		t.Errorf("releases:\n%s", rel.out)
	}
	if r := h.ok("application", "releases", "shop", "-o", "name"); r.out != "applications/shop/releases/1\n" {
		t.Errorf("names: %q", r.out)
	}
	if r := h.fails(int(exitcode.Conflict), "application", "promote", "shop", "--yes"); !strings.Contains(r.err, "nothing to promote") {
		t.Errorf("a second promotion: %q", r.err)
	}
	if r := h.fails(int(exitcode.Conflict), "application", "rollback", "shop", "--yes"); !strings.Contains(r.err, "no earlier release") {
		t.Errorf("a rollback with one release: %q", r.err)
	}
	h.fails(int(exitcode.Usage), "application", "rollback", "shop", "--to", "-1", "--yes")
	h.fails(int(exitcode.NotFound), "application", "rollback", "shop", "--to", "9", "--yes")

	// Both are in the audit log, each once with its outcome.
	if r := h.ok("audit", "list", "--action", "application.promote"); !strings.Contains(r.out, "applications/shop") {
		t.Errorf("audit:\n%s", r.out)
	}
}
