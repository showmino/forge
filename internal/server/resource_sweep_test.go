package server

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
)

// testClock is a clock tests move by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
}

func TestSweepExpiresPlans(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clk.Now })
	ctx := context.Background()
	ready, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	denied, _ := f.plan(t, "writer", "production", database("200Gi", 2), false)
	events := len(allEvents(t, f.store))

	f.res.Sweep(ctx) // nothing is due yet
	if rec, _ := f.store.GetResourcePlan(ctx, ready.GetName()); rec.Plan.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(rec.PlanFile) == 0 {
		t.Fatalf("swept too early: %+v", rec)
	}
	clk.Add(24 * time.Hour)
	f.res.Sweep(ctx)
	rec, _ := f.store.GetResourcePlan(ctx, ready.GetName())
	if rec.Plan.GetState() != forgev1.ResourcePlan_STATE_EXPIRED || len(rec.PlanFile) != 0 || len(rec.PlanJSON) != 0 {
		t.Errorf("an expired plan: %+v", rec)
	}
	if len(rec.Plan.GetChanges()) != 1 || rec.Plan.GetPolicy() == nil {
		t.Error("an expired plan keeps its summary and policy result")
	}
	if rec, _ := f.store.GetResourcePlan(ctx, denied.GetName()); rec.Plan.GetState() != forgev1.ResourcePlan_STATE_DENIED {
		t.Errorf("a denied plan: %v", rec.Plan.GetState())
	}
	if got := len(allEvents(t, f.store)); got != events {
		t.Errorf("expiry wrote %d audit events", got-events)
	}
	_, err := f.startApply(&forgev1.ApplyResourcePlanRequest{Name: ready.GetName()})
	wantCode(t, "an expired plan", err, connect.CodeFailedPrecondition, "expired")
}

func TestSweepEndsDeadRuns(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clk.Now })
	ctx := context.Background()

	// A run whose forge-api died: the record says "applying", nobody watches it.
	tmpl, err := protojson.Marshal(&forgev1.AuditEvent{Kind: "outcome", Action: "resource.apply",
		Actor:   &forgev1.AuditActor{Kind: "user", Subject: "ana", Email: "ana@example.com"},
		Service: "services/payments", Target: "resources/payments/staging", Operation: "operations/op_0123456789ab"})
	if err != nil {
		t.Fatal(err)
	}
	main := &forgev1.ResourceSettings{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}
	dead := &store.PlanRecord{
		Plan: &forgev1.ResourcePlan{
			Name: "resourcePlans/aaaaaaaaaaaa", Service: "services/payments", Env: "staging", State: forgev1.ResourcePlan_STATE_APPLYING,
			Resources: []*forgev1.ResourceSettings{main}, Changes: []*forgev1.ResourceChange{{Action: "create", Resource: "main", Type: "postgres", After: main}},
			Policy: &forgev1.PolicyResult{Rules: "0123456789ab"}, Operation: "operations/op_0123456789ab",
			CreateTime: timestamppb.New(clk.Now()), ExpireTime: timestamppb.New(clk.Now().Add(24 * time.Hour)),
		},
		TokenHash: hashToken("dead-token"), Deadline: clk.Now().Add(15 * time.Minute), Audit: tmpl, PlanFile: []byte("plan"),
	}
	err = f.store.WriteResources(ctx, func(tx store.ResourceTx) error {
		if err := tx.PutResourcePlan(ctx, dead); err != nil {
			return err
		}
		return tx.PutResource(ctx, &forgev1.Resource{Name: "resources/payments/staging/main", Service: "services/payments", Env: "staging",
			Type: "postgres", Settings: main, State: forgev1.Resource_STATE_APPLYING})
	})
	if err != nil {
		t.Fatal(err)
	}

	f.res.Sweep(ctx) // before the deadline: the run may still report
	if rec, _ := f.store.GetResourcePlan(ctx, dead.Plan.GetName()); rec.Plan.GetState() != forgev1.ResourcePlan_STATE_APPLYING {
		t.Fatalf("swept before the deadline: %v", rec.Plan.GetState())
	}
	clk.Add(16 * time.Minute)
	// Past the deadline its token is dead too.
	wantCode(t, "a late complete", f.res.Complete(ctx, "dead-token", run.Result{}), connect.CodeUnauthenticated, "not valid")
	f.res.Sweep(ctx)
	f.res.Sweep(ctx) // a second sweep finds nothing to do

	rec, _ := f.store.GetResourcePlan(ctx, dead.Plan.GetName())
	if rec.Plan.GetState() != forgev1.ResourcePlan_STATE_APPLY_FAILED || rec.Plan.GetMessage() != "the run did not finish before its deadline" ||
		rec.TokenHash != "" || len(rec.PlanFile) != 0 {
		t.Errorf("the dead run's plan: %+v", rec)
	}
	if res := f.getResource(t, "resources/payments/staging/main"); res.GetState() != forgev1.Resource_STATE_FAILED {
		t.Errorf("its resource: %v", res.GetState())
	}
	outs := f.outcomes(t, dead.Plan.GetName())
	if len(outs) != 1 {
		t.Fatalf("%d outcome events, want 1: %v", len(outs), outs)
	}
	if o := outs[0]; o.GetResult().GetCode() != "failed" || o.GetResult().GetMessage() != "run_timeout: the run did not finish before its deadline" ||
		o.GetActor().GetEmail() != "ana@example.com" || o.GetOperation() != "operations/op_0123456789ab" || o.GetAction() != "resource.apply" {
		t.Errorf("outcome: %v", o)
	}
}

