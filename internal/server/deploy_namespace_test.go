package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

func TestDeployRefusesForeignNamespace(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	// A namespace someone else made, with the name Forge would use.
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-staging", Labels: map[string]string{"team": "docs"}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, validateOnly := range []bool{true, false} {
		_, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", ValidateOnly: validateOnly}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "payments-staging") {
			t.Errorf("validate_only=%v: %v (want FailedPrecondition naming the namespace)", validateOnly, err)
		}
	}
	if hist, err := f.repo.History(ctx, "services/payments/staging/values.yaml", 1); err != nil || len(hist) != 0 {
		t.Errorf("deploy repo history %v, %v: want nothing committed", hist, err)
	}
	// Only the foreign namespace is refused: production is free.
	if _, err := f.start(t, "production"); err != nil {
		t.Fatalf("production: %v", err)
	}
}

func TestPromoteRefusesForeignNamespace(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	resp, err := f.start(t, "staging")
	if err != nil {
		t.Fatal(err)
	}
	waitCIOp(t, f.ops, resp.GetOperation().GetName())
	if _, err := f.cluster.Kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "payments-production"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "payments-production") {
		t.Errorf("promote into a foreign namespace: %v", err)
	}
}

func TestDeployRefusesForeignApplication(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	// An Application someone else made, with the name Forge would use.
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "payments-staging", "namespace": "argocd"},
	}}
	if _, err := f.cluster.Dynamic.Resource(envtest.Applications).Namespace("argocd").Create(ctx, app, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, validateOnly := range []bool{true, false} {
		_, err := f.client.StartDeploy(ctx, as("writer", &forgev1.StartDeployRequest{Service: "services/payments", Env: "staging", ValidateOnly: validateOnly}))
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "argocd/payments-staging") {
			t.Errorf("validate_only=%v: %v (want FailedPrecondition naming the Application)", validateOnly, err)
		}
	}
	if hist, err := f.repo.History(ctx, "services/payments/staging/values.yaml", 1); err != nil || len(hist) != 0 {
		t.Errorf("deploy repo history %v, %v: want nothing committed", hist, err)
	}
	// Applications the fake Argo makes are Forge's, so a repeat deploy is fine.
	for i := range 2 {
		resp, err := f.start(t, "production")
		if err != nil {
			t.Fatalf("production deploy %d: %v", i, err)
		}
		waitCIOp(t, f.ops, resp.GetOperation().GetName())
	}
}
