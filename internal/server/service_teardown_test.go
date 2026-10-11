package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

// teardownFixture is the deploy fixture with payments running in staging and
// production, one preview of it, and DeleteService taking all of it down.
type teardownFixture struct {
	*deployFixture
	envs *Environments
	// removes is what the last delete said it takes down.
	removes []string
}

func newTeardownFixture(t *testing.T, tweak func(*DeployOptions, *envtest.Argo)) *teardownFixture {
	t.Helper()
	f := &teardownFixture{deployFixture: newDeployFixture(t, false, tweak)}
	f.envs = NewEnvironments(EnvOptions{
		Cluster: f.cluster, Catalog: f.catalog, Operations: f.ops, Org: "showmino", Domain: "preview.example",
		Poll: 5 * time.Millisecond, DestroyTimeout: 5 * time.Second,
	})
	f.catalog.EnableTeardown(f.deploys, f.envs, f.ops)
	return f
}

// deployBoth puts payments into staging and production.
func (f *teardownFixture) deployBoth(t *testing.T) {
	t.Helper()
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, resp.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("deploy: %v", op.GetError())
	}
	p, err := f.client.PromoteDeploy(context.Background(), as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	if err != nil {
		t.Fatal(err)
	}
	if op := wait(t, f.ops, p.Msg.GetOperation().GetName()); op.GetError() != nil {
		t.Fatalf("promote: %v", op.GetError())
	}
}

func (f *teardownFixture) preview(t *testing.T, id, service string) {
	t.Helper()
	if err := f.cluster.CreateEnv(context.Background(), id, map[string]string{labelService: service}, nil); err != nil {
		t.Fatal(err)
	}
}

func (f *teardownFixture) delete(ctx context.Context, req *forgev1.DeleteServiceRequest) (*forgev1.Operation, error) {
	resp, err := f.catalog.DeleteService(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	f.removes = resp.Msg.GetRemoves()
	return resp.Msg.GetOperation(), nil
}

// left says what of payments still exists, as one line.
func (f *teardownFixture) left(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var out []string
	for _, env := range deployEnvs {
		if _, err := f.repo.Read(ctx, valuesPath("payments", env), ""); err == nil {
			out = append(out, "values:"+env)
		}
		if there, _ := f.cluster.ForgeApplicationExists(ctx, "argocd", "payments-"+env); there {
			out = append(out, "app:"+env)
		}
		if _, err := f.cluster.Kube.CoreV1().Namespaces().Get(ctx, "payments-"+env, metav1.GetOptions{}); err == nil {
			out = append(out, "ns:"+env)
		}
	}
	nss, err := f.cluster.Envs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range nss {
		out = append(out, "preview:"+ns.Labels[labelEnvironment])
	}
	if has, _ := f.catalog.has(ctx, "services/payments"); has {
		out = append(out, "catalog")
	}
	return strings.Join(out, " ")
}

const everything = "values:staging app:staging ns:staging values:production app:production ns:production preview:pr-1 preview:theirs catalog"

func TestDeleteServiceTakesEverythingDown(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	f.deployBoth(t)
	f.preview(t, "pr-1", "payments")
	f.preview(t, "theirs", "ledger") // another service's preview stays
	if got := f.left(t); got != everything {
		t.Fatalf("before: %s", got)
	}

	// A dry run says it would work and touches nothing.
	if op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments", ValidateOnly: true}); err != nil || op != nil {
		t.Fatalf("dry run: %v %v", op, err)
	}
	if got := f.left(t); got != everything {
		t.Fatalf("after a dry run: %s", got)
	}
	// It names what a delete takes down: the preview, production before staging. Not another service's preview.
	if got := strings.Join(f.removes, " "); got != "environments/pr-1 deployments/payments/production deployments/payments/staging" {
		t.Errorf("a dry run names: %s", got)
	}

	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil {
		t.Fatal(err)
	}
	if op.GetKind() != kindDeleteService || op.GetTarget() != "services/payments" || op.GetCreator() != "ana" {
		t.Errorf("operation: %v", op)
	}
	// While it runs nothing can be started for the service, and asking again gets the same delete.
	if _, err := f.start(t, "staging"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("a deploy during the delete: %v", err)
	}
	if again, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"}); err != nil || again.GetName() != op.GetName() {
		t.Errorf("a second delete while the first runs: %v %v, want %s", again.GetName(), err, op.GetName())
	}
	done := wait(t, f.ops, op.GetName())
	if done.GetError() != nil {
		t.Fatalf("delete: %v", done.GetError())
	}
	// Previews first, then production before staging, the catalog entry last.
	if got := eventSteps(done); got != "preview,destroy,commit,argo,namespace,commit,argo,namespace,catalog" {
		t.Errorf("steps: %s", got)
	}
	var said []string
	for _, e := range done.GetEvents() {
		said = append(said, e.GetMessage())
	}
	if all := strings.Join(said, "\n"); strings.Index(all, "payments from production") > strings.Index(all, "payments from staging") ||
		!strings.Contains(all, "its GitHub repo is kept") {
		t.Errorf("events:\n%s", all)
	}
	if got := f.left(t); got != "preview:theirs" {
		t.Errorf("after the delete: %s", got)
	}
	// Nothing stays locked: the name can be used again.
	f.deploys.mu.Lock()
	running := len(f.deploys.running)
	f.deploys.mu.Unlock()
	if running != 0 {
		t.Errorf("%d deploy targets still locked", running)
	}
	if err := f.catalog.refuseDeleting("services/payments"); err != nil {
		t.Errorf("still marked as being deleted: %v", err)
	}
	// A deploy for a service that is gone is refused, not started.
	if _, err := f.start(t, "staging"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a deploy after the delete: %v", err)
	}
}

