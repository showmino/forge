package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

// setStaging overwrites what Argo CD says about payments-staging. The fake
// Argo CD writes the status again only when the deploy repo's head moves.
func (f *deployFixture) setStaging(t *testing.T, sync, health string, revisions ...any) {
	t.Helper()
	ctx := context.Background()
	apps := f.cluster.Dynamic.Resource(envtest.Applications).Namespace("argocd")
	app, err := apps.Get(ctx, "payments-staging", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := app.Object["status"].(map[string]any)
	st["sync"].(map[string]any)["status"] = sync
	if revisions != nil {
		st["sync"].(map[string]any)["revisions"] = revisions
	}
	st["health"] = map[string]any{"status": health}
	if _, err := apps.Update(ctx, app, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestPromotionPolicy(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	ctx := context.Background()
	f.deployOK(t, "staging")
	f.deployOK(t, "staging")
	// A deploy to production moves the deploy repo's head: Argo CD then
	// reports that commit for staging too, whose values are unchanged.
	f.deployOK(t, "production")
	head, _, err := f.repo.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "staging at the new head", func() bool {
		app, err := f.cluster.Dynamic.Resource(envtest.Applications).Namespace("argocd").Get(ctx, "payments-staging", metav1.GetOptions{})
		if err != nil {
			return false
		}
		revs, _, _ := unstructured.NestedStringSlice(app.Object, "status", "sync", "revisions")
		return slices.Contains(revs, head)
	})
	promote := func(validateOnly bool) error {
		_, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments", ValidateOnly: validateOnly}))
		return err
	}
	if err := promote(true); err != nil {
		t.Errorf("synced to a later head with the same values: %v", err)
	}
	refused := func(name, want string) {
		t.Helper()
		for _, dry := range []bool{true, false} {
			err := promote(dry)
			if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), want) {
				t.Errorf("%s (dry run %v): %v, want permission denied with %q", name, dry, err, want)
			}
		}
	}

	f.setStaging(t, "Synced", "Degraded")
	refused("degraded", "staging is Degraded; it must be Healthy to be promoted")
	f.setStaging(t, "OutOfSync", "Healthy")
	refused("out of sync", "staging is OutOfSync; it must be Synced to be promoted")
	hist, err := f.client.ListDeployHistory(ctx, as("reader", &forgev1.ListDeployHistoryRequest{Name: "deployments/payments/staging"}))
	if err != nil {
		t.Fatal(err)
	}
	latest := hist.Msg.GetRecords()[0].GetDeployCommit()
	f.setStaging(t, "Synced", "Healthy", "chart", "0000000000000000000000000000000000000000")
	refused("not at the latest deploy", "staging does not run its latest deploy ("+shortSHA(latest)+") yet")

	// A rollback is never gated.
	f.setStaging(t, "Synced", "Degraded")
	rb, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging", ValidateOnly: true}))
	if err != nil {
		t.Errorf("rollback of a degraded staging: %v %v", rb, err)
	}

	f.setStaging(t, "Synced", "Healthy", "chart", latest)
	if err := promote(false); err != nil {
		t.Fatalf("healthy staging: %v", err)
	}
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
