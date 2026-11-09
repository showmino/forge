package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// setEnvs changes payments' environments as its team would.
func (f *deployFixture) setEnvs(envs ...string) error {
	_, err := f.catalogClient.UpdateService(context.Background(), as("writer", &forgev1.UpdateServiceRequest{
		Service:    &forgev1.Service{Name: "services/payments", Environments: envs},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"environments"}},
	}))
	return err
}

func (f *teardownFixture) remove(t *testing.T, env string) (*forgev1.DeleteDeploymentResponse, error) {
	t.Helper()
	resp, err := f.client.DeleteDeployment(context.Background(), as("writer", &forgev1.DeleteDeploymentRequest{Name: "deployments/payments/" + env}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

func refused(t *testing.T, what string, err error, code connect.Code, msg string) {
	t.Helper()
	if connect.CodeOf(err) != code || !strings.Contains(err.Error(), msg) {
		t.Errorf("%s: %v, want %v with %q", what, err, code, msg)
	}
}

// A service with production only: a person deploys to production; staging,
// a promote and a staging rollback are refused, each saying what to do.
func TestServiceWithProductionOnly(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	if err := f.setEnvs("production"); err != nil {
		t.Fatal(err)
	}
	_, err := f.start(t, "staging")
	refused(t, "deploy to staging", err, connect.CodeFailedPrecondition,
		"services/payments has production only, not staging: add it with 'forge service update payments --environments staging,production'")
	f.deployOK(t, "production")
	_, err = f.client.PromoteDeploy(context.Background(), as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	refused(t, "promote", err, connect.CodeFailedPrecondition, "has production only, so there is nothing to promote: deploy to it with 'forge deploy start payments --env production'")
	_, err = f.client.RollbackDeploy(context.Background(), as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"}))
	refused(t, "rollback staging", err, connect.CodeFailedPrecondition, "has production only, not staging")

	// The catalog says so.
	svc, err := f.catalog.get(context.Background(), "services/payments")
	if err != nil || strings.Join(svc.GetEnvironments(), ",") != "production" {
		t.Errorf("catalog %v %v", svc.GetEnvironments(), err)
	}
}

// A direct deploy to production cuts the final version, as a promote does.
func TestProductionOnlyVersion(t *testing.T) {
	t.Parallel()
	f, rel := newReleaseFixture(t)
	if err := f.setEnvs("production"); err != nil {
		t.Fatal(err)
	}
	if op := f.deployRef(t, "production", ""); op.GetError() != nil {
		t.Fatalf("deploy: %v", op.GetError())
	}
	if got := f.versions(t); got != "production=v0.1.0" {
		t.Errorf("versions %s", got)
	}
	if r, ok := rel.Release("v0.1.0"); !ok || r.Prerelease {
		t.Errorf("release %+v %v, want a final one", r, ok)
	}
}

// Changing a service's environments: adding one is free; dropping one that
// still runs something waits for forge deploy remove.
func TestChangeServiceEnvironments(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	f.deployBoth(t)

	refused(t, "unknown", f.setEnvs("qa"), connect.CodeInvalidArgument, `environment "qa" must be staging or production`)
	refused(t, "drop staging", f.setEnvs("production"), connect.CodeFailedPrecondition,
		"services/payments still runs in staging: take it down first with 'forge deploy remove payments --env staging'")

	// Nothing runs in an environment never deployed to.
	if _, err := f.client.DeleteDeployment(context.Background(), as("reader", &forgev1.DeleteDeploymentRequest{Name: "deployments/payments/staging"})); connect.CodeOf(err) != connect.CodeNotFound && connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a reader's remove: %v", err)
	}
	dry, err := f.client.DeleteDeployment(context.Background(), as("writer", &forgev1.DeleteDeploymentRequest{Name: "deployments/payments/staging", ValidateOnly: true}))
	if err != nil || dry.Msg.GetOperation() != nil || dry.Msg.GetDeployment().GetEnv() != "staging" {
		t.Fatalf("dry run: %v %v", dry, err)
	}
	if f.left(t) != everythingButPreviews() {
		t.Errorf("a dry run changed something: %s", f.left(t))
	}
	resp, err := f.remove(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, resp.GetOperation().GetName()); op.GetError() != nil || op.GetKind() != kindRemove {
		t.Fatalf("remove: %v", op)
	}
	if got := f.left(t); got != "values:production app:production ns:production catalog" {
		t.Errorf("after the remove: %s", got)
	}
	_, err = f.remove(t, "staging")
	refused(t, "remove again", err, connect.CodeNotFound, "nothing of services/payments runs in staging")

	if err := f.setEnvs("production"); err != nil {
		t.Fatalf("drop staging after the remove: %v", err)
	}
	if err := f.setEnvs("production", "staging"); err != nil {
		t.Fatalf("add staging back: %v", err)
	}
	svc, _ := f.catalog.get(context.Background(), "services/payments")
	if got := strings.Join(svc.GetEnvironments(), ","); got != "staging,production" {
		t.Errorf("environments %s, want deploy order", got)
	}
}

// everythingButPreviews is what deployBoth leaves: both environments and the
// catalog entry.
func everythingButPreviews() string {
	return "values:staging app:staging ns:staging values:production app:production ns:production catalog"
}

// An infrastructure repo's environments are its GitHub environments and
// cloud access: an update cannot change them.
func TestInfrastructureEnvironmentsAreFixed(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	f.makeCombined(t)
	refused(t, "infra", f.setEnvs("production"), connect.CodeFailedPrecondition, "is an infrastructure repo: its environments are its GitHub environments")
	if err := f.setEnvs("staging", "production"); err != nil {
		t.Errorf("the same environments: %v", err)
	}
}

// A service that runs an image may have staging only: it deploys there and
// is never promoted.
func TestImageServiceWithStagingOnly(t *testing.T) {
	t.Parallel()
	f := newImageFixture(t, nil)
	msgs, err := f.create(t, &forgev1.CreateServiceRequest{ServiceId: "web", Image: "nginx:1.27", OwnerTeam: "billing", Environments: []string{"staging"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(msgs[len(msgs)-1].GetService().GetEnvironments(), ","); got != "staging" {
		t.Fatalf("environments %q", got)
	}
	if op := f.deployImage(t, "web", "staging", ""); op.GetError() != nil {
		t.Fatalf("deploy: %v", op.GetError())
	}
	_, err = f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/web", Env: "production"}))
	refused(t, "production", err, connect.CodeFailedPrecondition, "services/web has staging only, not production")
	_, err = f.client.PromoteDeploy(context.Background(), as("writer", &forgev1.PromoteDeployRequest{Service: "services/web"}))
	refused(t, "promote", err, connect.CodeFailedPrecondition, "add production with 'forge service update web --environments staging,production' to promote")
}
