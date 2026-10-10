package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// resourceFixture is a Forge API with resources on a fake runner.
// Tokens: "writer" (ana), "reader" (bo), "ci" (payments' CI), "ci-billing".
type resourceFixture struct {
	res    *Resources
	runner *envtest.Runner
	store  *store.Memory
	ops    *Operations
	cat    *Catalog
	client forgev1connect.ResourceServiceClient
	opc    forgev1connect.OperationServiceClient
}

func newResourceFixture(t *testing.T, tweak func(*ResourceOptions)) *resourceFixture {
	t.Helper()
	st := store.NewMemory()
	f := &resourceFixture{runner: &envtest.Runner{}, store: st, ops: NewOperations(nil), cat: NewCatalogWithStore(st, nil)}
	for _, id := range []string{"payments", "billing"} {
		register(t, f.cat, id)
	}
	pol, err := policy.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	o := ResourceOptions{
		Store: st, Catalog: f.cat, Operations: f.ops, Policy: pol, Launcher: f.runner,
		Poll: 2 * time.Millisecond, Grace: 30 * time.Millisecond,
	}
	if tweak != nil {
		tweak(&o)
	}
	if f.res, err = NewResources(o); err != nil {
		t.Fatal(err)
	}
	f.runner.API = f.res
	t.Cleanup(f.runner.Close)
	h, err := New(Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Email: "ana@example.com", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Email: "bo@example.com", Groups: []string{"dev"}},
			"ci": {Subject: "github:showmino/payments", Permissions: []string{PermDeploy("payments", "staging")},
				Workload: &Workload{Repository: "showmino/payments", Service: "payments", SHA: envSHA}},
			"ci-billing": {Subject: "github:showmino/billing", Permissions: []string{PermDeploy("billing", "staging")},
				Workload: &Workload{Repository: "showmino/billing", Service: "billing", SHA: envSHA}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Catalog:    f.cat, Operations: f.ops, Resources: f.res, Audit: st,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f.client = forgev1connect.NewResourceServiceClient(srv.Client(), srv.URL)
	f.opc = forgev1connect.NewOperationServiceClient(srv.Client(), srv.URL)
	return f
}

// database declares one Postgres database.
func database(storage string, instances int32) []*forgev1.ResourceDeclaration {
	return []*forgev1.ResourceDeclaration{{Name: "main", Type: "postgres", Storage: storage, Instances: instances}}
}

// startPlan asks for a plan and returns the response.
func (f *resourceFixture) startPlan(t *testing.T, token, service, env string, decls []*forgev1.ResourceDeclaration, allowDestroy bool) *forgev1.PlanResourcesResponse {
	t.Helper()
	resp, err := f.client.PlanResources(context.Background(), as(token, &forgev1.PlanResourcesRequest{
		Service: service, Env: env, Resources: decls, AllowDestroy: allowDestroy,
	}))
	if err != nil {
		t.Fatalf("plan %s (%s): %v", service, env, err)
	}
	return resp.Msg
}

// plan asks for a plan, waits for its operation, and returns both as they ended.
func (f *resourceFixture) plan(t *testing.T, token, env string, decls []*forgev1.ResourceDeclaration, allowDestroy bool) (*forgev1.ResourcePlan, *forgev1.Operation) {
	t.Helper()
	resp := f.startPlan(t, token, "services/payments", env, decls, allowDestroy)
	op := waitCIOp(t, f.ops, resp.GetOperation().GetName())
	return f.getPlan(t, resp.GetPlan().GetName()), op
}

func (f *resourceFixture) getPlan(t *testing.T, name string) *forgev1.ResourcePlan {
	t.Helper()
	got, err := f.client.GetResourcePlan(context.Background(), as("writer", &forgev1.GetResourcePlanRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	return got.Msg.GetPlan()
}

// outcomes returns the outcome events about plan, oldest first.
func (f *resourceFixture) outcomes(t *testing.T, plan string) []*forgev1.AuditEvent {
	t.Helper()
	var out []*forgev1.AuditEvent
	for _, e := range allEvents(t, f.store) {
		if e.GetKind() == "outcome" && e.GetDetails()["plan"] == plan {
			out = append(out, e)
		}
	}
	return out
}

func wantCode(t *testing.T, what string, err error, code connect.Code, contains string) {
	t.Helper()
	if connect.CodeOf(err) != code || err == nil || !strings.Contains(err.Error(), contains) {
		t.Errorf("%s: %v, want %s containing %q", what, err, code, contains)
	}
}

func TestPlanResources(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)

	if op.GetError() != nil || op.GetKind() != "resource.plan" || op.GetTarget() != "resources/payments/staging" || op.GetCreator() != "ana@example.com" {
		t.Fatalf("operation: %v", op)
	}
	if steps := eventSteps(op); !strings.Contains(steps, "launch") || !strings.Contains(steps, "init") || !strings.Contains(steps, "policy") {
		t.Errorf("events: %s", steps)
	}
	if p.GetState() != forgev1.ResourcePlan_STATE_PLANNED || p.GetService() != "services/payments" || p.GetEnv() != "staging" ||
		p.GetCreator() != "ana@example.com" || p.GetOperation() != op.GetName() || !strings.HasPrefix(p.GetName(), "resourcePlans/") {
		t.Errorf("plan: %v", p)
	}
	if rs := p.GetResources(); len(rs) != 1 || rs[0].GetStorageGi() != 5 || rs[0].GetInstances() != 1 {
		t.Errorf("settings: %v", rs)
	}
	if cs := p.GetChanges(); len(cs) != 1 || cs[0].GetAction() != "create" || cs[0].GetResource() != "main" || cs[0].GetAfter().GetStorageGi() != 5 {
		t.Errorf("changes: %v", cs)
	}
	if pol := p.GetPolicy(); pol == nil || pol.GetDenied() || len(pol.GetRules()) != 12 {
		t.Errorf("policy: %v", pol)
	}
	if got := p.GetExpireTime().AsTime().Sub(p.GetCreateTime().AsTime()); got != 24*time.Hour {
		t.Errorf("expires after %s, want 24h", got)
	}
	if len(f.runner.Databases()) != 0 {
		t.Errorf("a plan must change nothing: %v", f.runner.Databases())
	}

	// The audit log has the request and one outcome, by the same person.
	var req *forgev1.AuditEvent
	for _, e := range allEvents(t, f.store) {
		if e.GetKind() == "request" && e.GetAction() == "resource.plan" {
			req = e
		}
	}
	if req == nil || req.GetActor().GetEmail() != "ana@example.com" || req.GetService() != "services/payments" ||
		req.GetTarget() != "resources/payments/staging" || req.GetOperation() != op.GetName() ||
		req.GetDetails()["environment"] != "staging" || req.GetDetails()["plan"] != p.GetName() || req.GetResult().GetCode() != "ok" {
		t.Errorf("request event: %v", req)
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 {
		t.Fatalf("%d outcome events, want 1: %v", len(outs), outs)
	}
	out := outs[0]
	if out.GetAction() != "resource.plan" || out.GetActor().GetEmail() != "ana@example.com" || out.GetService() != "services/payments" ||
		out.GetTarget() != "resources/payments/staging" || out.GetOperation() != op.GetName() || out.GetResult().GetCode() != "ok" {
		t.Errorf("outcome event: %v", out)
	}
	if d := out.GetDetails(); d["environment"] != "staging" || d["policy"] != "passed" || d["policy_rules"] != p.GetPolicy().GetRules() ||
		d["changes"] != "create=1 update=0 delete=0" || d["allow_destroy"] != "" {
		t.Errorf("outcome details: %v", d)
	}

	// The record keeps the plan files; the API has no field to show them.
	rec, err := f.store.GetResourcePlan(context.Background(), p.GetName())
	if err != nil || len(rec.PlanFile) == 0 || len(rec.PlanJSON) == 0 || rec.TokenHash != "" || !rec.Deadline.IsZero() || rec.CreatorSubject != "ana" {
		t.Errorf("record: %+v %v", rec, err)
	}
}

func TestPlanDenied(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	p, op := f.plan(t, "writer", "production", database("200Gi", 2), false)
	if op.GetError().GetCode() != "policy_denied" || !strings.Contains(op.GetError().GetMessage(), "storage 200Gi is above the 50Gi limit for production") {
		t.Errorf("operation error: %v", op.GetError())
	}
	if p.GetState() != forgev1.ResourcePlan_STATE_DENIED || !p.GetPolicy().GetDenied() ||
		!slices.Equal(p.GetPolicy().GetDeny(), []string{"storage 200Gi is above the 50Gi limit for production"}) {
		t.Errorf("plan: %v", p)
	}
	rec, _ := f.store.GetResourcePlan(context.Background(), p.GetName())
	if len(rec.PlanFile) != 0 || len(rec.PlanJSON) != 0 {
		t.Error("a denied plan keeps no plan file")
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 || outs[0].GetResult().GetCode() != "failed" || outs[0].GetDetails()["policy"] != "denied" ||
		!strings.HasPrefix(outs[0].GetResult().GetMessage(), "policy_denied: storage 200Gi") {
		t.Errorf("outcome: %v", outs)
	}

	// A warning does not block.
	w, op := f.plan(t, "writer", "staging", database("5Gi", 2), false)
	if op.GetError() != nil || w.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(w.GetPolicy().GetWarn()) != 1 {
		t.Errorf("warned plan: %v %v", w, op.GetError())
	}
}

func TestPlanAccess(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	ctx := context.Background()
	req := func(service, env string) *forgev1.PlanResourcesRequest {
		return &forgev1.PlanResourcesRequest{Service: service, Env: env, Resources: database("5Gi", 0)}
	}

	_, err := f.client.PlanResources(ctx, as("reader", req("services/payments", "staging")))
	wantCode(t, "a reader", err, connect.CodePermissionDenied, "services.write")
	_, err = f.client.PlanResources(ctx, connect.NewRequest(req("services/payments", "staging")))
	wantCode(t, "no token", err, connect.CodeUnauthenticated, "")

	// CI plans its own service, in either environment, and never with allow_destroy.
	for _, env := range []string{"staging", "production"} {
		resp := f.startPlan(t, "ci", "services/payments", env, database("5Gi", 2), false)
		if op := waitCIOp(t, f.ops, resp.GetOperation().GetName()); op.GetCreator() != "github:showmino/payments" {
			t.Errorf("%s: creator %q", env, op.GetCreator())
		}
		// It may read its own plan and follow its own operation.
		if _, err := f.client.GetResourcePlan(ctx, as("ci", &forgev1.GetResourcePlanRequest{Name: resp.GetPlan().GetName()})); err != nil {
			t.Errorf("CI reads its plan: %v", err)
		}
		if _, err := f.opc.GetOperation(ctx, as("ci", &forgev1.GetOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
			t.Errorf("CI reads its operation: %v", err)
		}
		_, err := f.client.GetResourcePlan(ctx, as("ci-billing", &forgev1.GetResourcePlanRequest{Name: resp.GetPlan().GetName()}))
		wantCode(t, "another repo's CI reads the plan", err, connect.CodePermissionDenied, "only read the plans of services/billing")
	}
	_, err = f.client.PlanResources(ctx, as("ci-billing", req("services/payments", "staging")))
	wantCode(t, "another repo's CI", err, connect.CodePermissionDenied, "only plan the resources of services/billing")
	destroy := req("services/payments", "staging")
	destroy.AllowDestroy = true
	_, err = f.client.PlanResources(ctx, as("ci", destroy))
	wantCode(t, "CI with allow_destroy", err, connect.CodePermissionDenied, "not plan with allow_destroy")
	for _, call := range []func() error{
		func() error {
			_, err := f.client.ListResourcePlans(ctx, as("ci", &forgev1.ListResourcePlansRequest{}))
			return err
		},
		func() error {
			_, err := f.client.ApplyResourcePlan(ctx, as("ci", &forgev1.ApplyResourcePlanRequest{Name: "resourcePlans/0123456789ab"}))
			return err
		},
		func() error {
			_, err := f.client.ListResources(ctx, as("ci", &forgev1.ListResourcesRequest{}))
			return err
		},
	} {
		wantCode(t, "CI on another call", call(), connect.CodePermissionDenied, "CI token")
	}

	_, err = f.client.PlanResources(ctx, as("writer", req("services/nope", "staging")))
	wantCode(t, "an unknown service", err, connect.CodeNotFound, "services/nope")
	_, err = f.client.PlanResources(ctx, as("writer", req("services/payments", "qa")))
	wantCode(t, "an unknown environment", err, connect.CodeInvalidArgument, "staging or production")
	bad := req("services/payments", "staging")
	bad.Resources = database("5GB", 0)
	_, err = f.client.PlanResources(ctx, as("writer", bad))
	wantCode(t, "a bad size", err, connect.CodeInvalidArgument, `resources: main: storage "5GB"`)
	_, err = f.client.GetResourcePlan(ctx, as("writer", &forgev1.GetResourcePlanRequest{Name: "plans/x"}))
	wantCode(t, "a bad plan name", err, connect.CodeInvalidArgument, "resourcePlans/<id>")
	_, err = f.client.GetResourcePlan(ctx, as("writer", &forgev1.GetResourcePlanRequest{Name: "resourcePlans/0123456789ab"}))
	wantCode(t, "a missing plan", err, connect.CodeNotFound, "resourcePlans/0123456789ab")

	// Resources off: every call says so.
	var off *Resources
	_, err = off.PlanResources(writerCtx(), connect.NewRequest(req("services/payments", "staging")))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured on this Forge API")
	_, err = off.GetResourcePlan(writerCtx(), connect.NewRequest(&forgev1.GetResourcePlanRequest{}))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured")
	_, err = off.ListResourcePlans(writerCtx(), connect.NewRequest(&forgev1.ListResourcePlansRequest{}))
	wantCode(t, "resources off", err, connect.CodeFailedPrecondition, "resources are not configured")
}

func TestPlanOneAtATimeAndRequestID(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.Hang = true
	ctx := context.Background()
	req := &forgev1.PlanResourcesRequest{Service: "services/payments", Env: "staging", Resources: database("5Gi", 0), RequestId: "req-1"}
	first, err := f.client.PlanResources(ctx, as("writer", req))
	if err != nil {
		t.Fatal(err)
	}
	// The same request id returns the same operation and plan.
	again, err := f.client.PlanResources(ctx, as("writer", req))
	if err != nil || again.Msg.GetOperation().GetName() != first.Msg.GetOperation().GetName() || again.Msg.GetPlan().GetName() != first.Msg.GetPlan().GetName() {
		t.Errorf("replay: %v %v", again, err)
	}
	// Another plan for the same target is refused and names the running operation.
	other := &forgev1.PlanResourcesRequest{Service: "services/payments", Env: "staging", Resources: database("6Gi", 0)}
	_, err = f.client.PlanResources(ctx, as("writer", other))
	wantCode(t, "a second plan", err, connect.CodeFailedPrecondition, first.Msg.GetOperation().GetName()+" is already running for resources/payments/staging")
	// Another environment is free.
	prod := f.startPlan(t, "writer", "services/payments", "production", database("5Gi", 2), false)
	if prod.GetPlan().GetName() == first.Msg.GetPlan().GetName() {
		t.Error("production got staging's plan")
	}
}

func TestPlanCancel(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.Hang = true
	ctx := context.Background()
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	// Wait until the runner started, then cancel.
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := f.opc.CancelOperation(ctx, as("writer", &forgev1.CancelOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
		t.Fatal(err)
	}
	op := waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if op.GetError().GetCode() != "cancelled" {
		t.Errorf("operation: %v", op.GetError())
	}
	p := f.getPlan(t, resp.GetPlan().GetName())
	if p.GetState() != forgev1.ResourcePlan_STATE_FAILED || p.GetMessage() != "the run was cancelled" {
		t.Errorf("plan: %v", p)
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 || outs[0].GetResult().GetCode() != "cancelled" {
		t.Errorf("outcomes: %v", outs)
	}
	// The runner was stopped and the target is free again.
	stopped := false
	for end := time.Now().Add(5 * time.Second); !stopped && time.Now().Before(end); time.Sleep(time.Millisecond) {
		st, err := f.runner.Status(ctx, p.GetName(), run.ModePlan)
		stopped = err == nil && st.Done
	}
	if !stopped {
		t.Error("the runner was not stopped")
	}
	f.runner.Hang = false
	if p2, op2 := f.plan(t, "writer", "staging", database("5Gi", 0), false); op2.GetError() != nil || p2.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Errorf("a plan after the cancel: %v %v", p2, op2.GetError())
	}
}

func TestPlanFailures(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		set     func(*envtest.Runner)
		tweak   func(*ResourceOptions)
		code    string
		message string
	}{
		"OpenTofu fails":             {func(r *envtest.Runner) { r.FailPlan = "tofu: boom" }, nil, "run_failed", "tofu: boom"},
		"the runner crashes":         {func(r *envtest.Runner) { r.Crash = "OOMKilled" }, nil, "runner_failed", "the runner failed: OOMKilled"},
		"the runner says nothing":    {func(r *envtest.Runner) { r.Silent = true }, nil, "runner_failed", "the runner ended without a result"},
		"the runner cannot start":    {func(r *envtest.Runner) { r.StartErr = context.DeadlineExceeded }, nil, "launch_failed", "the runner could not be started"},
		"the run takes too long":     {func(r *envtest.Runner) { r.Hang = true }, func(o *ResourceOptions) { o.RunTimeout = 40 * time.Millisecond }, "run_timeout", "the run did not finish within 40ms"},
		"control characters removed": {func(r *envtest.Runner) { r.FailPlan = "bad\x1b[31m thing\x00" }, nil, "run_failed", "bad�[31m thing"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newResourceFixture(t, tc.tweak)
			tc.set(f.runner)
			p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
			if op.GetError().GetCode() != tc.code || !strings.Contains(op.GetError().GetMessage(), tc.message) {
				t.Errorf("operation error: %v", op.GetError())
			}
			if p.GetState() != forgev1.ResourcePlan_STATE_FAILED || !strings.Contains(p.GetMessage(), tc.message) || p.GetPolicy() != nil {
				t.Errorf("plan: %v", p)
			}
			outs := f.outcomes(t, p.GetName())
			if len(outs) != 1 || outs[0].GetResult().GetCode() != "failed" || !strings.HasPrefix(outs[0].GetResult().GetMessage(), tc.code+": ") {
				t.Errorf("outcomes: %v", outs)
			}
			// The target is free again.
			f.runner.FailPlan, f.runner.Crash, f.runner.Silent, f.runner.StartErr, f.runner.Hang = "", "", false, nil, false
			f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
		})
	}
}

func TestPlanUnreadableResult(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		planJSON []byte
		message  string
	}{
		"not JSON": {[]byte("tofu says hi"), "the plan could not be read"},
		"a list":   {[]byte("[]"), "the plan could not be read"},
		"empty":    {[]byte{}, "the runner returned no plan"},
		"too big":  {append([]byte(`{"x":"`), append(make([]byte, 9<<20), `"}`...)...), "the plan is larger than"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newResourceFixture(t, nil)
			f.runner.PlanJSON = tc.planJSON
			p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
			if p.GetState() != forgev1.ResourcePlan_STATE_FAILED || !strings.Contains(p.GetMessage(), tc.message) || op.GetError().GetCode() != "run_failed" {
				t.Errorf("plan %v, operation error %v", p, op.GetError())
			}
			rec, _ := f.store.GetResourcePlan(context.Background(), p.GetName())
			if len(rec.PlanFile) != 0 || len(rec.PlanJSON) != 0 {
				t.Error("a failed plan keeps no plan file")
			}
		})
	}
	// A plan with nothing in it is a plan with no changes.
	f := newResourceFixture(t, nil)
	f.runner.PlanJSON = []byte(`{}`)
	p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	if p.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(p.GetChanges()) != 0 || op.GetError() != nil {
		t.Errorf("an empty plan: %v %v", p, op.GetError())
	}
}

func TestRunTokens(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clock; o.RunTimeout = time.Hour })
	f.runner.Hang = true // the test plays the runner itself
	ctx := context.Background()
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	spec := f.runner.Started()[0]
	if len(spec.Token) != 64 || spec.Plan != resp.GetPlan().GetName() || spec.Mode != run.ModePlan || spec.Service != "payments" || spec.Env != "staging" {
		t.Fatalf("spec: %+v", spec)
	}
	// Only the token's hash is stored.
	rec, _ := f.store.GetResourcePlan(ctx, spec.Plan)
	if rec.TokenHash == "" || rec.TokenHash == spec.Token || strings.Contains(string(rec.Audit), spec.Token) {
		t.Errorf("stored token hash %q", rec.TokenHash)
	}

	for _, bad := range []string{"", "nope", strings.Repeat("0", 64)} {
		_, err := f.res.Claim(ctx, bad)
		wantCode(t, "claim with a wrong token", err, connect.CodeUnauthenticated, "the run token is not valid")
		wantCode(t, "complete with a wrong token", f.res.Complete(ctx, bad, run.Result{Error: "x"}), connect.CodeUnauthenticated, "not valid")
		wantCode(t, "event with a wrong token", f.res.Event(ctx, bad, "s", "m"), connect.CodeUnauthenticated, "not valid")
		_, err = f.res.ReadState(ctx, bad)
		wantCode(t, "state with a wrong token", err, connect.CodeUnauthenticated, "not valid")
	}

	work, err := f.res.Claim(ctx, spec.Token)
	if err != nil {
		t.Fatal(err)
	}
	if work.Plan != spec.Plan || work.Mode != run.ModePlan || work.PlanFile != nil || work.Variables.Namespace != "payments-staging" ||
		work.Variables.Service != "payments" || work.Variables.Environment != "staging" ||
		!slices.Equal(work.Variables.Resources, []run.Resource{{Name: "main", Type: "postgres", StorageGi: 5, Instances: 1}}) {
		t.Errorf("work: %+v", work)
	}
	// A plan run may lock and read the state, never write it.
	if _, err := f.res.LockState(ctx, spec.Token, "lock-1", []byte(`{"ID":"lock-1"}`)); err != nil {
		t.Errorf("lock: %v", err)
	}
	if data, err := f.res.ReadState(ctx, spec.Token); err != nil || data != nil {
		t.Errorf("read state: %s %v", data, err)
	}
	wantCode(t, "a plan run writing state", f.res.WriteState(ctx, spec.Token, "lock-1", []byte("x")), connect.CodeFailedPrecondition, "a plan run may not write the state")
	if err := f.res.UnlockState(ctx, spec.Token, "lock-1"); err != nil {
		t.Errorf("unlock: %v", err)
	}
	if err := f.res.Event(ctx, spec.Token, "plan", "planning\x07"); err != nil {
		t.Errorf("event: %v", err)
	}

	// Past the deadline the token is dead.
	mu.Lock()
	now = now.Add(2 * time.Hour)
	mu.Unlock()
	_, err = f.res.Claim(ctx, spec.Token)
	wantCode(t, "an expired token", err, connect.CodeUnauthenticated, "not valid")
	mu.Lock()
	now = now.Add(-2 * time.Hour)
	mu.Unlock()

	// Completing works once.
	planJSON := []byte(`{"resource_changes":[]}`)
	if err := f.res.Complete(ctx, spec.Token, run.Result{PlanFile: planJSON, PlanJSON: planJSON}); err != nil {
		t.Fatal(err)
	}
	op := waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if op.GetError() != nil || !strings.Contains(eventSteps(op), "plan") {
		t.Errorf("operation: %v", op)
	}
	wantCode(t, "a second complete", f.res.Complete(ctx, spec.Token, run.Result{Error: "late"}), connect.CodeUnauthenticated, "not valid")
	_, err = f.res.Claim(ctx, spec.Token)
	wantCode(t, "a claim after the end", err, connect.CodeUnauthenticated, "not valid")
	if p := f.getPlan(t, spec.Plan); p.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Errorf("plan: %v", p)
	}
	if outs := f.outcomes(t, spec.Plan); len(outs) != 1 || outs[0].GetResult().GetCode() != "ok" {
		t.Errorf("outcomes: %v", outs)
	}
}

