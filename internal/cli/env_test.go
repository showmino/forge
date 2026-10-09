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

const cliSHA = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"

// servePreviews restarts the harness API with previews on fakes; services
// must be registered with --repo https://github.com/showmino/<name>.
func (h *harness) servePreviews(available bool) *server.Cluster {
	h.t.Helper()
	cluster := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
	images := &envtest.Images{}
	images.Add("ghcr.io/showmino/payments:1a2b3c4")
	h.envs = server.NewEnvironments(server.EnvOptions{
		Cluster: cluster, Images: images, Catalog: h.catalog, Operations: h.ops,
		GitHub: &envtest.GitHub{Refs: map[string]string{"main": cliSHA}},
		Org:    "showmino", Domain: "preview.example",
		ImagePoll: 5 * time.Millisecond, Poll: 5 * time.Millisecond,
		ImageTimeout: 5 * time.Second, ReadyTimeout: 300 * time.Millisecond, DestroyTimeout: 5 * time.Second,
	})
	h.startAPI(h.authn)
	if available {
		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup
		wg.Go(func() { envtest.MarkAvailable(ctx, cluster.Kube, nil) })
		h.t.Cleanup(func() { cancel(); wg.Wait() })
	}
	return cluster
}

func TestEnvLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.servePreviews(true)
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")

	r := h.ok("env", "create", "pr-1", "--service", "payments", "--dry-run")
	if !strings.HasPrefix(r.out, "Would create environments/pr-1 for services/payments at main (1a2b3c4), expiring in 24h:\n") ||
		!strings.Contains(r.out, "  url:   http://pr-1.preview.example\n") || !strings.Contains(r.out, "  image: ghcr.io/showmino/payments:1a2b3c4\n") {
		t.Errorf("dry run:\n%s", r.out)
	}
	h.fails(3, "env", "get", "pr-1") // the dry run created nothing

	r = h.ok("env", "create", "pr-1", "--service", "payments")
	for _, step := range []string{"resolve", "image", "namespace", "apply", "ready"} {
		if !strings.Contains(r.err, "  "+step) {
			t.Errorf("no %s event:\n%s", step, r.err)
		}
	}
	if !strings.Contains(r.out, "state       ready") || !strings.Contains(r.out, "url         http://pr-1.preview.example") {
		t.Errorf("printed environment:\n%s", r.out)
	}
	h.fails(6, "env", "create", "pr-1", "--service", "payments") // exists

	golden(t, "env_list_table", h.ok("env", "list").out)
	if r := h.ok("env", "list", "-o", "json"); !strings.Contains(r.out, `"name": "environments/pr-1"`) || !strings.Contains(r.out, `"state": "STATE_READY"`) {
		t.Errorf("list json:\n%s", r.out)
	}
	get := h.ok("env", "get", "pr-1").out
	head, _, _ := strings.Cut(get, "expires")
	golden(t, "env_get_table", head) // expiry and operation id vary
	h.ok("env", "get", "environments/pr-1", "-o", "name")

	if r := h.ok("env", "extend", "pr-1", "--ttl", "48h"); !strings.HasPrefix(r.out, "environments/pr-1 expires ") || !strings.HasSuffix(r.out, " (in 47h)\n") {
		t.Errorf("extend: %q", r.out)
	}
	h.fails(2, "env", "extend", "pr-1")                  // --ttl required
	h.fails(2, "env", "extend", "pr-1", "--ttl", "200h") // over 168h

	if r := h.ok("env", "destroy", "pr-1", "--dry-run"); r.out != "Would destroy environments/pr-1 (namespace preview-pr-1)\n" {
		t.Errorf("destroy dry run: %q", r.out)
	}
	h.fails(2, "env", "destroy", "pr-1") // no --yes, no terminal
	if r := h.ok("env", "destroy", "pr-1", "--yes"); r.out != "environments/pr-1 destroyed\n" || !strings.Contains(r.err, "  destroy") {
		t.Errorf("destroy: %v", r)
	}
	h.fails(3, "env", "get", "pr-1")
	if r := h.ok("env", "list"); !strings.Contains(r.err, "No environments.") {
		t.Errorf("empty list: %v", r)
	}
	if r := h.ok("env", "list", "-o", "json"); !strings.Contains(r.out, `"environments": []`) {
		t.Errorf("empty json: %q", r.out)
	}
}

func TestEnvCreateNoWaitAndFailures(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.servePreviews(false) // never ready: creates fail not_ready after 300ms
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")

	r := h.ok("env", "create", "a", "--service", "payments", "--wait=false")
	if !strings.HasPrefix(r.out, "operations/op_") || !strings.Contains(r.err, "Watch it with: forge op watch op_") {
		t.Errorf("--wait=false: %v", r)
	}
	r = h.fails(7, "env", "create", "b", "--service", "payments")
	if !strings.Contains(r.err, "(not_ready)") || !strings.Contains(r.err, "hint: see 'forge env get b'") {
		t.Errorf("not ready: %s", r.err)
	}
	h.fails(2, "env", "create", "Bad_Name", "--service", "payments")
	h.fails(2, "env", "create", "c") // no --service outside a repo
	h.fails(2, "env", "create", "c", "--service", "payments", "--ttl", "30m")
	h.fails(3, "env", "create", "c", "--service", "nope")
	h.fails(3, "env", "create", "c", "--service", "payments", "--ref", "nope")
}

func TestEnvServiceFromManifest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.servePreviews(true)
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	h.ok("init", "--team", "billing", "--name", "payments")
	if r := h.ok("env", "create", "m", "--dry-run"); !strings.Contains(r.out, "for services/payments") {
		t.Errorf("service from forge.yaml: %q", r.out)
	}
}

func TestEnvPreviewsOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if r := h.fails(6, "env", "list"); !strings.Contains(r.err, "previews are not configured") {
		t.Errorf("off: %s", r.err)
	}
}

func TestUntil(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		-time.Second: "expired", 0: "expired", 30 * time.Second: "in <1m", 5 * time.Minute: "in 5m",
		23*time.Hour + 59*time.Minute: "in 23h", 50 * time.Hour: "in 2d",
	} {
		if got := until(d); got != want {
			t.Errorf("until(%v) = %q, want %q", d, got, want)
		}
	}
}
