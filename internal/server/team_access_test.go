package server

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func TestOwnerTeamGetsTheRepo(t *testing.T) {
	t.Parallel()
	ctx := writerCtx()
	repos := newFakeRepos()
	c := NewCatalog(nil)
	c.ManageRepoAccess(repos, "showmino")
	reg := func(id, team, repo string, validateOnly bool) error {
		_, err := c.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{
			ServiceId: id, ValidateOnly: validateOnly,
			Service: &forgev1.Service{OwnerTeam: team, RepoUrl: repo},
		}))
		return err
	}
	update := func(svc *forgev1.Service, paths ...string) error {
		_, err := c.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{Service: svc, UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}}))
		return err
	}
	teams := func(repo string, want ...string) {
		t.Helper()
		if got := repos.teams(repo); !slices.Equal(got, want) {
			t.Errorf("%s: teams %v, want %v", repo, got, want)
		}
	}

	if err := reg("payments", "billing", "https://github.com/showmino/payments", true); err != nil {
		t.Fatal(err)
	}
	teams("payments") // a dry run changes nothing
	if err := reg("payments", "billing", "https://github.com/showmino/payments", false); err != nil {
		t.Fatal(err)
	}
	teams("payments", "billing=maintain")
	// A repo outside the organization is not Forge's to change.
	if err := reg("outside", "billing", "https://github.com/someone/outside", false); err != nil {
		t.Fatal(err)
	}
	if err := reg("norepo", "billing", "", false); err != nil {
		t.Fatal(err)
	}

	// A change that is not the team or the repo leaves GitHub alone.
	repos.grantErr = errBoom
	if err := update(&forgev1.Service{Name: "services/payments", DisplayName: "Payments"}, "display_name"); err != nil {
		t.Fatal(err)
	}
	// Moving to another team: if the new team cannot get the repo, the move
	// is refused and nothing changes.
	if err := update(&forgev1.Service{Name: "services/payments", OwnerTeam: "ledger"}, "owner_team"); connect.CodeOf(err) != connect.CodeUnavailable || !strings.Contains(err.Error(), "ledger") {
		t.Errorf("failed grant: %v", err)
	}
	if team, _ := c.teamOf(context.Background(), "services/payments"); team != "billing" {
		t.Errorf("the move happened anyway: %s", team)
	}
	teams("payments", "billing=maintain")
	repos.grantErr = nil

	// The new team gets the repo, the old one loses it.
	if err := update(&forgev1.Service{Name: "services/payments", OwnerTeam: "ledger"}, "owner_team"); err != nil {
		t.Fatal(err)
	}
	teams("payments", "ledger=maintain")

	// Another repo: the team gets that one.
	if err := update(&forgev1.Service{Name: "services/payments", RepoUrl: "https://github.com/showmino/payments-v2"}, "repo_url"); err != nil {
		t.Fatal(err)
	}
	teams("payments-v2", "ledger=maintain")

	// A refused register gives nothing.
	repos.grantErr = errBoom
	if err := reg("ledger", "ledger", "https://github.com/showmino/ledger", false); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("register with a failed grant: %v", err)
	}
	if registered(t, c, "services/ledger") {
		t.Error("registered although the team did not get the repo")
	}
}
