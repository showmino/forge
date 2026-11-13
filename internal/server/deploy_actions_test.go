package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
)

const stagingValues = "services/payments/staging/values.yaml"

func (f *deployFixture) values(t *testing.T, env string) deployValues {
	t.Helper()
	v, err := parseValues([]byte(f.read(t, "services/payments/"+env+"/values.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// rewrite changes an environment's values in the deploy repo directly, as
// a deploy with other options would have written them.
func (f *deployFixture) rewrite(t *testing.T, env string, change func(*deployValues)) {
	t.Helper()
	ctx := context.Background()
	v := f.values(t, env)
	change(&v)
	head, tree, err := f.repo.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.CommitFile(ctx, head, tree, "services/payments/"+env+"/values.yaml", v.marshal(), "test"); err != nil {
		t.Fatal(err)
	}
}

func (f *deployFixture) okOp(t *testing.T, op *forgev1.Operation) *forgev1.Operation {
	t.Helper()
	done := wait(t, f.ops, op.GetName())
	if done.GetError() != nil {
		t.Fatalf("%s: %v", op.GetName(), done.GetError())
	}
	return done
}

// A restart marks the pod template, is no deploy in the history, and the
// next deploy leaves the mark out. A promote after a restart ships it.
func TestRestartDeployment(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	name := "deployments/payments/staging"

	_, err := f.client.RestartDeployment(ctx, as("writer", &forgev1.RestartDeploymentRequest{Name: name}))
	refused(t, "restart before a deploy", err, connect.CodeNotFound, "has never been deployed")
	f.deployOK(t, "staging")
	_, err = f.client.RestartDeployment(ctx, as("reader", &forgev1.RestartDeploymentRequest{Name: name}))
	refused(t, "restart by a reader", err, connect.CodePermissionDenied, "")

	dry, err := f.client.RestartDeployment(ctx, as("writer", &forgev1.RestartDeploymentRequest{Name: name, ValidateOnly: true}))
	if err != nil || dry.Msg.GetOperation() != nil || dry.Msg.GetDeployment().GetRestartTime() == nil {
		t.Fatalf("dry run: %v %v", dry, err)
	}
	if f.values(t, "staging").RestartedAt != "" {
		t.Fatal("a dry run changed the values")
	}

	resp, err := f.client.RestartDeployment(ctx, as("writer", &forgev1.RestartDeploymentRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	op := f.okOp(t, resp.Msg.GetOperation())
	if op.GetKind() != kindRestart || eventSteps(op) != "commit,sync" {
		t.Errorf("op %s steps %s", op.GetKind(), eventSteps(op))
	}
	v := f.values(t, "staging")
	if v.RestartedAt == "" || v.Forge.Kind != kindDeploy || v.Replicas != 1 {
		t.Errorf("values after restart: %+v", v)
	}
	hist, _ := f.repo.History(ctx, stagingValues, 1)
	if msg := hist[0].Message; !strings.HasPrefix(msg, "restart payments in staging (ana@example.com)") || strings.Contains(msg, "Forge-") || !strings.Contains(msg, "Operation: "+op.GetName()) {
		t.Errorf("restart commit message %q", msg)
	}
	recs, err := f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: name}))
	if err != nil || len(recs.Msg.GetRecords()) != 1 {
		t.Errorf("history after a restart: %v %v", recs, err)
	}
	got, err := f.client.GetDeployment(ctx, as("reader", &forgev1.GetDeploymentRequest{Name: name}))
	if err != nil || got.Msg.GetDeployment().GetRestartTime() == nil || got.Msg.GetDeployment().GetReplicas() != 1 {
		t.Errorf("deployment after restart: %v %v", got, err)
	}

	// A promote ships what runs in staging, and production gets no mark.
	pr, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	if err != nil {
		t.Fatalf("promote after a restart: %v", err)
	}
	f.okOp(t, pr.Msg.GetOperation())
	if f.values(t, "production").RestartedAt != "" {
		t.Error("the restart mark was promoted")
	}

	// The next deploy leaves the mark out.
	f.deployOK(t, "staging")
	if v := f.values(t, "staging"); v.RestartedAt != "" {
		t.Errorf("a deploy kept the restart mark %q", v.RestartedAt)
	}

	events, err := f.store.ListAuditEvents(ctx, store.AuditQuery{Action: "deploy.restart", Limit: 10})
	if err != nil || len(events) == 0 || events[0].GetTarget() != name {
		t.Errorf("audit: %+v %v", events, err)
	}
}

// A scale lasts until the next deploy; it refuses an autoscaled deployment,
// a chart and a count outside forge.yaml's limits.
func TestScaleDeployment(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	name := "deployments/payments/staging"
	scale := func(who string, n int32) (*forgev1.ScaleDeploymentResponse, error) {
		resp, err := f.client.ScaleDeployment(ctx, as(who, &forgev1.ScaleDeploymentRequest{Name: name, Replicas: n}))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	f.deployOK(t, "staging")

	for _, n := range []int32{0, -1, 11} {
		_, err := scale("writer", n)
		refused(t, "scale out of range", err, connect.CodeInvalidArgument, "must be 1 to 10")
	}
	_, err := scale("reader", 2)
	refused(t, "scale by a reader", err, connect.CodePermissionDenied, "")

	resp, err := scale("writer", 3)
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPreviousReplicas() != 1 || resp.GetDeployment().GetReplicas() != 3 {
		t.Errorf("scale answer %v", resp)
	}
	op := f.okOp(t, resp.GetOperation())
	if op.GetKind() != kindScale {
		t.Errorf("kind %s", op.GetKind())
	}
	if v := f.values(t, "staging"); v.Replicas != 3 || v.Forge.Kind != kindDeploy {
		t.Errorf("values after scale: %+v", v)
	}
	hist, _ := f.repo.History(ctx, stagingValues, 1)
	if msg := hist[0].Message; !strings.HasPrefix(msg, "scale payments in staging to 3 (ana@example.com)") || strings.Contains(msg, "Forge-") {
		t.Errorf("scale commit message %q", msg)
	}
	events, err := f.store.ListAuditEvents(ctx, store.AuditQuery{Action: "deploy.scale", Limit: 10})
	if err != nil || len(events) < 2 || events[0].GetKind() != "outcome" || events[1].GetDetails()["replicas"] != "3" || events[1].GetResult().GetCode() != "ok" {
		t.Errorf("audit: %+v %v", events, err)
	}

	// The next deploy runs forge.yaml's count again.
	f.deployOK(t, "staging")
	if v := f.values(t, "staging"); v.Replicas != 1 {
		t.Errorf("replicas after the next deploy: %d", v.Replicas)
	}

	f.rewrite(t, "staging", func(v *deployValues) { v.Autoscaling = &deployAutoscaling{Min: 2, Max: 5} })
	_, err = scale("writer", 4)
	refused(t, "scale an autoscaled deployment", err, connect.CodeFailedPrecondition, "is autoscaled (2 to 5 pods): change run.autoscaling in forge.yaml")
	got, _ := f.client.GetDeployment(ctx, as("reader", &forgev1.GetDeploymentRequest{Name: name}))
	if !got.Msg.GetDeployment().GetAutoscaled() {
		t.Error("autoscaled not shown")
	}

	f.rewrite(t, "staging", func(v *deployValues) { v.Autoscaling, v.Chart = nil, &deployChart{} })
	_, err = scale("writer", 2)
	refused(t, "scale a chart", err, connect.CodeFailedPrecondition, "runs a chart")
	_, err = f.client.RestartDeployment(ctx, as("writer", &forgev1.RestartDeploymentRequest{Name: name}))
	refused(t, "restart a chart", err, connect.CodeFailedPrecondition, "runs a chart")
}

// A restart waits for a running operation on the same environment.
func TestRestartWhileDeploying(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	f.deployOK(t, "staging")
	f.deploys.mu.Lock()
	f.deploys.running["payments/staging"] = "operations/op_busy"
	f.deploys.mu.Unlock()
	_, err := f.client.RestartDeployment(context.Background(), as("writer", &forgev1.RestartDeploymentRequest{Name: "deployments/payments/staging"}))
	refused(t, "restart while busy", err, connect.CodeFailedPrecondition, "operations/op_busy is already running")
}
