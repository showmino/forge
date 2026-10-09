package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// ciOldSHA is a commit the deploy fixture's "release" ref points at and main
// does not.
const ciOldSHA = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"

// waitCIOp polls until the operation is done.
func waitCIOp(t *testing.T, ops *Operations, name string) *forgev1.Operation {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if op, ok := ops.Get(name); ok && op.GetDone() {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not finish", name)
	return nil
}

func TestCIStartDeploy(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()

	// The request's ref is ignored: CI deploys its token's commit.
	resp, err := f.client.StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", Ref: "no-such-ref"}))
	if err != nil {
		t.Fatal(err)
	}
	op := resp.Msg.GetOperation()
	if op.GetCreator() != "github:showmino/payments" {
		t.Errorf("creator %q", op.GetCreator())
	}
	if done := waitCIOp(t, f.ops, op.GetName()); done.GetError() != nil {
		t.Fatalf("deploy failed: %v", done.GetError())
	}
	got, err := f.client.GetDeployment(ctx, as("ci", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if d := got.Msg.GetDeployment(); d.GetCommit() != envSHA || d.GetDeployedBy() != "github:showmino/payments" {
		t.Errorf("deployment: commit %s by %s", d.GetCommit(), d.GetDeployedBy())
	}

	// Another CI caller cannot read it.
	_, err = f.opsClient.GetOperation(ctx, as("ci-billing", &forgev1.GetOperationRequest{Name: op.GetName()}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "operations it started") {
		t.Errorf("GetOperation of another CI caller's operation: %v", err)
	}

	// Its own operation: readable and watchable.
	if _, err := f.opsClient.GetOperation(ctx, as("ci", &forgev1.GetOperationRequest{Name: op.GetName()})); err != nil {
		t.Errorf("GetOperation own: %v", err)
	}
	stream, err := f.opsClient.WatchOperation(ctx, as("ci", &forgev1.WatchOperationRequest{Name: op.GetName()}))
	if err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	if err := stream.Err(); err != nil {
		t.Errorf("WatchOperation own: %v", err)
	}
}

func TestCIScope(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	denied := func(name string, err error, want string) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want PermissionDenied naming %q)", name, err, want)
		}
	}

	_, err := f.client.StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{Service: "services/payments", Env: "production"}))
	denied("production", err, "only deploy services/payments to staging")
	_, err = f.client.StartDeploy(ctx, as("ci-billing", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging"}))
	denied("another service", err, "only deploy services/billing to staging")
	_, err = f.client.StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{Service: "payments", Env: "staging"}))
	denied("malformed service name", err, "only deploy services/payments")

	// A human's deploy to production, then CI tries to read it.
	human, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "production"}))
	if err != nil {
		t.Fatal(err)
	}
	hop := human.Msg.GetOperation().GetName()
	waitCIOp(t, f.ops, hop)
	_, err = f.client.GetDeployment(ctx, as("ci", &forgev1.GetDeploymentRequest{Name: "deployments/payments/production"}))
	denied("GetDeployment production", err, "deployments/payments/staging")
	_, err = f.client.GetDeployment(ctx, as("ci-billing", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	denied("GetDeployment other service", err, "deployments/billing/staging")
	_, err = f.opsClient.GetOperation(ctx, as("ci", &forgev1.GetOperationRequest{Name: hop}))
	denied("GetOperation of a human's operation", err, "operations it started")
	stream, err := f.opsClient.WatchOperation(ctx, as("ci", &forgev1.WatchOperationRequest{Name: hop}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	denied("WatchOperation of a human's operation", err, "operations it started")
	_, err = f.opsClient.GetOperation(ctx, as("ci", &forgev1.GetOperationRequest{Name: "operations/op_000000000000"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown operation: %v", err)
	}

	// The other DeployService RPCs are closed to CI.
	_, err = f.client.ListDeployments(ctx, as("ci", &forgev1.ListDeploymentsRequest{}))
	denied("ListDeployments", err, "CI token")
	_, err = f.client.ListDeployHistory(ctx, as("ci", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/staging"}))
	denied("ListDeployHistory", err, "CI token")
	_, err = f.client.RollbackDeploy(ctx, as("ci", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"}))
	denied("RollbackDeploy", err, "CI token")
	_, err = f.client.PromoteDeploy(ctx, as("ci", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	denied("PromoteDeploy", err, "CI token")
}

func TestRequestIDPerCaller(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	human, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "production", RequestId: "same"}))
	if err != nil {
		t.Fatal(err)
	}
	ci, err := f.client.StartDeploy(ctx, as("ci", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", RequestId: "same"}))
	if err != nil {
		t.Fatal(err)
	}
	if ci.Msg.GetOperation().GetName() == human.Msg.GetOperation().GetName() {
		t.Fatal("CI got the human's operation back for the same request_id")
	}
	if ci.Msg.GetOperation().GetTarget() != "deployments/payments/staging" {
		t.Errorf("CI operation target %q", ci.Msg.GetOperation().GetTarget())
	}
	again, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "production", RequestId: "same"}))
	if err != nil || again.Msg.GetOperation().GetName() != human.Msg.GetOperation().GetName() {
		t.Errorf("the human's retry should return its own operation: %v", err)
	}
	waitCIOp(t, f.ops, human.Msg.GetOperation().GetName())
	waitCIOp(t, f.ops, ci.Msg.GetOperation().GetName())
}

func TestCIDeploysTokenSHA(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	// The request names main, which is at envSHA; the token's commit wins.
	resp, err := f.client.StartDeploy(ctx, as("ci-old", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", Ref: "main"}))
	if err != nil {
		t.Fatal(err)
	}
	if done := waitCIOp(t, f.ops, resp.Msg.GetOperation().GetName()); done.GetError() != nil {
		t.Fatalf("deploy failed: %v", done.GetError())
	}
	got, err := f.client.GetDeployment(ctx, as("ci-old", &forgev1.GetDeploymentRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if d := got.Msg.GetDeployment(); d.GetCommit() != ciOldSHA || d.GetImage() != "ghcr.io/showmino/payments:9f8e7d6" {
		t.Errorf("deployed commit %s image %s", d.GetCommit(), d.GetImage())
	}
}
