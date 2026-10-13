package server

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/envtest"
)

func TestDeployRunOptions(t *testing.T) {
	t.Parallel()
	withOptions := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n  port: 9090\n  health: /ready\n" +
		"  replicas: 3\n  resources: {limits: {cpu: 250m, memory: 256Mi}}\n  env: {LOG_LEVEL: info}\n" +
		"  secrets: [{name: stripe, item: vaults/Apps/items/stripe, env: {STRIPE_KEY: credential}}]\n" +
		"  jobs: [{name: cleanup, schedule: \"0 3 * * *\", command: [node, cleanup.js]}]\n  autoscaling: {max: 6}\n"
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		o.GitHub.(*envtest.GitHub).Files[envSHA+":forge.yaml"] = []byte(withOptions)
		o.SecretVaults = []string{"Apps"}
	})
	ctx := context.Background()
	values := func(env string) deployValues {
		t.Helper()
		data, err := f.repo.Read(ctx, valuesPath("payments", env), "")
		if err != nil {
			t.Fatal(err)
		}
		v, err := parseValues(data)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}

	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	s := values("staging")
	// Staging runs one pod and never autoscales; the rest is as forge.yaml says.
	if s.Replicas != 1 || s.Autoscaling != nil || s.Vars["LOG_LEVEL"] != "info" || s.Resources.Limits.Memory != "256Mi" ||
		len(s.Secrets) != 1 || s.Secrets[0].Item != "vaults/Apps/items/stripe" || len(s.Jobs) != 1 || s.Jobs[0].Schedule != "0 3 * * *" {
		t.Errorf("staging: %+v", s)
	}
	if op := f.promote(t, forgev1.Bump_BUMP_UNSPECIFIED); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	p := values("production")
	if p.Replicas != 3 || p.Autoscaling == nil || p.Autoscaling.Min != 3 || p.Autoscaling.Max != 6 || len(p.Secrets) != 1 || len(p.Jobs) != 1 {
		t.Errorf("production: replicas %d autoscaling %+v", p.Replicas, p.Autoscaling)
	}
}

func TestDeploySecretVaults(t *testing.T) {
	t.Parallel()
	kube := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n" +
		"  secrets: [{name: app, item: vaults/Kube/items/forge-github-app, env: {KEY: private-key}}]\n"
	for name, vaults := range map[string][]string{"no vaults": nil, "another vault": {"Apps"}} {
		f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
			o.GitHub.(*envtest.GitHub).Files[envSHA+":forge.yaml"] = []byte(kube)
			o.SecretVaults = vaults
		})
		_, err := f.start(t, "staging")
		if connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), `the vault "Kube" is not one services may read`) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDeployWithoutOptions(t *testing.T) {
	t.Parallel()
	f := newDeployFixture(t, false, nil)
	if op := f.deployRef(t, "staging", ""); op.GetError() != nil {
		t.Fatal(op.GetError())
	}
	data, err := f.repo.Read(context.Background(), valuesPath("payments", "staging"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"resources:", "vars:", "secrets:", "jobs:", "autoscaling:", "autoscaleMax:"} {
		if strings.Contains(string(data), key) {
			t.Errorf("values without options have %s:\n%s", key, data)
		}
	}
}

// A vault taken off the list stops its secrets at the next rollback and
// promote too, not only at the next deploy.
func TestSecretVaultRemoved(t *testing.T) {
	t.Parallel()
	apps := "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\nrun:\n" +
		"  secrets: [{name: stripe, item: vaults/Apps/items/stripe, env: {STRIPE_KEY: credential}}]\n"
	f := newDeployFixture(t, false, func(o *DeployOptions, _ *envtest.Argo) {
		gh := o.GitHub.(*envtest.GitHub)
		gh.Files[envSHA+":forge.yaml"], gh.Files[ciOldSHA+":forge.yaml"] = []byte(apps), []byte(apps)
		o.SecretVaults = []string{"Apps"}
	})
	for _, ref := range []string{"release", ""} {
		if op := f.deployRef(t, "staging", ref); op.GetError() != nil {
			t.Fatal(op.GetError())
		}
	}
	f.deploys.o.SecretVaults = nil
	ctx := context.Background()
	if _, err := f.client.RollbackDeploy(ctx, as("writer", &forgev1.RollbackDeployRequest{Name: "deployments/payments/staging"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("rollback: %v", err)
	}
	if _, err := f.client.PromoteDeploy(ctx, as("writer", &forgev1.PromoteDeployRequest{Service: "services/payments"})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("promote: %v", err)
	}
}

// Staging values written before run options have no forge.replicas: a
// promotion of them gives production one pod and no autoscaler.
func TestForEnvOldValues(t *testing.T) {
	t.Parallel()
	v, err := parseValues([]byte("name: payments\nenv: staging\nimage: x\nport: 8080\nhealth: /healthz\nreplicas: 1\nhost: h\nforge:\n  service: services/payments\n"))
	if err != nil {
		t.Fatal(err)
	}
	v.forEnv("production")
	if v.Replicas != 1 || v.Autoscaling != nil {
		t.Errorf("production from old values: %d %+v", v.Replicas, v.Autoscaling)
	}
}
