package server

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/showmino/forge/internal/envtest"
)

func serviceNS(name, svc, env string, forge bool) *corev1.Namespace {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
	if forge {
		ns.Labels = map[string]string{labelManagedBy: "forge", labelService: svc, labelDeployEnv: env}
	}
	return ns
}

func TestWritePullSecretGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster(
		serviceNS("payments-staging", "payments", "staging", true),
		serviceNS("ledger-staging", "ledger", "staging", false), // made by hand
		serviceNS("billing-production", "other", "production", true),
	)
	if err := c.WriteServicePullSecret(ctx, "payments", "staging"); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteServicePullSecret(ctx, "payments", "staging"); err != nil {
		t.Errorf("second write must be a no-op: %v", err)
	}
	s, err := c.Kube.CoreV1().Secrets("payments-staging").Get(ctx, "forge-pull", metav1.GetOptions{})
	if err != nil || s.Type != corev1.SecretTypeDockerConfigJson || string(s.Data[corev1.DockerConfigJsonKey]) != string(envtest.PullSecretJSON) {
		t.Errorf("secret: %v %v", s, err)
	}
	for _, tc := range []struct{ svc, env string }{{"ledger", "staging"}, {"billing", "production"}, {"nope", "staging"}, {"payments", "kube"}} {
		if err := c.WriteServicePullSecret(ctx, tc.svc, tc.env); !errors.Is(err, errNoEnv) {
			t.Errorf("%s-%s: %v, want errNoEnv", tc.svc, tc.env, err)
		}
	}
	if _, err := c.Kube.CoreV1().Secrets("ledger-staging").Get(ctx, "forge-pull", metav1.GetOptions{}); err == nil {
		t.Error("wrote into a namespace Forge did not make")
	}

	wctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := c.WaitServiceNamespace(wctx, "ledger", "staging", 5*time.Millisecond); err == nil {
		t.Error("waited successfully for an unlabelled namespace")
	}
	if err := c.WaitServiceNamespace(ctx, "payments", "staging", 5*time.Millisecond); err != nil {
		t.Errorf("wait: %v", err)
	}
}

func TestApplicationStatus(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := testCluster()
	st, err := c.Application(ctx, "argocd", "payments-staging")
	if err != nil || st.Health != "Missing" || st.Sync != "Unknown" {
		t.Errorf("missing: %+v %v", st, err)
	}
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "payments-staging", "namespace": "argocd"},
		"status": map[string]any{
			"sync":       map[string]any{"status": "Synced", "revisions": []any{"chartrev", "deploy1"}},
			"health":     map[string]any{"status": "Degraded", "message": "back-off pulling image"},
			"conditions": []any{map[string]any{"type": "SyncError", "message": "one or more objects failed"}},
		},
	}}
	if _, err := c.Dynamic.Resource(envtest.Applications).Namespace("argocd").Create(ctx, app, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	st, err = c.Application(ctx, "argocd", "payments-staging")
	if err != nil || st.Sync != "Synced" || st.Health != "Degraded" || len(st.Revisions) != 2 || st.Revisions[1] != "deploy1" ||
		st.Message != "one or more objects failed; back-off pulling image" {
		t.Errorf("status: %+v %v", st, err)
	}
}
