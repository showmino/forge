package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

// A worker (run.worker) is deployed and promoted with no port, no health
// check and no address.
func TestDeployWorker(t *testing.T) {
	t.Parallel()
	worker := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  worker: true\n  env: {QUEUE: orders}\n"
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.GitHub.(*envtest.GitHub).Files[envSHA+":forge.yaml"] = []byte(worker)
	})
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	for _, env := range []string{"staging", "production"} {
		if env == "production" {
			if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
				t.Fatal(op.GetError())
			}
		}
		v := f.values(t, env)
		if !v.Worker || !v.Forge.Worker || v.Host != "" || v.Port != 0 || v.Health != "" || v.DNS != nil || v.Vars["QUEUE"] != "orders" {
			t.Errorf("%s values: %+v", env, v)
		}
		dep, err := f.client.GetDeployment(context.Background(), as("writer", &forgev1.GetDeploymentRequest{Name: "deployments/payments/" + env}))
		if err != nil {
			t.Fatal(err)
		}
		if d := dep.Msg.GetDeployment(); !d.GetWorker() || d.GetUrl() != "" {
			t.Errorf("%s deployment: worker %v url %q", env, d.GetWorker(), d.GetUrl())
		}
	}

	// It has no address to set, and nothing to preview.
	_, err := f.client.SetAddress(context.Background(), as("writer", &forgev1.SetAddressRequest{Service: "services/payments", Host: "pay"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "is a worker") {
		t.Errorf("set address: %v", err)
	}
}
