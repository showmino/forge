package server

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

func (f *deployFixture) deployOK(t *testing.T, env string) {
	t.Helper()
	resp, err := f.start(t, env)
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, resp.GetOperation().GetName()); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
}

func TestRollbackAndPromote(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()

	if _, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("rollback with no deploys: %v", err)
	}
	if _, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("promote from empty staging: %v", err)
	}

	f.deployOK(t, "staging") // record 1
	f.deployOK(t, "staging") // record 2, same image (a redeploy)
	if _, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging", To: "nope"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("rollback to unknown commit: %v", err)
	}
	hist, err := f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/staging"}))
	if err != nil || len(hist.Msg.GetRecords()) != 2 || hist.Msg.GetRecords()[0].GetKind() != "deploy" || hist.Msg.GetRecords()[0].GetActor() != "ana@example.com" {
		t.Fatalf("history: %v %v", hist, err)
	}
	first := hist.Msg.GetRecords()[1].GetDeployCommit()

	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, rb.Msg.GetOperation().GetName()); op.GetError() != nil || eventSteps(op) != "commit,namespace,sync" {
		t.Fatalf("rollback: %v %s", op.GetError(), eventSteps(op))
	}
	hist, _ = f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/staging", Limit: 1}))
	if r := hist.Msg.GetRecords(); len(r) != 1 || r[0].GetKind() != "rollback" {
		t.Errorf("after rollback: %v", r)
	}
	data, _ := f.repo.Read(ctx, valuesPath("payments", "staging"), "")
	v, _ := parseValues(data)
	old, _ := f.repo.Read(ctx, valuesPath("payments", "staging"), first)
	ov, _ := parseValues(old)
	if v.Image != ov.Image || v.Forge.Kind != "rollback" || v.Forge.Commit != ov.Forge.Commit {
		t.Errorf("rollback content %+v vs %+v", v, ov)
	}

	pr, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, pr.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("promote: %v", op.GetError())
	}
	data, _ = f.repo.Read(ctx, valuesPath("payments", "production"), "")
	pv, _ := parseValues(data)
	if pv.Image != v.Image || pv.Env != "production" || pv.Host != "payments.production.10.150.1.222.nip.io" || pv.Forge.Kind != "promote" || pv.Port != 9090 {
		t.Errorf("promoted values %+v", pv)
	}
	for _, req := range []*forgev1.PromoteDeployRequest{
		{Service: "services/payments", From: "production", To: "staging"},
		{Service: "services/payments", From: "staging", To: "staging"},
		{Service: "services/payments", To: "qa"},
	} {
		if _, err := f.client.PromoteDeploy(ctx, as("writer", req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("promote %v: %v", req, err)
		}
	}
	if _, err := f.client.RollbackDeploy(ctx, as("reader", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("reader rollback: %v", err)
	}
	if _, err := f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/production", Limit: 51})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("limit 51: %v", err)
	}
}

func TestStartDeployNeedsWrite(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, true, nil)
	for _, env := range []string{"staging", "production"} {
		_, err := f.client.StartDeploy(context.Background(), as("reader", &forgev1.StartDeployRequest{Service: "services/payments", Env: env}))
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("reader deploy to %s: %v", env, err)
		}
	}
}