func TestDeleteServiceThatRunsNothing(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil || op != nil {
		t.Fatalf("got %v %v: a service that runs nothing is removed at once, with no operation", op, err)
	}
	if got := f.left(t); got != "" {
		t.Errorf("left: %s", got)
	}
	if _, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a second delete: %v", err)
	}
}

func TestDeleteServiceRefusals(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	f.deployBoth(t)
	f.preview(t, "pr-1", "payments")
	f.preview(t, "theirs", "ledger")
	reader := context.WithValue(context.Background(), principalKey{}, &Principal{Subject: "bo", Permissions: []string{PermServicesRead}})

	t.Run("a reader", func(t *testing.T) {
		if _, err := f.delete(reader, &forgev1.DeleteServiceRequest{Name: "services/payments"}); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("got %v", err)
		}
	})
	t.Run("a stale etag", func(t *testing.T) {
		if _, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments", Etag: `W/"999"`}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("got %v", err)
		}
	})
	t.Run("another service depends on it", func(t *testing.T) {
		register(t, f.catalog, "checkout", "services/payments")
		_, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "services/checkout") {
			t.Errorf("got %v", err)
		}
		if _, err := f.catalog.DeleteService(writerCtx(), connect.NewRequest(&forgev1.DeleteServiceRequest{Name: "services/checkout"})); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("a deploy is running", func(t *testing.T) {
		// A real one, held up by an Argo CD that does not react.
		f.argo.SetLag(time.Hour)
		f.images.Add("ghcr.io/showmino/payments:9f8e7d6")
		resp, err := f.client.StartDeploy(context.Background(), as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", Ref: "release"}))
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "already running") {
			t.Errorf("got %v", err)
		}
		// A dry run says the same: it must not promise a delete that would be refused.
		if _, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments", ValidateOnly: true}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Errorf("dry run during a deploy: %v", err)
		}
		f.ops.Cancel(resp.Msg.GetOperation().GetName())
		wait(t, f.ops, resp.Msg.GetOperation().GetName())
		f.argo.SetLag(0)
	})
	// Every refusal came before anything was taken down, and left no lock or mark behind.
	if got := f.left(t); got != everything {
		t.Errorf("after the refusals: %s", got)
	}
	if err := f.catalog.refuseDeleting("services/payments"); err != nil {
		t.Errorf("a refused delete left its mark: %v", err)
	}
	if release, err := f.deploys.lockService("payments"); err != nil {
		t.Errorf("a refused delete left a lock: %v", err)
	} else {
		release()
	}
}

// A delete that cannot finish keeps the catalog entry, so the service stays
// visible and the delete can be asked for again; the second one finishes.
func TestDeleteServiceCanBeRetried(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	f.deployBoth(t)
	// Argo CD stops reacting: the applications stay.
	f.argo.SetLag(time.Hour)
	f.deploys.o.SyncTimeout = 150 * time.Millisecond

	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil {
		t.Fatal(err)
	}
	failed := wait(t, f.ops, op.GetName())
	if e := failed.GetError(); e.GetCode() != "sync_timeout" || !strings.Contains(e.GetMessage(), "the Argo CD application payments-production still exists after") ||
		strings.Contains(e.GetMessage(), "deadline") {
		t.Fatalf("got %v", e)
	}
	// Production's values are out of the repo; the rest, and the entry, are still there.
	if got := f.left(t); got != "values:staging app:staging ns:staging app:production ns:production catalog" {
		t.Errorf("after the failed delete: %s", got)
	}

	f.argo.SetLag(0)
	f.deploys.o.SyncTimeout = 5 * time.Second
	op, err = f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil {
		t.Fatal(err)
	}
	if done := wait(t, f.ops, op.GetName()); done.GetError() != nil {
		t.Fatalf("second delete: %v", done.GetError())
	}
	if got := f.left(t); got != "" {
		t.Errorf("after the second delete: %s", got)
	}
}

