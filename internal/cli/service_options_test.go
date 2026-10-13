package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/exitcode"
)

func TestServiceOptionsCheck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	write := func(name, content string) string {
		t.Helper()
		dir := filepath.Join(h.dir, name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	base := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"
	good := write("good", base+`run:
  replicas: 2
  resources: {requests: {cpu: 100m}, limits: {cpu: 500m, memory: 512Mi}}
  env: {LOG_LEVEL: info, REGION: eu}
  secrets: [{name: stripe, item: vaults/Apps/items/stripe, env: {STRIPE_KEY: credential}}]
  jobs: [{name: cleanup, schedule: "0 3 * * *", command: [node, cleanup.js]}]
  autoscaling: {max: 4}
`)
	r := h.ok("service", "options", "check", good)
	golden(t, "service_options_check", r.out)
	if !strings.Contains(r.err, "payments's run options are valid") {
		t.Errorf("stderr: %q", r.err)
	}
	if r := h.ok("service", "options", "check", good, "-o", "json"); !strings.Contains(r.out, `"maxAutoscaling": 4`) || !strings.Contains(r.out, `"item": "vaults/Apps/items/stripe"`) {
		t.Errorf("json:\n%s", r.out)
	}
	if r := h.ok("service", "options", "check", write("plain", base)); !strings.Contains(r.out, "replicas") {
		t.Errorf("without options:\n%s", r.out)
	}
	if r := h.fails(int(exitcode.Unexpected), "service", "options", "check", write("bad", base+"run:\n  replicas: 11\n")); !strings.Contains(r.err, "run.replicas 11 must be 1 to 10") {
		t.Errorf("bad: %q", r.err)
	}
	h.fails(int(exitcode.Usage), "service", "options", "check", filepath.Join(h.dir, "nope"))
}
