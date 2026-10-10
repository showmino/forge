package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/internal/store/pgtest"
)

// brokenStore fails every call, like a database that is down.
type brokenStore struct{}

var errDown = errors.New("database is down")

func (brokenStore) Get(context.Context, string) (*forgev1.Service, int64, error) {
	return nil, 0, errDown
}
func (brokenStore) List(context.Context, store.ListQuery) ([]*forgev1.Service, error) {
	return nil, errDown
}
func (brokenStore) Names(context.Context) ([]string, error)                   { return nil, errDown }
func (brokenStore) ForRepo(context.Context, string, string) ([]string, error) { return nil, errDown }
func (brokenStore) Write(context.Context, func(store.Tx) error) error         { return errDown }
func (brokenStore) Close()                                                    {}

func TestCatalogStoreDown(t *testing.T) {
	t.Parallel()
	c := NewCatalogWithStore(brokenStore{}, nil)
	ctx := writerCtx()
	_, err := c.ListServices(ctx, connect.NewRequest(&forgev1.ListServicesRequest{}))
	assertUnavailable(t, "list", err)
	_, err = c.GetService(ctx, connect.NewRequest(&forgev1.GetServiceRequest{Name: "services/a"}))
	assertUnavailable(t, "get", err)
	_, err = c.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "a", Service: &forgev1.Service{OwnerTeam: "t"}}))
	assertUnavailable(t, "register", err)
	_, err = c.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service: &forgev1.Service{Name: "services/a", OwnerTeam: "u"}, UpdateMask: fieldMask("owner_team"),
	}))
	assertUnavailable(t, "update", err)
	_, err = c.DeleteService(ctx, connect.NewRequest(&forgev1.DeleteServiceRequest{Name: "services/a"}))
	assertUnavailable(t, "delete", err)
	_, err = c.has(ctx, "services/a")
	assertUnavailable(t, "has", err)
	_, err = c.get(ctx, "services/a")
	assertUnavailable(t, "helper get", err)
	_, err = c.ids(ctx)
	assertUnavailable(t, "ids", err)
	_, err = c.servicesForRepo(ctx, "showmino", "a")
	assertUnavailable(t, "servicesForRepo", err)
}

func assertUnavailable(t *testing.T, what string, err error) {
	t.Helper()
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("%s: %v, want Unavailable", what, err)
	}
}

func TestGitHubOIDCStoreDown(t *testing.T) {
	t.Parallel()
	iss := newActionsIssuer(t)
	clk := &ciClock{t: time.Now()}
	a := newCIAuth(t, iss, NewCatalogWithStore(brokenStore{}, nil), &fakeBranches{}, clk)
	_, err := a.Authenticate(context.Background(), iss.sign(t, nil, iss.claims(clk.Now(), nil)))
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("CI login with the catalog down: %v, want Unavailable (not 'not a Forge service')", err)
	}
}

// The catalog's own tests run on the memory store; this runs the rules that
// depend on transactions against PostgreSQL.
func TestCatalogOnPostgres(t *testing.T) {
	t.Parallel()
	pg, err := store.OpenPostgres(context.Background(), pgtest.URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	if err := pg.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := NewCatalogWithStore(pg, nil)
	ctx := writerCtx()
	register(t, c, "ledger")
	pay := register(t, c, "payments", "services/ledger")

	_, err = c.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "payments", Service: &forgev1.Service{OwnerTeam: "t"}}))
	if connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate: %v", err)
	}
	_, err = c.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{ServiceId: "x", Service: &forgev1.Service{OwnerTeam: "t", Dependencies: []string{"services/nope"}}}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("missing dependency: %v", err)
	}
	_, err = c.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service: &forgev1.Service{Name: "services/payments", OwnerTeam: "u", Etag: `W/"9"`}, UpdateMask: fieldMask("owner_team"),
	}))
	if connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("stale etag: %v", err)
	}
	up, err := c.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
		Service: &forgev1.Service{Name: "services/payments", OwnerTeam: "u", Etag: pay.GetEtag()}, UpdateMask: fieldMask("owner_team"),
	}))
	if err != nil || up.Msg.GetService().GetEtag() != `W/"2"` {
		t.Fatalf("update: %v %v", up, err)
	}
	_, err = c.DeleteService(ctx, connect.NewRequest(&forgev1.DeleteServiceRequest{Name: "services/ledger"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("delete a dependency: %v", err)
	}
	for _, id := range []string{"payments", "ledger"} {
		if _, err := c.DeleteService(ctx, connect.NewRequest(&forgev1.DeleteServiceRequest{Name: "services/" + id})); err != nil {
			t.Errorf("delete %s: %v", id, err)
		}
	}
	if _, err := c.get(ctx, "services/payments"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("deleted: %v", err)
	}
}

func fieldMask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }
