package cli

import (
	"context"
	"strings"
	"testing"
)

// forge service restart and scale: a dry run, the real thing, what a reader
// may not do, and a scale undone by the next deploy.
func TestServiceRestartAndScale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	repo := h.serveDeploys(map[string]string{})
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	h.ok("deploy", "start", "payments", "--env", "staging")

	h.fails(2, "service", "restart", "payments")                                       // --env required
	h.fails(2, "service", "scale", "payments", "--env", "staging")                     // --replicas required
	h.fails(2, "service", "scale", "payments", "--env", "staging", "--replicas", "11") // out of range

	if r := h.ok("service", "restart", "payments", "--env", "staging", "--dry-run"); r.out != "Would restart the 1 pod of deployments/payments/staging (ghcr.io/showmino/payments:1a2b3c4)\n" {
		t.Errorf("restart dry run: %q", r.out)
	}
	r := h.ok("service", "restart", "payments", "--env", "staging")
	if !strings.Contains(r.err, "deployments/payments/staging restarted: its new pods are healthy.") {
		t.Errorf("restart:\n%s", r.err)
	}
	values := func() string {
		data, err := repo.Read(context.Background(), "services/payments/staging/values.yaml", "")
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	if !strings.Contains(values(), "restartedAt: ") {
		t.Errorf("no restart mark:\n%s", values())
	}

	if r := h.ok("service", "scale", "payments", "--env", "staging", "--replicas", "3", "--dry-run"); r.out != "Would scale deployments/payments/staging from 1 pod to 3 pods\n" {
		t.Errorf("scale dry run: %q", r.out)
	}
	r = h.ok("service", "scale", "payments", "--env", "staging", "--replicas", "3")
	if !strings.Contains(r.err, "deployments/payments/staging runs 3 pods (was 1) until the next deploy, which runs 1 pod again.") {
		t.Errorf("scale:\n%s", r.err)
	}
	if !strings.Contains(values(), "replicas: 3\n") {
		t.Errorf("not scaled:\n%s", values())
	}
	r = h.ok("service", "scale", "payments", "--env", "staging", "--replicas", "2", "--wait=false")
	if !strings.HasPrefix(r.out, "operations/") {
		t.Fatalf("scale without waiting: %q", r.out)
	}
	h.ok("op", "watch", strings.TrimSpace(r.out))

	h.env["FORGE_TOKEN"] = readerToken
	h.fails(5, "service", "restart", "payments", "--env", "staging")
	h.env["FORGE_TOKEN"] = writerToken

	h.ok("deploy", "start", "payments", "--env", "staging")
	if !strings.Contains(values(), "replicas: 1\n") || strings.Contains(values(), "restartedAt") {
		t.Errorf("the next deploy kept the scale or the mark:\n%s", values())
	}
}
