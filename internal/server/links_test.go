package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

// run.links gives a part the address of another part of its application,
// in the environment it is deployed or promoted to.
func TestDeployLinks(t *testing.T) {
	t.Parallel()
	linked := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n  application: shop\nrun:\n  port: 9090\n  health: /ready\n  links: [ledger]\n"
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.GitHub.(*envtest.GitHub).Files[envSHA+":forge.yaml"] = []byte(linked)
	})
	ctx := writerCtx()
	join := func(svc, app string) {
		t.Helper()
		if _, err := f.catalog.UpdateService(ctx, connect.NewRequest(&forgev1.UpdateServiceRequest{
			Service:    &forgev1.Service{Name: "services/" + svc, Application: app},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"application"}},
		})); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(want string) {
		t.Helper()
		_, err := f.start(t, "staging")
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), want) {
			t.Fatalf("want a refusal with %q, got %v", want, err)
		}
	}

	// The catalog decides who is a part, not forge.yaml.
	refused("the service is not a part of an application")
	join("payments", "shop")
	refused("services/ledger is not registered")
	if _, err := f.catalog.RegisterService(ctx, connect.NewRequest(&forgev1.RegisterServiceRequest{
		ServiceId: "ledger", Service: &forgev1.Service{OwnerTeam: "billing"},
	})); err != nil {
		t.Fatal(err)
	}
	refused("services/ledger is not a part of the application shop")
	join("ledger", "shop")

	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := f.values(t, "staging").Links; len(got) != 1 || got["LEDGER_URL"] != "http://ledger.ledger-staging.svc" {
		t.Errorf("staging links: %v", got)
	}
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	if got := f.values(t, "production").Links; len(got) != 1 || got["LEDGER_URL"] != "http://ledger.ledger-production.svc" {
		t.Errorf("production links: %v", got)
	}
	dep, err := f.client.GetDeployment(context.Background(), as("writer", &forgev1.GetDeploymentRequest{Name: "deployments/payments/production"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := dep.Msg.GetDeployment().GetLinks(); len(got) != 1 || got[0] != "services/ledger" {
		t.Errorf("the deployment's links: %v", got)
	}

	// A part that left the application cannot be linked to any more.
	join("ledger", "")
	refused("services/ledger is not a part of the application shop")
}