// Cancelling after the service left the deploy repo cannot stop Argo CD from
// taking it down, and the operation says so instead of a bare "cancelled".
func TestDeleteServiceCancelledHalfWay(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	f.deployBoth(t)
	f.argo.SetLag(time.Hour) // the applications stay: the delete waits for Argo CD

	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until production's values are out of the repo, then cancel.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if _, err := f.repo.Read(context.Background(), valuesPath("payments", "production"), ""); err != nil {
			break
		}
	}
	f.ops.Cancel(op.GetName())
	cancelled := wait(t, f.ops, op.GetName())
	e := cancelled.GetError()
	if e.GetCode() != OpCodeCancelled || !strings.Contains(e.GetMessage(), "payments is already removed from production in the deploy repo (commit ") ||
		!strings.Contains(e.GetMessage(), "delete again to finish") {
		t.Fatalf("got %v", e)
	}
	// The service is still in the catalog and nothing is left claimed: the delete can be finished.
	if got := f.left(t); !strings.HasSuffix(got, "catalog") {
		t.Errorf("left: %s", got)
	}
	f.argo.SetLag(0)
	op, err = f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil {
		t.Fatal(err)
	}
	if done := wait(t, f.ops, op.GetName()); done.GetError() != nil {
		t.Fatalf("finishing the delete: %v", done.GetError())
	}
	if got := f.left(t); got != "" {
		t.Errorf("after finishing: %s", got)
	}
}

// A preview that is still waiting for its image has no namespace yet. The
// delete takes it down too, and no preview can be started meanwhile.
func TestDeleteServiceWithAPreviewBeingCreated(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	f.deployBoth(t)
	// As CreateEnvironment leaves it while the create waits for its image.
	f.envs.mu.Lock()
	f.envs.pending["pr-9"] = "payments"
	f.envs.pending["other"] = "ledger"
	f.envs.mu.Unlock()
	if _, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments", ValidateOnly: true}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.removes, " "); !strings.HasPrefix(got, "environments/pr-9 ") || strings.Contains(got, "other") {
		t.Errorf("a dry run names: %s", got)
	}
	// And while a service is being deleted, nothing new is started for it.
	if !f.catalog.markDeleting("services/payments", "operations/op_test") {
		t.Fatal("could not mark")
	}
	if err := f.catalog.refuseDeleting("services/payments"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "being deleted") {
		t.Errorf("got %v", err)
	}
	if _, err := f.start(t, "staging"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "being deleted") {
		t.Errorf("a deploy: %v", err)
	}
	f.catalog.unmarkDeleting("services/payments")
}

// With deploys and previews switched off there is nothing to take down:
// delete removes the catalog entry, as before.
func TestDeleteServiceWithoutDeploysOrPreviews(t *testing.T) {
	t.Parallel()
	c := NewCatalog(nil)
	c.EnableTeardown(nil, nil, NewOperations(nil))
	register(t, c, "payments")
	resp, err := c.DeleteService(writerCtx(), connect.NewRequest(&forgev1.DeleteServiceRequest{Name: "services/payments"}))
	if err != nil || resp.Msg.GetOperation() != nil || len(resp.Msg.GetRemoves()) != 0 {
		t.Fatalf("got %v %v", resp, err)
	}
	if has, _ := c.has(context.Background(), "services/payments"); has {
		t.Error("still in the catalog")
	}
}

// A namespace with the service's name that is not Forge's is never touched.
func TestDeleteServiceLeavesForeignNamespaces(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	ctx := context.Background()
	if err := f.cluster.EnsureServiceNamespace(ctx, "payments", "staging"); err != nil {
		t.Fatal(err)
	}
	ns, err := f.cluster.Kube.CoreV1().Namespaces().Get(ctx, "payments-staging", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.Labels = map[string]string{"owner": "someone-else"}
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil || op != nil {
		t.Fatalf("got %v %v", op, err)
	}
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Get(ctx, "payments-staging", metav1.GetOptions{}); apierrors.IsNotFound(err) {
		t.Error("the foreign namespace was deleted")
	}
	if err := f.cluster.DeleteServiceNamespace(ctx, "payments", "staging"); !errors.Is(err, errUnlabelled) {
		t.Errorf("deleting a foreign namespace must be refused, got %v", err)
	}
}

// The same when the service is deployed and its namespace was taken over by
// someone else meanwhile: the delete removes what is Forge's and leaves that
// namespace alone.
func TestDeleteServiceLeavesANamespaceThatIsNoLongerForges(t *testing.T) {
	t.Parallel()
	f := newTeardownFixture(t, nil)
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	wait(t, f.ops, resp.GetOperation().GetName())
	ctx := context.Background()
	ns, err := f.cluster.Kube.CoreV1().Namespaces().Get(ctx, "payments-staging", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.Labels = map[string]string{"owner": "someone-else"}
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The fake Argo CD would relabel the namespace on its next pass: it only creates, so the labels stay.
	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil {
		t.Fatal(err)
	}
	done := wait(t, f.ops, op.GetName())
	if done.GetError() != nil {
		t.Fatalf("delete: %v", done.GetError())
	}
	if got := eventSteps(done); got != "commit,argo,namespace,catalog" {
		t.Errorf("steps: %s", got)
	}
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Get(ctx, "payments-staging", metav1.GetOptions{}); err != nil {
		t.Errorf("the namespace that is not Forge's was deleted: %v", err)
	}
	if has, _ := f.catalog.has(ctx, "services/payments"); has {
		t.Error("still in the catalog")
	}
}
