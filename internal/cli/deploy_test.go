package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/server"
)

// serveDeploys restarts the harness API with deploys on fakes and Argo
// playing along; unhealthy names "<svc>-<env>" Argo reports Degraded.
func (h *harness) serveDeploys(unhealthy map[string]string) *envtest.Repo {
	h.t.Helper()
	cluster := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
	images := &envtest.Images{}
	images.Add("ghcr.io/showmino/payments:1a2b3c4")
	repo := &envtest.Repo{}
	h.deploys = server.NewDeploys(server.DeployOptions{
		Cluster: cluster, Images: images, Repo: repo, Catalog: h.catalog, Operations: h.ops,
		GitHub: &envtest.GitHub{Refs: map[string]string{"main": cliSHA}},
		Org:    "showmino", Domain: "10.150.1.222.nip.io",
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, SyncTimeout: 300 * time.Millisecond, NamespaceTimeout: 5 * time.Second,
	})
	h.startAPI(h.authn)
	argo := &envtest.Argo{Kube: cluster.Kube, Dynamic: cluster.Dynamic, Repo: repo, Namespace: "argocd", Unhealthy: unhealthy}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { argo.Run(ctx) })
	h.t.Cleanup(func() { cancel(); wg.Wait() })
	return repo
}

func TestDeployLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveDeploys(map[string]string{})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")

	h.fails(2, "deploy", "start", "payments") // --env required
	h.fails(2, "deploy", "start", "payments", "--env", "qa")
	h.fails(2, "deploy", "start", "--env", "staging") // no service outside a repo
	h.fails(3, "deploy", "status", "payments", "--env", "staging")

	if r := h.ok("deploy", "start", "payments", "--env", "staging", "--dry-run"); r.out != "Would deploy ghcr.io/showmino/payments:1a2b3c4 (main at 1a2b3c4) to deployments/payments/staging at http://payments.staging.10.150.1.222.nip.io\n" {
		t.Errorf("dry run: %q", r.out)
	}
	r := h.ok("deploy", "start", "payments", "--env", "staging")
	for _, step := range []string{"resolve", "image", "commit", "namespace", "sync"} {
		if !strings.Contains(r.err, "  "+step) {
			t.Errorf("no %s event:\n%s", step, r.err)
		}
	}
	if !strings.Contains(r.out, "health          healthy") {
		t.Errorf("printed deployment:\n%s", r.out)
	}
	h.ok("deploy", "start", "payments", "--env", "staging")

	status, _, ok := strings.Cut(h.ok("deploy", "status").out, "ana@example.com") // the age varies before the actor
	if !ok {
		t.Fatalf("no actor in status:\n%s", status)
	}
	golden(t, "deploy_status_table", status)
	hist := h.ok("deploy", "history", "payments", "--env", "staging").out
	if lines := strings.Split(strings.TrimSpace(hist), "\n"); len(lines) != 3 || !strings.Contains(lines[1], "deploy") || !strings.Contains(lines[1], "1a2b3c4") {
		t.Errorf("history:\n%s", hist)
	}

	h.fails(2, "deploy", "rollback", "payments", "--env", "staging") // no --yes, no terminal
	if r := h.ok("deploy", "rollback", "payments", "--env", "staging", "--dry-run"); r.out != "Would roll back deployments/payments/staging to ghcr.io/showmino/payments:1a2b3c4\n" {
		t.Errorf("rollback dry run: %q", r.out)
	}
	h.ok("deploy", "rollback", "payments", "--env", "staging", "--yes")
	if r := h.ok("deploy", "history", "payments", "--env", "staging", "--limit", "1"); !strings.Contains(r.out, "rollback") {
		t.Errorf("after rollback:\n%s", r.out)
	}

	h.fails(2, "deploy", "promote", "payments") // to production: needs --yes
	if r := h.ok("deploy", "promote", "payments", "--dry-run"); r.out != "Would promote ghcr.io/showmino/payments:1a2b3c4 from staging to production\n" {
		t.Errorf("promote dry run: %q", r.out)
	}
	h.ok("deploy", "promote", "payments", "--yes")
	if r := h.ok("deploy", "status", "payments", "--env", "production", "-o", "json"); !strings.Contains(r.out, `"env": "production"`) || !strings.Contains(r.out, `"health": "HEALTH_HEALTHY"`) {
		t.Errorf("production json:\n%s", r.out)
	}
}

func TestDeployFailureAndOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if r := h.fails(6, "deploy", "status"); !strings.Contains(r.err, "deploys are not configured") {
		t.Errorf("off: %s", r.err)
	}
	h.serveDeploys(map[string]string{"payments-staging": "ImagePullBackOff"})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	r := h.fails(7, "deploy", "start", "payments", "--env", "staging")
	if !strings.Contains(r.err, "(not_healthy)") || !strings.Contains(r.err, "forge deploy rollback payments --env staging") {
		t.Errorf("not healthy:\n%s", r.err)
	}
	h.ok("init", "--team", "billing", "--name", "payments")
	if r := h.ok("deploy", "start", "--env", "staging", "--dry-run"); !strings.Contains(r.out, "deployments/payments/staging") {
		t.Errorf("service from forge.yaml: %q", r.out)
	}
}
