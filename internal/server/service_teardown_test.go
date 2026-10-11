package server

import (
	"context"
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
		if there, _ := f.cluster.ApplicationExists(ctx, "argocd", "payments-"+env); there {
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

	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil {
		t.Fatal(err)
	}
	if op.GetKind() != kindDeleteService || op.GetTarget() != "services/payments" || op.GetCreator() != "ana" {
		t.Errorf("operation: %v", op)
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
		release, err := f.deploys.lockService("payments")
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
		release()
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "already running") {
			t.Errorf("got %v", err)
		}
	})
	// Every refusal came before anything was taken down, and left no lock behind.
	if got := f.left(t); got != everything {
		t.Errorf("after the refusals: %s", got)
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
	f := newTeardownFixture(t, func(o *DeployOptions, _ *envtest.Argo) { o.SyncTimeout = 150 * time.Millisecond })
	f.deploys.o.SyncTimeout = 5 * time.Second
	f.deployBoth(t)
	// Argo CD stops reacting: the applications stay.
	f.argo.SetLag(time.Hour)
	f.deploys.o.SyncTimeout = 150 * time.Millisecond

	op, err := f.delete(writerCtx(), &forgev1.DeleteServiceRequest{Name: "services/payments"})
	if err != nil {
		t.Fatal(err)
	}
	failed := wait(t, f.ops, op.GetName())
	if failed.GetError().GetCode() != "sync_timeout" || !strings.Contains(failed.GetError().GetMessage(), "payments-production") {
		t.Fatalf("got %v", failed.GetError())
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
	if err := f.cluster.DeleteServiceNamespace(ctx, "payments", "staging"); err == nil {
		t.Error("deleting a foreign namespace must be refused")
	}
}
