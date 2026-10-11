package server

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/run"
	"github.com/showmino/forge/internal/store"
)

func TestRunText(t *testing.T) {
	t.Parallel()
	if got := runText("  Initializing...\n\n\tError: no capacity \n  on main.tf line 3\n"); got != "Initializing...; Error: no capacity; on main.tf line 3" {
		t.Errorf("lines: %q", got)
	}
	if got := runText(" \n\t\n"); got != "" {
		t.Errorf("blank: %q", got)
	}
	// The end is kept: OpenTofu puts the error last.
	long := strings.Repeat("progress line\n", 200) + "Error: the real reason"
	got := runText(long)
	if !strings.HasSuffix(got, "Error: the real reason") || !strings.HasPrefix(got, "…") || utf8.RuneCountInString(got) > auditMessageMax-39 {
		t.Errorf("long output: %d runes, %q", utf8.RuneCountInString(got), got)
	}
	// Multi-byte text is cut on a rune boundary.
	if got := runText(strings.Repeat("é", 1000)); !utf8.ValidString(got) {
		t.Error("cut inside a rune")
	}
}

// slowStart is a launcher whose Start waits until it is cancelled.
type slowStart struct {
	*envtest.Runner
	started chan struct{}
}

func (s *slowStart) Start(ctx context.Context, _ run.Spec) error {
	close(s.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestCancelDuringLaunch(t *testing.T) {
	t.Parallel()
	slow := &slowStart{Runner: &envtest.Runner{}, started: make(chan struct{})}
	f := newResourceFixture(t, func(o *ResourceOptions) { o.Launcher = slow })
	resp := f.startPlan(t, "writer", "services/payments", "staging", database("5Gi", 0), false)
	<-slow.started
	if _, err := f.opc.CancelOperation(context.Background(), as("writer", &forgev1.CancelOperationRequest{Name: resp.GetOperation().GetName()})); err != nil {
		t.Fatal(err)
	}
	op := waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if op.GetError().GetCode() != "cancelled" {
		t.Errorf("a cancel while the runner was starting: %v, want cancelled", op.GetError())
	}
	if outs := f.outcomes(t, resp.GetPlan().GetName()); len(outs) != 1 || outs[0].GetResult().GetCode() != "cancelled" {
		t.Errorf("outcomes: %v", outs)
	}
}

// The run ends when its token does, however long the launch took.
func TestRunDeadlineIsTheTokens(t *testing.T) {
	t.Parallel()
	f := newResourceFixture(t, func(o *ResourceOptions) { o.RunTimeout = 60 * time.Millisecond })
	f.runner.Hang = true
	start := time.Now()
	p, op := f.plan(t, "writer", "staging", database("5Gi", 0), false)
	if op.GetError().GetCode() != "run_timeout" || p.GetState() != forgev1.ResourcePlan_STATE_FAILED {
		t.Fatalf("plan %v, operation error %v", p.GetState(), op.GetError())
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the run took %s to time out", took)
	}
}

// The delete guard holds with resources turned off, as long as the catalog
// was given the store.
func TestDeleteGuardWithResourcesOff(t *testing.T) {
	t.Parallel()
	st := store.NewMemory()
	cat := NewCatalogWithStore(st, nil)
	register(t, cat, "payments")
	register(t, cat, "billing")
	err := st.WriteResources(context.Background(), func(tx store.ResourceTx) error {
		return tx.PutResource(context.Background(), &forgev1.Resource{
			Name: "resources/payments/staging/main", Service: "services/payments", Env: "staging", Type: "postgres",
			State: forgev1.Resource_STATE_READY,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	del := func(name string) error {
		_, err := cat.DeleteService(writerCtx(), connect.NewRequest(&forgev1.DeleteServiceRequest{Name: name}))
		return err
	}
	// Without the guard the catalog does not look.
	if err := del("services/billing"); err != nil {
		t.Fatal(err)
	}
	cat.GuardResources(st)
	wantCode(t, "a service with a record, resources off", del("services/payments"), connect.CodeFailedPrecondition, "still has resources (resources/payments/staging/main)")
}
