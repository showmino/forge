package server

import (
	"context"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
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

// failingPut is a store whose writes fail when they store a service.
type failingPut struct{ store.Store }

func (f failingPut) Write(ctx context.Context, fn func(tx store.Tx) error) error {
	return f.Store.Write(ctx, func(tx store.Tx) error { return fn(failingPutTx{tx}) })
}

type failingPutTx struct{ store.Tx }

func (failingPutTx) Put(context.Context, *forgev1.Service, int64) error { return errBoom }

func TestRepoAccessIsOnlyAdded(t *testing.T) {
	t.Parallel()
	ctx := writerCtx()
	repos := newFakeRepos()
	c := NewCatalog(nil)
	c.ManageRepoAccess(repos, "showmino")
	reg := func(id, team, repo string) {
		t.Helper()
		if _, err := c.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{
			ServiceId: id, Service: &forgev1.Service{OwnerTeam: team, RepoUrl: repo},
		})); err != nil {
			t.Fatal(err)
		}
	}
	move := func(id, team string) {
		t.Helper()
		if _, err := c.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
			Service: &forgev1.Service{Name: "services/" + id, OwnerTeam: team}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"owner_team"}},
		})); err != nil {
			t.Fatal(err)
		}
	}
	teams := func(repo string, want ...string) {
		t.Helper()
		if got := repos.teams(repo); !slices.Equal(got, want) {
			t.Errorf("%s: teams %v, want %v", repo, got, want)
		}
	}

	// A team that is admin of the repo stays admin, also when the service
	// moves away from it.
	repos.give("tools", "platform", "admin")
	reg("tools", "platform", "https://github.com/showmino/tools")
	teams("tools", "platform=admin")
	move("tools", "billing")
	teams("tools", "billing=maintain", "platform=admin")

	// Two services in one repo: moving one keeps the team on the repo.
	reg("api", "web", "https://github.com/showmino/mono")
	reg("site", "web", "https://github.com/showmino/mono")
	move("api", "billing")
	teams("mono", "billing=maintain", "web=maintain")
	move("site", "billing")
	teams("mono", "billing=maintain")
}

func TestRepoAccessUndoneWhenTheWriteFails(t *testing.T) {
	t.Parallel()
	ctx := writerCtx()
	repos := newFakeRepos()
	mem := store.NewMemory()
	good := NewCatalogWithStore(mem, nil)
	good.ManageRepoAccess(repos, "showmino")
	if _, err := good.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/payments"},
	})); err != nil {
		t.Fatal(err)
	}
	repos.give("ledger", "billing", "push")

	bad := NewCatalogWithStore(failingPut{mem}, nil)
	bad.ManageRepoAccess(repos, "showmino")
	if _, err := bad.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "ledger", Service: &forgev1.Service{OwnerTeam: "billing", RepoUrl: "https://github.com/showmino/ledger"},
	})); err == nil {
		t.Fatal("register must fail")
	}
	if got := repos.teams("ledger"); !slices.Equal(got, []string{"billing=push"}) {
		t.Errorf("a failed register must put the old role back: %v", got)
	}
	if _, err := bad.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service: &forgev1.Service{Name: "services/payments", OwnerTeam: "ledger"}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"owner_team"}},
	})); err == nil {
		t.Fatal("update must fail")
	}
	if got := repos.teams("payments"); !slices.Equal(got, []string{"billing=maintain"}) {
		t.Errorf("a failed move must change nothing on GitHub: %v", got)
	}
}
