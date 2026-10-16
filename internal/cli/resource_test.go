package cli

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/policy"
)

// serveResources restarts the harness API with resources on a fake runner.
func (h *harness) serveResources() *envtest.Runner {
	h.t.Helper()
	pol, err := policy.New(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	runner := &envtest.Runner{}
	h.resources, err = server.NewResources(server.ResourceOptions{
		Store: h.audit, Catalog: h.catalog, Operations: h.ops, Policy: pol, Launcher: runner,
		Poll: 2 * time.Millisecond, Grace: 30 * time.Millisecond,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	runner.API = h.resources
	h.t.Cleanup(runner.Close)
	h.startAPI(h.authn)
	return runner
}

// manifestWith writes forge.yaml for service payments with resources.
func (h *harness) manifestWith(resources string) {
	h.t.Helper()
	body := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n" + resources
	if err := os.WriteFile(filepath.Join(h.dir, "forge.yaml"), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

const (
	database5Gi = "resources:\n  - name: main\n    type: postgres\n    storage: 5Gi\n"
	noDatabase  = ""
)

var planNameRE = regexp.MustCompile(`resourcePlans/[0-9a-f]{12}`)

// scrub replaces plan names, which are random, with a fixed one.
func scrub(s string) string { return planNameRE.ReplaceAllString(s, "resourcePlans/PLAN") }

func TestResourceLifecycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.serveResources()
	h.ok("service", "register", "payments", "--team", "billing")

	if r := h.fails(2, "resource", "plan", "--env", "staging"); !strings.Contains(r.err, "no forge.yaml here") {
		t.Errorf("no manifest: %v", r)
	}
	h.manifestWith(database5Gi)
	h.fails(2, "resource", "plan")                // --env is required
	h.fails(2, "resource", "plan", "--env", "qa") // the API refuses it

	if r := h.ok("resource", "list"); r.out != "" || !strings.Contains(r.err, "No resources.") {
		t.Errorf("empty list: %v", r)
	}
	if r := h.ok("resource", "list", "-o", "json"); !strings.Contains(r.out, `"resources": []`) {
		t.Errorf("empty list as JSON: %s", r.out)
	}

	r := h.ok("resource", "plan", "--env", "staging")
	golden(t, "resource_plan_table", scrub(r.out))
	for _, step := range []string{"launch", "init", "plan", "policy"} {
		if !strings.Contains(r.err, "  "+step) {
			t.Errorf("no %s event:\n%s", step, r.err)
		}
	}
	plan := planNameRE.FindString(r.out)
	if plan == "" {
		t.Fatalf("no plan name in:\n%s", r.out)
	}
	if len(runner.Databases()) != 0 {
		t.Errorf("a plan made a database: %v", runner.Databases())
	}

	if r := h.ok("resource", "apply", plan, "--dry-run"); scrub(r.out) != "Would apply resourcePlans/PLAN to payments (staging)\n" {
		t.Errorf("dry run: %q", r.out)
	}
	// Without a terminal and without --yes, nothing is applied.
	if r := h.fails(2, "resource", "apply", plan); !strings.Contains(r.err, "--yes") {
		t.Errorf("no confirmation: %v", r)
	}
	h.fails(2, "resource", "apply")                              // a plan or --env
	h.fails(2, "resource", "apply", plan, "--env", "staging")    // not both
	h.fails(2, "resource", "apply", "nope", "--yes")             // not a plan name
	h.fails(3, "resource", "apply", "0123456789ab", "--yes")     // no such plan
	h.fails(3, "resource", "apply", "--env", "production", "-y") // no plan of mine there

	r = h.ok("resource", "apply", "--env", "staging", "--yes")
	if scrub(r.out) != "Applied resourcePlans/PLAN to payments (staging)\n" || !strings.Contains(r.err, "Deploy to give the app DATABASE_URL.") {
		t.Errorf("apply: %v", r)
	}
	if got := runner.Databases(); !slices.Equal(got, []string{"payments-staging/main 5Gi x1"}) {
		t.Errorf("databases: %v", got)
	}

	golden(t, "resource_list_table", h.ok("resource", "list").out)
	if r := h.ok("resource", "list", "payments", "--env", "staging", "-o", "name"); r.out != "resources/payments/staging/main\n" {
		t.Errorf("list -o name: %q", r.out)
	}
	if r := h.ok("resource", "list", "--env", "production"); !strings.Contains(r.err, "No resources.") {
		t.Errorf("production: %v", r)
	}
	r = h.ok("resource", "get", "payments/staging/main")
	for _, want := range []string{"resources/payments/staging/main", "ready", "main-app", "5Gi", plan, "unknown"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("get lacks %q:\n%s", want, r.out)
		}
	}
	if r := h.ok("resource", "get", "resources/payments/staging/main", "-o", "json"); !strings.Contains(r.out, `"state": "STATE_READY"`) {
		t.Errorf("get as JSON: %s", r.out)
	}
	h.fails(3, "resource", "get", "payments/staging/nope")
	h.fails(2, "resource", "get", "nope")

	// A plan is used once (exit 6), and planning again finds nothing to do.
	if r := h.fails(6, "resource", "apply", plan, "--yes"); !strings.Contains(r.err, "was already applied") {
		t.Errorf("a second apply: %v", r)
	}
	r = h.ok("resource", "plan", "--env", "staging")
	if !strings.Contains(r.out, "  No changes\n") || strings.Contains(r.out, "Apply it with") {
		t.Errorf("an unchanged plan:\n%s", r.out)
	}
	if r := h.fails(6, "resource", "apply", "--env", "staging", "--yes"); !strings.Contains(r.err, "nothing to apply") {
		t.Errorf("applying no changes: %v", r)
	}

	// forge.yaml changed after the plan: apply refuses.
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "10Gi", 1))
	r = h.ok("resource", "plan", "--env", "staging", "-o", "json")
	if !strings.Contains(r.out, `"state": "STATE_PLANNED"`) || !strings.Contains(r.out, `"action": "update"`) {
		t.Errorf("plan as JSON: %s", r.out)
	}
	grow := planNameRE.FindString(r.out)
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "12Gi", 1))
	if r := h.fails(6, "resource", "apply", grow, "--yes"); !strings.Contains(r.err, "forge.yaml changed since") {
		t.Errorf("a changed manifest: %v", r)
	}
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "10Gi", 1))
	r = h.ok("resource", "apply", grow, "--yes", "--wait=false")
	if !strings.Contains(r.err, "forge op watch") {
		t.Errorf("--wait=false: %v", r)
	}
}