func TestCompleteOnce(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	f.runner.Hang = true
	ctx := context.Background()
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for len(f.runner.Started()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	token := f.runner.Started()[0].Token
	planJSON := []byte(`{"resource_changes":[]}`)
	var wg sync.WaitGroup
	var okMu sync.Mutex
	ok := 0
	for range 8 {
		wg.Go(func() {
			if err := f.res.Complete(ctx, token, run.Result{PlanFile: planJSON, PlanJSON: planJSON}); err == nil {
				okMu.Lock()
				ok++
				okMu.Unlock()
			}
		})
	}
	wg.Wait()
	waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if ok != 1 {
		t.Errorf("%d of 8 completes worked, want 1", ok)
	}
	if outs := f.outcomes(t, resp.GetPlan().GetName()); len(outs) != 1 {
		t.Errorf("%d outcome events, want 1", len(outs))
	}
}

func TestListAndViewPlans(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clock })
	ctx := context.Background()
	a, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	b, _ := f.plan(t, "ci", "staging", database("6Gi", 0), false)
	c, _ := f.plan(t, "writer", "production", database("200Gi", 2), false) // denied

	list := func(token string, req *forgev1.ListResourcePlansRequest) []string {
		t.Helper()
		resp, err := f.client.ListResourcePlans(ctx, as(token, req))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range resp.Msg.GetPlans() {
			names = append(names, p.GetName())
		}
		return names
	}
	if got := list("reader", &forgev1.ListResourcePlansRequest{}); !slices.Equal(got, []string{c.GetName(), b.GetName(), a.GetName()}) {
		t.Errorf("all, newest first: %v", got)
	}
	if got := list("writer", &forgev1.ListResourcePlansRequest{Service: "payments", Env: "staging"}); !slices.Equal(got, []string{b.GetName(), a.GetName()}) {
		t.Errorf("staging: %v", got)
	}
	if got := list("writer", &forgev1.ListResourcePlansRequest{Mine: true}); !slices.Equal(got, []string{c.GetName(), a.GetName()}) {
		t.Errorf("mine: %v", got)
	}
	if got := list("writer", &forgev1.ListResourcePlansRequest{States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_DENIED}}); !slices.Equal(got, []string{c.GetName()}) {
		t.Errorf("denied: %v", got)
	}
	// Paging.
	first, err := f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{PageSize: 2}))
	if err != nil || len(first.Msg.GetPlans()) != 2 || first.Msg.GetNextPageToken() == "" {
		t.Fatalf("first page: %v %v", first, err)
	}
	second, err := f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{PageSize: 2, PageToken: first.Msg.GetNextPageToken()}))
	if err != nil || len(second.Msg.GetPlans()) != 1 || second.Msg.GetPlans()[0].GetName() != a.GetName() || second.Msg.GetNextPageToken() != "" {
		t.Errorf("second page: %v %v", second, err)
	}
	_, err = f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{PageToken: "!!"}))
	wantCode(t, "a bad page token", err, connect.CodeInvalidArgument, "page_token")
	_, err = f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{PageSize: -1}))
	wantCode(t, "a negative page size", err, connect.CodeInvalidArgument, "page_size")
	_, err = f.client.ListResourcePlans(ctx, as("writer", &forgev1.ListResourcePlansRequest{Env: "qa"}))
	wantCode(t, "a bad environment", err, connect.CodeInvalidArgument, "staging or production")

	// A day later a plan that was ready reads as expired, before any sweep ran.
	mu.Lock()
	now = now.Add(25 * time.Hour)
	mu.Unlock()
	if p := f.getPlan(t, a.GetName()); p.GetState() != forgev1.ResourcePlan_STATE_EXPIRED {
		t.Errorf("after a day: %v", p.GetState())
	}
	if p := f.getPlan(t, c.GetName()); p.GetState() != forgev1.ResourcePlan_STATE_DENIED {
		t.Errorf("a denied plan stays denied: %v", p.GetState())
	}
	if got := list("writer", &forgev1.ListResourcePlansRequest{States: []forgev1.ResourcePlan_State{forgev1.ResourcePlan_STATE_PLANNED}}); len(got) != 0 {
		t.Errorf("expired plans listed as planned: %v", got)
	}
}