func TestSweepLeavesWatchedRuns(t *testing.T) {
	t.Parallel()
	clk := newTestClock()
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clk.Now })
	f.runner.Hang = true
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	clk.Add(time.Hour) // past the run's deadline by the fake clock; its operation still watches it
	f.res.Sweep(context.Background())
	if p := f.getPlan(t, resp.GetPlan().GetName()); p.GetState() != forgev1.ResourcePlan_STATE_PLANNING {
		t.Errorf("the sweep ended a run its operation watches: %v", p.GetState())
	}
	if outs := f.outcomes(t, resp.GetPlan().GetName()); len(outs) != 0 {
		t.Errorf("outcomes: %v", outs)
	}
}

func TestCompleteAfterRestart(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.Hang = true // the runner outlives forge-api; the test plays it
	ctx := context.Background()
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	token := f.runner.Started()[0].Token

	// "Restart": a new API on the same store, with no memory of the run.
	after, err := NewResources(ResourceOptions{Store: f.store, Catalog: f.cat, Operations: NewOperations(nil), Policy: f.res.o.Policy, Launcher: &envtest.Runner{}})
	if err != nil {
		t.Fatal(err)
	}
	after.setAuditor(&auditor{store: f.store, log: slog.New(slog.DiscardHandler)})

	work, err := after.Claim(ctx, token)
	if err != nil || work.Mode != run.ModePlan {
		t.Fatalf("claim after the restart: %+v %v", work, err)
	}
	if err := after.Event(ctx, token, "plan", "still planning"); err != nil {
		t.Errorf("an event with no operation to show it: %v", err)
	}
	planJSON := []byte(`{"resource_changes":[{"address":"kubernetes_manifest.database[\"main\"]","type":"kubernetes_manifest",
		"change":{"actions":["create"],"before":null,"after":{"manifest":{"apiVersion":"postgresql.cnpg.io/v1","kind":"Cluster",
		"metadata":{"name":"main","namespace":"payments-staging"},"spec":{"instances":1,"storage":{"size":"5Gi","storageClass":"longhorn-retain"}}}}}}]}`)
	if err := after.Complete(ctx, token, run.Result{PlanFile: planJSON, PlanJSON: planJSON}); err != nil {
		t.Fatal(err)
	}

	p := f.getPlan(t, resp.GetPlan().GetName())
	if p.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(p.GetChanges()) != 1 {
		t.Fatalf("plan: %v", p)
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 || outs[0].GetResult().GetCode() != "ok" || outs[0].GetActor().GetEmail() != "ana@example.com" ||
		outs[0].GetOperation() != resp.GetOperation().GetName() {
		t.Fatalf("outcomes: %v", outs)
	}

	// The old API's operation, still watching, ends without a second outcome.
	if _, err := f.opc.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
		t.Fatal(err)
	}
	waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if outs := f.outcomes(t, p.GetName()); len(outs) != 1 {
		t.Errorf("%d outcome events after the old operation ended, want 1", len(outs))
	}
	if got := f.getPlan(t, p.GetName()); got.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Errorf("the plan changed: %v", got.GetState())
	}

	// A nil service sweeps nothing and does not panic.
	var off *Resources
	off.Sweep(ctx)
}