func TestResourcePlanDenied(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveResources()
	h.ok("service", "register", "payments", "--team", "billing")
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "200Gi", 1))
	r := h.fails(5, "resource", "plan", "--env", "production")
	golden(t, "resource_plan_denied", scrub(r.out))
	if !strings.Contains(r.err, "was denied by policy") {
		t.Errorf("stderr: %s", r.err)
	}
	plan := planNameRE.FindString(r.out)
	if r := h.fails(6, "resource", "apply", plan, "--yes"); !strings.Contains(r.err, "was denied by policy") {
		t.Errorf("applying a denied plan: %v", r)
	}
	// A warning is shown and does not fail the command.
	h.manifestWith(database5Gi + "    instances: 2\n")
	if r := h.ok("resource", "plan", "--env", "staging"); !strings.Contains(r.out, "  ! staging has 2 instances; 1 is usually enough\nPolicy: passed\n") {
		t.Errorf("a warning:\n%s", r.out)
	}
}

func TestResourceDestroy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.serveResources()
	h.ok("service", "register", "payments", "--team", "billing")
	h.manifestWith(database5Gi)
	h.ok("resource", "plan", "--env", "staging")
	h.ok("resource", "apply", "--env", "staging", "--yes")

	// The database left forge.yaml: refused unless asked for.
	h.manifestWith(noDatabase)
	if r := h.fails(5, "resource", "plan", "--env", "staging"); !strings.Contains(r.out, "✗ the plan deletes database main; plan again with --allow-destroy to remove it") {
		t.Errorf("a delete without --allow-destroy:\n%s", r.out)
	}
	r := h.ok("resource", "plan", "--env", "staging", "--allow-destroy")
	if !strings.Contains(r.out, "  - postgres main   storage 5Gi, instances 1\n") {
		t.Errorf("destroy plan:\n%s", r.out)
	}

	// At a terminal, apply asks for the resource's name.
	h.tty = true
	h.stdin = "nope\n"
	if r := h.fails(2, "resource", "apply", "--env", "staging"); !strings.Contains(r.err, "Type its name to confirm") || !strings.Contains(r.err, "did not match") {
		t.Errorf("a wrong name: %v", r)
	}
	if len(runner.Databases()) != 1 {
		t.Fatal("the database went without confirmation")
	}
	h.stdin = "main\n"
	r = h.ok("resource", "apply", "--env", "staging")
	if !strings.Contains(r.err, "The next deploy drops DATABASE_URL.") {
		t.Errorf("destroy: %v", r)
	}
	if len(runner.Databases()) != 0 {
		t.Errorf("databases: %v", runner.Databases())
	}
	if r := h.ok("resource", "list"); !strings.Contains(r.err, "No resources.") {
		t.Errorf("after the destroy: %v", r)
	}
}

