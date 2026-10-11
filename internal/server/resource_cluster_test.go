package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/store"
)

func TestEnsureServiceNamespace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	foreign := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ledger-production"}}
	c := testCluster(foreign)

	// Missing: made with the labels Argo CD's ApplicationSet gives it.
	if err := c.EnsureServiceNamespace(ctx, "payments", "production"); err != nil {
		t.Fatal(err)
	}
	ns, err := c.Kube.CoreV1().Namespaces().Get(ctx, "payments-production", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if l := ns.Labels; l["app.kubernetes.io/managed-by"] != "forge" || l["forge.dev/service"] != "payments" || l["forge.dev/deploy-env"] != "production" || len(l) != 3 {
		t.Errorf("labels: %v", l)
	}
	// So a deploy accepts it as Forge's, and making it again changes nothing.
	if err := c.checkServiceNamespace(ctx, "payments", "production"); err != nil {
		t.Errorf("the namespace is not Forge's: %v", err)
	}
	if err := c.EnsureServiceNamespace(ctx, "payments", "production"); err != nil {
		t.Errorf("a second ensure: %v", err)
	}
	// Someone else's namespace is refused, not taken over.
	wantCode(t, "a foreign namespace", c.EnsureServiceNamespace(ctx, "ledger", "production"), connect.CodeFailedPrecondition, "is not Forge's")
}

func TestDatabaseHealth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster()
	db := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "main", "namespace": "payments-staging"},
		"spec":     map[string]any{"instances": int64(2)},
		"status":   map[string]any{"phase": "Cluster in healthy state", "readyInstances": int64(1)},
	}}
	if _, err := c.Dynamic.Resource(envtest.Databases).Namespace("payments-staging").Create(ctx, db, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h, err := c.DatabaseHealth(ctx, "payments", "staging", "main")
	if err != nil || h.GetPhase() != "Cluster in healthy state" || h.GetReadyInstances() != 1 || h.GetInstances() != 2 {
		t.Errorf("health: %v %v", h, err)
	}
	// A database with no status yet, and one that is not there.
	bare := db.DeepCopy()
	bare.SetName("new")
	delete(bare.Object, "status")
	if _, err := c.Dynamic.Resource(envtest.Databases).Namespace("payments-staging").Create(ctx, bare, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if h, err := c.DatabaseHealth(ctx, "payments", "staging", "new"); err != nil || h.GetPhase() != "unknown" || h.GetReadyInstances() != 0 {
		t.Errorf("no status: %v %v", h, err)
	}
	if _, err := c.DatabaseHealth(ctx, "payments", "staging", "nope"); err == nil {
		t.Error("a missing database: want an error (the caller shows unknown)")
	}
}

// A deploy gives the pods DATABASE_URL from the database the environment
// has now: never copied from another deploy or environment.
func TestDeployDatabaseSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) { o.Resources = st })
	put := func(env, name, secret string, state forgev1.Resource_State) {
		t.Helper()
		err := st.WriteResources(ctx, func(tx store.ResourceTx) error {
			return tx.PutResource(ctx, &forgev1.Resource{Name: "resources/payments/" + env + "/" + name, Service: "services/payments",
				Env: env, Type: "postgres", Secret: secret, State: state})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	deploy := func() (string, *forgev1.Operation) {
		t.Helper()
		resp, err := f.start(t, "staging")
		if err != nil {
			t.Fatal(err)
		}
		op := wait(t, f.ops, resp.GetOperation().GetName())
		data, err := f.repo.Read(ctx, "services/payments/staging/values.yaml", "")
		if err != nil {
			t.Fatal(err)
		}
		return string(data), op
	}

	// No database in staging: production's is not used.
	put("production", "main", "main-app", forgev1.Resource_STATE_READY)
	if values, _ := deploy(); strings.Contains(values, "database") {
		t.Errorf("values without a database:\n%s", values)
	}
	put("staging", "main", "main-app", forgev1.Resource_STATE_READY)
	values, op := deploy()
	if !strings.Contains(values, "database:\n    secret: main-app\n") && !strings.Contains(values, "database:\n  secret: main-app\n") {
		t.Errorf("values with a database:\n%s", values)
	}
	if !strings.Contains(eventSteps(op), "database") {
		t.Errorf("events: %s", eventSteps(op))
	}
	// A database being deleted, or one with no Secret yet, is left out.
	put("staging", "main", "main-app", forgev1.Resource_STATE_DELETING)
	if values, _ := deploy(); strings.Contains(values, "database") {
		t.Errorf("values while the database is deleted:\n%s", values)
	}
	put("staging", "main", "", forgev1.Resource_STATE_APPLYING)
	if values, _ := deploy(); strings.Contains(values, "database") {
		t.Errorf("values before the database has a Secret:\n%s", values)
	}
}

// A store that cannot be read fails the deploy: dropping DATABASE_URL
// silently would break the app.
func TestDeployDatabaseStoreDown(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) { o.Resources = downResources{} })
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	op := wait(t, f.ops, resp.GetOperation().GetName())
	if op.GetError() == nil || !strings.Contains(op.GetError().GetMessage(), "read the resources of services/payments in staging") {
		t.Errorf("operation: %v", op.GetError())
	}
	if _, err := f.repo.Read(context.Background(), "services/payments/staging/values.yaml", ""); err == nil {
		t.Error("values were committed without knowing the database")
	}
}

type downResources struct{ store.ResourceStore }

func (downResources) ListResources(context.Context, string, string) ([]*forgev1.Resource, error) {
	return nil, errors.New("the database is away")
}
