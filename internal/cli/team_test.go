package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/store"
)

type fakeTeams []github.Team

func (f fakeTeams) ListTeams(context.Context, string) ([]github.Team, error) { return f, nil }

func TestTeamList(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.teams = fakeTeams{
		{Slug: "billing", Name: "Billing", Description: "Payments and the ledger"},
		{Slug: "platform-engineering", Name: "Platform Engineering"},
	}
	h.startAPI(h.authn)

	golden(t, "team_list_table", h.ok("team", "list").out)
	golden(t, "team_list_json", h.ok("team", "list", "-o", "json").out)
	if r := h.ok("teams", "ls", "-o", "name"); r.out != "teams/billing\nteams/platform-engineering\n" {
		t.Errorf("names: %q", r.out)
	}
}

// Without a GitHub App there is nowhere to read teams from, and the command says so.
func TestTeamListWithoutGitHub(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.fails(int(exitcode.Conflict), "team", "list")
	if !strings.Contains(r.err, "no GitHub App") {
		t.Errorf("stderr: %q", r.err)
	}
}

// An admin sets a rule's mode; a rule that guards Forge cannot change.
func TestPolicy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	r := h.ok("policy", "list")
	for _, want := range []string{"images.pinned-version", "enforce", "resources.staging-single", "warn", "promotion.checked-deploy", "always"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("list lacks %q:\n%s", want, r.out)
		}
	}
	if r := h.ok("policy", "set", "images.pinned-version", "--mode", "warn", "--dry-run"); r.out != "rules/images.pinned-version is warn (default enforce) (dry run, nothing changed)\n" {
		t.Errorf("dry run: %q", r.out)
	}
	if r := h.ok("policy", "set", "images.pinned-version", "--mode", "warn"); r.out != "rules/images.pinned-version is warn (default enforce)\n" {
		t.Errorf("set: %q", r.out)
	}
	if r := h.ok("policy", "list"); !strings.Contains(r.out, "warn (default enforce)") {
		t.Errorf("list after set:\n%s", r.out)
	}
	if r := h.ok("policy", "set", "rules/images.pinned-version", "--mode", "enforce"); r.out != "rules/images.pinned-version is enforce\n" {
		t.Errorf("back to the default: %q", r.out)
	}
	if r := h.fails(int(exitcode.Conflict), "policy", "set", "promotion.checked-deploy", "--mode", "off"); !strings.Contains(r.err, "always enforced") {
		t.Errorf("a rule that guards Forge: %q", r.err)
	}
	h.fails(int(exitcode.Usage), "policy", "set", "images.pinned-version", "--mode", "maybe")
	h.fails(int(exitcode.Usage), "policy", "set", "images.pinned-version")
	h.fails(int(exitcode.NotFound), "policy", "set", "nope", "--mode", "off")
}

// How much Forge is used, from the audit log: the harness's writer is an admin.
func TestAuditAdoption(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("service", "register", "payments", "--team", "billing")
	r := h.ok("audit", "adoption")
	for _, want := range []string{"people", "1", "self-served", "0% (0 of 1)"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("adoption lacks %q:\n%s", want, r.out)
		}
	}
	if r := h.ok("audit", "adoption", "--weekly"); strings.Count(r.out, "\n") != 13 {
		t.Errorf("12 weeks and a header:\n%s", r.out)
	}
}

// Make a personal token from a login's session, use it as FORGE_TOKEN within
// its scope, list it and revoke it.
func TestToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	idToken := fakeIDToken("ana", time.Now().Add(time.Hour))
	h.issuer = deviceIDP(t, idToken).URL
	h.sessions = server.NewSessions(server.SessionOptions{Store: store.NewMemory()})
	h.startAPI(server.StaticAuthenticator{idToken: {
		Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}, Client: server.ClientCLI, IssuedAt: time.Now(),
	}})
	delete(h.env, "FORGE_TOKEN")
	h.ok("auth", "login")

	r := h.ok("token", "create", "ci-read", "--expires", "7d")
	secret := strings.TrimSpace(r.out)
	if !strings.HasPrefix(secret, "fgp_") || !strings.Contains(r.err, "shown once") {
		t.Fatalf("create: out %q err %q", r.out, r.err)
	}
	if r := h.ok("token", "list"); !strings.Contains(r.out, "ci-read") || !strings.Contains(r.out, "read") {
		t.Errorf("list:\n%s", r.out)
	}
	h.env["FORGE_TOKEN"] = secret
	h.ok("service", "list")
	if r := h.fails(int(exitcode.PermissionDenied), "service", "register", "payments", "--team", "billing"); !strings.Contains(r.err, "ci-read") {
		t.Errorf("a read token registers: %q", r.err)
	}
	h.fails(int(exitcode.PermissionDenied), "token", "create", "another")
	delete(h.env, "FORGE_TOKEN")

	h.fails(int(exitcode.Usage), "token", "create", "x", "--expires", "500d")
	if r := h.ok("token", "revoke", "ci-read"); r.out != "ci-read revoked\n" {
		t.Errorf("revoke: %q", r.out)
	}
	h.fails(int(exitcode.NotFound), "token", "revoke", "ci-read")
	h.env["FORGE_TOKEN"] = secret
	h.fails(int(exitcode.Unauthenticated), "service", "list")
}