func TestResourceApplyAsksYesOrNo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	runner := h.serveResources()
	h.ok("service", "register", "payments", "--team", "billing")
	h.manifestWith(database5Gi)
	h.ok("resource", "plan", "--env", "staging")
	h.tty = true
	h.stdin = "n\n"
	r := h.fails(2, "resource", "apply", "--env", "staging")
	// The plan is shown before the question.
	if !strings.Contains(r.err, "+ postgres main") || !strings.Contains(r.err, "Apply this plan? [y/N]") || len(runner.Databases()) != 0 {
		t.Errorf("answering no: %v", r)
	}
	h.stdin = "y\n"
	h.ok("resource", "apply", "--env", "staging")
	if len(runner.Databases()) != 1 {
		t.Errorf("answering yes: %v", runner.Databases())
	}
}

func TestResourceAccessAndOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.ok("service", "register", "payments", "--team", "billing")
	h.manifestWith(database5Gi)
	// Resources are off on this API: exit 6 with the reason.
	if r := h.fails(6, "resource", "list"); !strings.Contains(r.err, "resources are not configured on this Forge API") {
		t.Errorf("resources off: %v", r)
	}
	h.fails(6, "resource", "plan", "--env", "staging")

	h.serveResources()
	h.env["FORGE_TOKEN"] = readerToken
	if r := h.fails(5, "resource", "plan", "--env", "staging"); !strings.Contains(r.err, "resource.plan") {
		t.Errorf("a reader plans: %v", r)
	}
	h.ok("resource", "list")

	// -f reads another manifest.
	h.env["FORGE_TOKEN"] = writerToken
	other := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(other, []byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"+strings.Replace(database5Gi, "5Gi", "7Gi", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := h.ok("resource", "plan", "--env", "staging", "-f", other); !strings.Contains(r.out, "storage 7Gi") {
		t.Errorf("-f: %s", r.out)
	}
	h.fails(2, "resource", "plan", "--env", "staging", "-f", filepath.Join(t.TempDir(), "missing.yaml"))
	// A manifest the CLI itself can refuse never reaches the API.
	h.manifestWith(strings.Replace(database5Gi, "5Gi", "5GB", 1))
	if r := h.fails(2, "resource", "plan", "--env", "staging"); !strings.Contains(r.err, "resources: main: storage") {
		t.Errorf("a bad manifest: %v", r)
	}
}
