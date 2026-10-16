package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/policy"
)

// serveDeploys restarts the harness API with deploys on fakes and Argo
// playing along; unhealthy names "<svc>-<env>" Argo reports Degraded.
func (h *harness) serveDeploys(unhealthy map[string]string) *envtest.Repo {
	h.t.Helper()
	cluster := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
	images := &envtest.Images{}
	images.Add("ghcr.io/showmino/payments:1a2b3c4")
	repo := &envtest.Repo{}
	rules, err := policy.New(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	h.deploys = server.NewDeploys(server.DeployOptions{
		Policy:  rules,
		Cluster: cluster, Images: images, Repo: repo, Catalog: h.catalog, Operations: h.ops,
		GitHub:   &envtest.GitHub{Refs: map[string]string{"main": cliSHA}},
		Releases: &envtest.Releases{},
		Org:      "showmino", Domain: "10.150.1.222.nip.io",
		PublicDomains: h.publicDomains, HostTaken: h.hostTaken,
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, SyncTimeout: 300 * time.Millisecond, NamespaceTimeout: 5 * time.Second,
	})
	// As forge-api wires it: deleting a service takes its deployments down first.
	h.catalog.EnableTeardown(h.deploys, h.envs, h.ops)
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

	if r := h.ok("deploy", "start", "payments", "--env", "staging", "--dry-run"); r.out != "Would deploy ghcr.io/showmino/payments:1a2b3c4 (main at 1a2b3c4) to deployments/payments/staging at http://payments.staging.10.150.1.222.nip.io as v0.1.0-rc.1\n" {
		t.Errorf("dry run: %q", r.out)
	}
	r := h.ok("deploy", "start", "payments", "--env", "staging")
	for _, step := range []string{"resolve", "image", "release", "commit", "namespace", "sync"} {
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
	h.fails(2, "deploy", "promote", "payments", "--yes", "--bump", "huge")
	if r := h.ok("deploy", "promote", "payments", "--dry-run"); r.out != "Would promote ghcr.io/showmino/payments:1a2b3c4 from staging to production as v0.1.0\n" {
		t.Errorf("promote dry run: %q", r.out)
	}
	if r := h.ok("deploy", "promote", "payments", "--dry-run", "--bump", "minor"); !strings.HasSuffix(r.out, " as v0.1.0\n") {
		t.Errorf("the first release is v0.1.0 for a patch and a minor alike: %q", r.out)
	}
	if r := h.ok("deploy", "promote", "payments", "--dry-run", "--bump", "major"); !strings.HasSuffix(r.out, " as v1.0.0\n") {
		t.Errorf("promote dry run, major: %q", r.out)
	}
	if r := h.ok("deploy", "promote", "payments", "--yes"); !strings.Contains(r.out, "v0.1.0") {
		t.Errorf("promote: %q", r.out)
	}
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
	// A degraded staging is not promoted, with the reason; a rollback still goes.
	if r := h.fails(5, "deploy", "promote", "payments", "--dry-run"); !strings.Contains(r.err, "staging is Degraded; it must be Healthy to be promoted") {
		t.Errorf("promote of a degraded staging:\n%s", r.err)
	}
	h.fails(7, "deploy", "start", "payments", "--env", "staging")
	h.ok("deploy", "rollback", "payments", "--env", "staging", "--dry-run")
	h.ok("init", "--team", "billing", "--name", "payments")
	if r := h.ok("deploy", "start", "--env", "staging", "--dry-run"); !strings.Contains(r.out, "deployments/payments/staging") {
		t.Errorf("service from forge.yaml: %q", r.out)
	}
}

// Deleting a service that is deployed takes it down first, and shows the work.
func TestServiceDeleteTakesDeploymentsDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	repo := h.serveDeploys(map[string]string{})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	h.ok("deploy", "start", "payments", "--env", "staging")
	if _, err := repo.Read(context.Background(), "services/payments/staging/values.yaml", ""); err != nil {
		t.Fatalf("not deployed: %v", err)
	}

	// A dry run changes nothing, and says what a delete would take down.
	dry := h.ok("service", "delete", "payments", "--dry-run")
	for _, want := range []string{"Deleting services/payments takes down:", "  deployments/payments/staging", "Its GitHub repo is kept.", "nothing changed"} {
		if !strings.Contains(dry.err, want) {
			t.Errorf("dry run lacks %q:\n%s", want, dry.err)
		}
	}
	if _, err := repo.Read(context.Background(), "services/payments/staging/values.yaml", ""); err != nil {
		t.Fatalf("the dry run removed the deployment: %v", err)
	}

	r := h.ok("service", "delete", "payments", "--yes")
	for _, want := range []string{"Deleting services/payments (operations/op_", "removed payments from staging", "Argo CD removed payments-staging", "removed payments-staging", "its GitHub repo is kept", "services/payments deleted."} {
		if !strings.Contains(r.err, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.err)
		}
	}
	if _, err := repo.Read(context.Background(), "services/payments/staging/values.yaml", ""); err == nil {
		t.Error("the deployment is still in the deploy repo")
	}
	h.fails(3, "service", "get", "payments")
}
