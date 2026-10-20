//go:build e2e

package server

// An end-to-end check of resources against a real cluster: real Jobs, the
// real runner image, real OpenTofu and CloudNativePG. It is not part of the
// normal suite. Run it only against a throwaway cluster that has
// CloudNativePG, a longhorn-retain storage class, the objects of
// deploy/k3s/resources.yaml.tmpl and the runner image:
//
//	FORGE_E2E_KUBECONFIG=<kubeconfig of the throwaway cluster> \
//	FORGE_E2E_RUNNER_IMAGE=forge-runner:dev \
//	FORGE_E2E_API_URL=http://<address of this machine as pods reach it>:18081 \
//	go test -tags e2e -run TestResourcesEndToEnd -count=1 -timeout 20m ./internal/server
//
// TestResourcesInterrupted (same variables, -timeout 45m) cuts a first apply
// short twice: a cancel while OpenTofu waits for the database, and a wait that
// times out. It removes and puts back the cluster's longhorn-retain storage
// class, so it too is for a throwaway cluster only.

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// e2e is a resource service wired to a throwaway cluster, with helpers.
type e2e struct {
	t       *testing.T
	ctx     context.Context
	kube    *kubernetes.Clientset
	dyn     *dynamic.DynamicClient
	st      *store.Memory
	cat     *Catalog
	res     *Resources
	ops     *Operations
	service string
	ns      string
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	kubeconfig, image, apiURL := os.Getenv("FORGE_E2E_KUBECONFIG"), os.Getenv("FORGE_E2E_RUNNER_IMAGE"), os.Getenv("FORGE_E2E_API_URL")
	if kubeconfig == "" || image == "" || apiURL == "" {
		t.Skip("set FORGE_E2E_KUBECONFIG, FORGE_E2E_RUNNER_IMAGE and FORGE_E2E_API_URL")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cluster := &Cluster{Kube: kube, Dynamic: dyn}
	ctx := writerCtx()

	st := store.NewMemory()
	cat, ops := NewCatalogWithStore(st, nil), NewOperations(nil)
	service := "e2e" + time.Now().Format("150405")
	register(t, cat, service)
	rules, err := policy.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := NewResources(ResourceOptions{
		Store: st, Catalog: cat, Operations: ops, Policy: rules, Namespaces: cluster, Health: cluster,
		Launcher: &JobLauncher{Kube: kube, Namespace: "forge-runner", Image: image, ServiceAccount: "forge-runner", APIURL: apiURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The runner API, on the port the Jobs were told.
	u, err := url.Parse(apiURL)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", ":"+u.Port())
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: RunnerHandler(res, nil), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	e := &e2e{t: t, ctx: ctx, kube: kube, dyn: dyn, st: st, cat: cat, res: res, ops: ops}
	e.use(service)
	return e
}

// use makes service, registered, the one the helpers work on, and removes
// its namespace when the test ends.
func (e *e2e) use(service string) {
	e.service, e.ns = service, service+"-staging"
	ns := e.ns
	e.t.Cleanup(func() { _ = e.kube.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })
}

// fresh registers a new service and works on it from now on.
func (e *e2e) fresh(suffix string) {
	service := "e2e" + time.Now().Format("150405") + suffix
	register(e.t, e.cat, service)
	e.use(service)
}

func (e *e2e) waitOp(name string) *forgev1.Operation {
	e.t.Helper()
	wctx, cancel := context.WithTimeout(e.ctx, 20*time.Minute)
	defer cancel()
	op, err := e.ops.Wait(wctx, name)
	if err != nil {
		e.t.Fatalf("waiting for %s: %v", name, err)
	}
	for _, ev := range op.GetEvents() {
		e.t.Logf("  %-10s %s", ev.GetStep(), ev.GetMessage())
	}
	return op
}

// startPlan starts a plan for staging and returns its operation and plan names.
func (e *e2e) startPlan(decls []*forgev1.ResourceDeclaration, allowDestroy bool) (op, plan string) {
	e.t.Helper()
	resp, err := e.res.PlanResources(e.ctx, connect.NewRequest(&forgev1.PlanResourcesRequest{
		Service: "services/" + e.service, Env: "staging", Resources: decls, AllowDestroy: allowDestroy}))
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.Msg.GetOperation().GetName(), resp.Msg.GetPlan().GetName()
}

func (e *e2e) getPlan(name string) *forgev1.ResourcePlan {
	e.t.Helper()
	got, err := e.res.GetResourcePlan(e.ctx, connect.NewRequest(&forgev1.GetResourcePlanRequest{Name: name}))
	if err != nil {
		e.t.Fatal(err)
	}
	return got.Msg.GetPlan()
}

// plan makes a plan for staging; a run that fails stops the test, a plan
// policy denies does not.
func (e *e2e) plan(decls []*forgev1.ResourceDeclaration, allowDestroy bool) *forgev1.ResourcePlan {
	e.t.Helper()
	opName, planName := e.startPlan(decls, allowDestroy)
	if op := e.waitOp(opName); op.GetError() != nil && op.GetError().GetCode() != OpCodePolicyDenied {
		e.t.Fatalf("plan: %v", op.GetError())
	}
	return e.getPlan(planName)
}

// startApply starts an apply and returns its operation's name.
func (e *e2e) startApply(p *forgev1.ResourcePlan) string {
	e.t.Helper()
	resp, err := e.res.ApplyResourcePlan(e.ctx, connect.NewRequest(&forgev1.ApplyResourcePlanRequest{Name: p.GetName()}))
	if err != nil {
		e.t.Fatal(err)
	}
	return resp.Msg.GetOperation().GetName()
}

func (e *e2e) apply(p *forgev1.ResourcePlan) {
	e.t.Helper()
	if op := e.waitOp(e.startApply(p)); op.GetError() != nil {
		e.t.Fatalf("apply: %v", op.GetError())
	}
}

func (e *e2e) resource() *forgev1.Resource {
	e.t.Helper()
	got, err := e.res.GetResource(e.ctx, connect.NewRequest(&forgev1.GetResourceRequest{Name: "resources/" + e.service + "/staging/main"}))
	if err != nil {
		e.t.Fatal(err)
	}
	return got.Msg.GetResource()
}

// databases are the CloudNativePG clusters in the service's namespace.
func (e *e2e) databases() []string {
	e.t.Helper()
	list, err := e.dyn.Resource(cnpgClusters).Namespace(e.ns).List(e.ctx, metav1.ListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, c := range list.Items {
		out = append(out, c.GetName())
	}
	return out
}

func TestResourcesEndToEnd(t *testing.T) {
	e := newE2E(t)
	ctx, kube, dyn, st, res, ops, service := e.ctx, e.kube, e.dyn, e.st, e.res, e.ops, e.service
	plan, apply := e.plan, e.apply
	ns := e.ns

	// Plan: nothing exists, not even the namespace.
	p := plan(database("1Gi", 0), false)
	if p.GetState() != forgev1.ResourcePlan_STATE_PLANNED || len(p.GetChanges()) != 1 || p.GetChanges()[0].GetAction() != "create" || p.GetPolicy().GetDenied() {
		t.Fatalf("the first plan: %v", p)
	}
	if _, err := kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err == nil {
		t.Error("the plan made the namespace")
	}

	// Apply: the namespace, the database, its Secret, the record.
	apply(p)
	got, err := res.GetResource(ctx, connect.NewRequest(&forgev1.GetResourceRequest{Name: "resources/" + service + "/staging/main"}))
	if err != nil {
		t.Fatal(err)
	}
	r := got.Msg.GetResource()
	if r.GetState() != forgev1.Resource_STATE_READY || r.GetSecret() != "main-app" || r.GetHealth().GetPhase() != "Cluster in healthy state" || r.GetHealth().GetReadyInstances() != 1 {
		t.Errorf("the resource after the apply: %v", r)
	}
	secret, err := kube.CoreV1().Secrets(ns).Get(ctx, "main-app", metav1.GetOptions{})
	if err != nil || len(secret.Data["uri"]) == 0 {
		t.Errorf("the database's Secret: %v (uri present: %v)", err, secret != nil && len(secret.Data["uri"]) > 0)
	}

	// The same again changes nothing; then the database goes.
	if same := plan(database("1Gi", 0), false); len(same.GetChanges()) != 0 {
		t.Errorf("a second plan is not empty (the module drifts): %v", same.GetChanges())
	}
	if refused := planDenied(t, res, ops, ctx, service); !strings.Contains(refused, "the plan deletes database main") {
		t.Errorf("a delete without allow_destroy: %q", refused)
	}
	gone := plan(nil, true)
	if len(gone.GetChanges()) != 1 || gone.GetChanges()[0].GetAction() != "delete" {
		t.Fatalf("the destroy plan: %v", gone)
	}
	apply(gone)
	if _, err := dyn.Resource(cnpgClusters).Namespace(ns).Get(ctx, "main", metav1.GetOptions{}); err == nil {
		t.Error("the database is still there after the destroy")
	}
	if rs, _ := st.ListResources(ctx, "services/"+service, ""); len(rs) != 0 {
		t.Errorf("records after the destroy: %v", rs)
	}
}

// TestResourcesInterrupted cuts a first apply short while OpenTofu waits for
// the database, by a cancel and by the wait's own timeout, and checks that a
// new plan and apply then end with one healthy database that Forge and
// OpenTofu both know about, without allow_destroy.
func TestResourcesInterrupted(t *testing.T) {
	e := newE2E(t)

	// settled checks the end state: one healthy database, a ready record with
	// its Secret, and a next plan with nothing to change or repair.
	// A database kept after a timeout may still be starting: give it time.
	settled := func(when string) {
		e.t.Helper()
		if dbs := e.databases(); len(dbs) != 1 || dbs[0] != "main" {
			e.t.Errorf("%s: databases %v", when, dbs)
		}
		r := e.resource()
		for end := time.Now().Add(5 * time.Minute); r.GetHealth().GetReadyInstances() != 1 && time.Now().Before(end); r = e.resource() {
			time.Sleep(5 * time.Second)
		}
		if r.GetState() != forgev1.Resource_STATE_READY || r.GetSecret() != "main-app" || r.GetHealth().GetReadyInstances() != 1 {
			e.t.Errorf("%s: the resource %v", when, r)
		}
		if p := e.plan(database("1Gi", 0), false); len(p.GetChanges()) != 0 || p.GetMessage() != "" {
			e.t.Errorf("%s: a further plan has changes %v or message %q", when, p.GetChanges(), p.GetMessage())
		}
	}
	// repair plans again and applies; a plan that policy denies is the finding.
	repair := func(when string) {
		e.t.Helper()
		p := e.plan(database("1Gi", 0), false)
		e.t.Logf("%s: the repair plan is %s with changes %v, message %q, policy %v", when, p.GetState(), p.GetChanges(), p.GetMessage(), p.GetPolicy())
		if p.GetState() != forgev1.ResourcePlan_STATE_PLANNED {
			e.t.Fatalf("%s: the repair plan is %s, not ready to apply", when, p.GetState())
		}
		e.apply(p)
	}
	destroy := func() {
		e.t.Helper()
		e.apply(e.plan(nil, true))
		if dbs := e.databases(); len(dbs) != 0 {
			e.t.Errorf("after the destroy: databases %v", dbs)
		}
	}

	t.Run("cancelled while waiting for the database", func(t *testing.T) {
		e.t = t
		e.fresh("c")
		p := e.plan(database("1Gi", 0), false)
		opName := e.startApply(p)
		// Cancel once the database exists and OpenTofu is waiting for it.
		deadline := time.Now().Add(5 * time.Minute)
		for len(e.databases()) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("the database never appeared")
			}
			time.Sleep(2 * time.Second)
		}
		time.Sleep(5 * time.Second)
		if _, ok := e.ops.Cancel(opName); !ok {
			t.Fatal("the apply could not be cancelled")
		}
		if op := e.waitOp(opName); op.GetError().GetCode() != OpCodeCancelled {
			t.Fatalf("the cancelled apply ended with %v", op.GetError())
		}
		t.Logf("after the cancel: record %v, databases %v", e.resource(), e.databases())
		repair("after a cancel")
		settled("after a cancel")
		destroy()
	})

	t.Run("the wait times out", func(t *testing.T) {
		e.t = t
		e.fresh("t")
		// Without its storage class the database's volume never binds, so it
		// never becomes healthy and OpenTofu's 10 minute wait runs out.
		sc, err := e.kube.StorageV1().StorageClasses().Get(e.ctx, "longhorn-retain", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.kube.StorageV1().StorageClasses().Delete(e.ctx, "longhorn-retain", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		putBack := func() {
			sc.ResourceVersion, sc.UID = "", ""
			if _, err := e.kube.StorageV1().StorageClasses().Create(context.Background(), sc, metav1.CreateOptions{}); err != nil && !strings.Contains(err.Error(), "already exists") {
				t.Errorf("putting the storage class back: %v", err)
			}
		}
		t.Cleanup(putBack)
		p := e.plan(database("1Gi", 0), false)
		op := e.waitOp(e.startApply(p))
		if op.GetError() == nil {
			t.Fatal("the apply passed without a storage class")
		}
		t.Logf("the timed out apply: %v; record %v, databases %v", op.GetError(), e.resource(), e.databases())
		putBack()
		repair("after a timeout")
		settled("after a timeout")
		destroy()
	})
}

// planDenied plans the removal of everything without allow_destroy and
// returns the operation's error message.
func planDenied(t *testing.T, res *Resources, ops *Operations, ctx context.Context, service string) string { //nolint:revive // a test helper
	t.Helper()
	resp, err := res.PlanResources(ctx, connect.NewRequest(&forgev1.PlanResourcesRequest{Service: "services/" + service, Env: "staging"}))
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	op, err := ops.Wait(wctx, resp.Msg.GetOperation().GetName())
	if err != nil {
		t.Fatal(err)
	}
	return op.GetError().GetMessage()
}