func TestPlanHostileText(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, nil)
	addr := "evil\x1b[31m\u202e" + strings.Repeat("A", 5000)
	body, err := json.Marshal(map[string]any{"format_version": "1.2", "resource_changes": []any{map[string]any{
		"address": addr, "type": "null_resource",
		"change": map[string]any{"actions": []string{"create"}, "before": nil, "after": map[string]any{"id": "1"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	f.runner.PlanJSON = body
	p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	bad := func(s string) bool { return strings.ContainsAny(s, "\x1b\u202e") }
	if p.GetState() != forgev1.ResourcePlan_STATE_DENIED {
		t.Fatalf("plan: %v", p)
	}
	if cs := p.GetChanges(); len(cs) != 1 || bad(cs[0].GetResource()) || len([]rune(cs[0].GetResource())) > 256 {
		t.Errorf("changes: %q", p.GetChanges())
	}
	if len(p.GetPolicy().GetDeny()) == 0 {
		t.Error("no deny messages")
	}
	for _, m := range append(slices.Clone(p.GetPolicy().GetDeny()), p.GetPolicy().GetWarn()...) {
		if bad(m) || len([]rune(m)) > 500 {
			t.Errorf("policy message %q", m)
		}
	}
	if m := op.GetError().GetMessage(); bad(m) {
		t.Errorf("operation error %q", m)
	}
	for _, ev := range op.GetEvents() {
		if bad(ev.GetMessage()) {
			t.Errorf("operation event %q", ev.GetMessage())
		}
	}
	outs := f.outcomes(t, p.GetName())
	if len(outs) != 1 || bad(outs[0].GetResult().GetMessage()) {
		t.Errorf("outcomes: %v", outs)
	}
}

// flakyLauncher wraps a launcher: Stop always fails, and so do the first
// statusFails calls of Status.
type flakyLauncher struct {
	run.Launcher
	mu          sync.Mutex
	statusFails int
}

func (l *flakyLauncher) Status(ctx context.Context, plan, mode string) (run.Status, error) {
	l.mu.Lock()
	fail := l.statusFails > 0
	if fail {
		l.statusFails--
	}
	l.mu.Unlock()
	if fail {
		return run.Status{}, errors.New("status is down")
	}
	return l.Launcher.Status(ctx, plan, mode)
}

func (l *flakyLauncher) Stop(context.Context, string, string) error {
	return errors.New("stop is down")
}

// lockedBuffer is a log sink safe for concurrent writes and reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func (l *flakyLauncher) failsLeft() int { l.mu.Lock(); defer l.mu.Unlock(); return l.statusFails }

func TestRunLogsLauncherErrors(t *testing.T) {
	t.Parallel()
	logs := &lockedBuffer{}
	var fl *flakyLauncher
	f := newResourceFixture(t, func(o *ResourceOptions) {
		fl = &flakyLauncher{Launcher: o.Launcher, statusFails: 3}
		o.Launcher = fl
		o.Log = slog.New(slog.NewTextHandler(logs, nil))
	})
	// A run that hangs is polled until all three failures were read, then
	// cancelled; its runner cannot be stopped.
	f.runner.Hang = true
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	deadline := time.Now().Add(5 * time.Second)
	for fl.failsLeft() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := f.opc.CancelOperation(context.Background(), as("writer", &forgev1.CancelOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
		t.Fatal(err)
	}
	waitCIOp(t, f.ops, resp.GetOperation().GetName())
	out := logs.String()
	if n := strings.Count(out, "reading a resource runner's status"); n != 1 {
		t.Errorf("%d status lines, want 1:\n%s", n, out)
	}
	if !strings.Contains(out, "stopping a resource runner") {
		t.Errorf("no stop line:\n%s", out)
	}
	if token := f.runner.Started()[0].Token; token == "" || strings.Contains(out, token) {
		t.Error("the log must not hold the run token")
	}

	// Errors from the launcher do not end a run that is going well.
	f.runner.Hang = false
	fl.mu.Lock()
	fl.statusFails = 3
	fl.mu.Unlock()
	if p, _ := f.plan(t, "writer", "staging", database("5Gi", 0), false); p.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
		t.Errorf("plan: %v", p)
	}
}

func TestPlanRefusedWhileStoredRunLives(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Now = clock })
	// What a forge-api that died mid-run leaves behind.
	err := f.store.WriteResources(context.Background(), func(tx store.ResourceTx) error {
		return tx.PutResourcePlan(context.Background(), &store.PlanRecord{
			Plan: &forgev1.ResourcePlan{Name: "resourcePlans/ghost", Service: "services/payments", Env: "staging",
				State: forgev1.ResourcePlan_STATE_PLANNING, CreateTime: timestamppb.New(now)},
			TokenHash: "h", Deadline: now.Add(10 * time.Minute),
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.client.PlanResources(context.Background(), as("writer", &forgev1.PlanResourcesRequest{
		Service: "services/payments", Env: "staging", Resources: database("5Gi", 0)}))
	wantCode(t, "a stored run", err, connect.CodeFailedPrecondition, "resourcePlans/ghost is still running for resources/payments/staging")
	f.startPlan(t, "writer", "services/payments", "production", database("5Gi", 0), false)

	mu.Lock()
	now = now.Add(11 * time.Minute)
	mu.Unlock()
	f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
}
