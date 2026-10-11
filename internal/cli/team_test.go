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
