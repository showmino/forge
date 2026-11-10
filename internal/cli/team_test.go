package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/exitcode"
	"github.com/showmino/forge/internal/github"
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

// An admin sets a team's on-call rotation; the list shows it.
func TestTeamUpdate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.teams = fakeTeams{{Slug: "billing", Name: "Billing"}}
	h.startAPI(h.authn)

	if r := h.ok("team", "update", "billing", "--oncall", "billing-primary", "--dry-run"); r.out != "teams/billing is on call as billing-primary (dry run, nothing changed)\n" {
		t.Errorf("dry run: %q", r.out)
	}
	if r := h.ok("team", "list"); strings.Contains(r.out, "billing-primary") {
		t.Errorf("a dry run changed the list: %q", r.out)
	}
	if r := h.ok("team", "update", "teams/billing", "--oncall", "billing-primary"); r.out != "teams/billing is on call as billing-primary\n" {
		t.Errorf("update: %q", r.out)
	}
	if r := h.ok("team", "list"); !strings.Contains(r.out, "billing-primary") {
		t.Errorf("list: %q", r.out)
	}
	if r := h.fails(int(exitcode.Usage), "team", "update", "billing"); !strings.Contains(r.err, "nothing to update") {
		t.Errorf("no flag: %q", r.err)
	}
	if r := h.fails(int(exitcode.NotFound), "team", "update", "nobody", "--oncall", "x"); !strings.Contains(r.err, "not a team") {
		t.Errorf("unknown team: %q", r.err)
	}
	if r := h.ok("team", "update", "billing", "--oncall", ""); r.out != "teams/billing has no on-call rotation\n" {
		t.Errorf("clear: %q", r.out)
	}
}
