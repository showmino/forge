package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/store"
)

func TestResources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := store.NewMemory()
	cluster := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
	cat, ops, log := server.NewCatalogWithStore(st, nil), server.NewOperations(nil), slog.New(slog.DiscardHandler)
	ok := resourceFlags{on: true, runnerImage: "ghcr.io/showmino/forge-runner:abc1234", runnerNamespace: "forge-runner",
		runnerServiceAccount: "forge-runner", runnerAPIURL: "http://forge-api-runner.forge.svc:8081"}

	if res, h, err := resources(ctx, resourceFlags{}, st, cluster, true, cat, ops, log); res != nil || h != nil || err != nil {
		t.Errorf("off: %v %v %v", res, h, err)
	}
	res, h, err := resources(ctx, ok, st, cluster, true, cat, ops, log)
	if err != nil || res == nil || h == nil {
		t.Fatalf("on: %v %v %v", res, h, err)
	}
	for name, tc := range map[string]struct {
		change  func(*resourceFlags)
		cluster *server.Cluster
		deploys bool
		want    string
	}{
		"no deploys":            {func(*resourceFlags) {}, cluster, false, "--resources needs --deploys"},
		"no cluster":            {func(*resourceFlags) {}, nil, true, "--resources needs --deploys"},
		"no image":              {func(f *resourceFlags) { f.runnerImage = "" }, cluster, true, "--resources needs --runner-image"},
		"a bad namespace":       {func(f *resourceFlags) { f.runnerNamespace = "Forge Runner" }, cluster, true, "--runner-namespace"},
		"a bad service account": {func(f *resourceFlags) { f.runnerServiceAccount = "" }, cluster, true, "--runner-service-account"},
		"a URL with no scheme":  {func(f *resourceFlags) { f.runnerAPIURL = "forge-api-runner:8081" }, cluster, true, "--runner-api-url"},
		"a URL with no host":    {func(f *resourceFlags) { f.runnerAPIURL = "http://" }, cluster, true, "--runner-api-url"},
	} {
		f := ok
		tc.change(&f)
		if res, h, err := resources(ctx, f, st, tc.cluster, tc.deploys, cat, ops, log); err == nil || res != nil || h != nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v %v %v, want an error containing %q", name, res, h, err, tc.want)
		}
	}
}
