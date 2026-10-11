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

func TestResourcesEndToEnd(t *testing.T) {
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

	waitOp := func(name string) *forgev1.Operation {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
		defer cancel()
		op, err := ops.Wait(wctx, name)
		if err != nil {
			t.Fatalf("waiting for %s: %v", name, err)
		}
		for _, ev := range op.GetEvents() {
			t.Logf("  %-10s %s", ev.GetStep(), ev.GetMessage())
		}
		return op
	}
	plan := func(decls []*forgev1.ResourceDeclaration, allowDestroy bool) *forgev1.ResourcePlan {
		t.Helper()
		resp, err := res.PlanResources(ctx, connect.NewRequest(&forgev1.PlanResourcesRequest{
			Service: "services/" + service, Env: "staging", Resources: decls, AllowDestroy: allowDestroy}))
		if err != nil {
			t.Fatal(err)
		}
		if op := waitOp(resp.Msg.GetOperation().GetName()); op.GetError() != nil {
			t.Fatalf("plan: %v", op.GetError())
		}
		got, err := res.GetResourcePlan(ctx, connect.NewRequest(&forgev1.GetResourcePlanRequest{Name: resp.Msg.GetPlan().GetName()}))
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.GetPlan()
	}
	apply := func(p *forgev1.ResourcePlan) {
		t.Helper()
		resp, err := res.ApplyResourcePlan(ctx, connect.NewRequest(&forgev1.ApplyResourcePlanRequest{Name: p.GetName()}))
		if err != nil {
			t.Fatal(err)
		}
		if op := waitOp(resp.Msg.GetOperation().GetName()); op.GetError() != nil {
			t.Fatalf("apply: %v", op.GetError())
		}
	}
	ns := service + "-staging"
	t.Cleanup(func() { _ = kube.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{}) })

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
