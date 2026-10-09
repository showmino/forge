package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

func TestDeploys(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app := &github.App{ID: 1, Key: key, Org: "showmino"}
	cluster := &server.Cluster{Kube: envtest.Kube(), Dynamic: envtest.Dynamic(), Gateway: "forge/forge-previews", PullSecret: "forge/mino-ghcr"}
	log := slog.New(slog.DiscardHandler)
	cat, ops := server.NewCatalog(nil), server.NewOperations(nil)
	ok := deployFlags{on: true, repo: "showmino/forge-deploys", syncTimeout: "10m", namespaceTimeout: "5m"}

	if d, err := deploys(deployFlags{}, nil, "", nil, nil, cat, ops, "showmino", log); d != nil || err != nil {
		t.Errorf("off: %v %v", d, err)
	}
	if d, err := deploys(ok, app, "preview.10.150.1.222.nip.io", cluster, &envtest.Images{}, cat, ops, "showmino", log); d == nil || err != nil {
		t.Errorf("on: %v %v", d, err)
	}
	for name, tc := range map[string]struct {
		f       deployFlags
		app     *github.App
		cluster *server.Cluster
		domain  string
		msg     string
	}{
		"no app":         {ok, nil, cluster, "preview.x.nip.io", "GitHub App"},
		"no previews":    {ok, app, nil, "", "--previews"},
		"bad repo":       {deployFlags{on: true, repo: "nope", syncTimeout: "10m", namespaceTimeout: "5m"}, app, cluster, "preview.x.nip.io", "owner/name"},
		"bad timeout":    {deployFlags{on: true, repo: "a/b", syncTimeout: "soon", namespaceTimeout: "5m"}, app, cluster, "preview.x.nip.io", "FORGE_DEPLOY_SYNC_TIMEOUT"},
		"bad ns timeout": {deployFlags{on: true, repo: "a/b", syncTimeout: "10m", namespaceTimeout: "soon"}, app, cluster, "preview.x.nip.io", "FORGE_DEPLOY_NAMESPACE_TIMEOUT"},
		"bad domain":     {ok, app, cluster, "x.nip.io", "--deploy-domain"},
	} {
		if _, err := deploys(tc.f, tc.app, tc.domain, tc.cluster, &envtest.Images{}, cat, ops, "showmino", log); err == nil || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want %q", name, err, tc.msg)
		}
	}
	if d, err := deploys(deployFlags{on: true, repo: "a/b", domain: "lan.example", syncTimeout: "1m", namespaceTimeout: "5m"}, app, "x.nip.io", cluster, &envtest.Images{}, cat, ops, "showmino", log); d == nil || err != nil {
		t.Errorf("explicit domain: %v %v", d, err)
	}
}

func TestWithCITokens(t *testing.T) {
	t.Parallel()
	base := server.StaticAuthenticator{"dev": {Subject: "dev"}}
	got := withCITokens(t.Context(), base, "forge", nil, server.NewCatalog(nil), "showmino")
	b, ok := got.(server.ByIssuer)
	if !ok {
		t.Fatalf("got %T, want server.ByIssuer", got)
	}
	if _, ok := b.Issuers[server.GitHubIssuer].(*server.GitHubOIDCAuthenticator); !ok || len(b.Issuers) != 1 {
		t.Errorf("issuers: %v", b.Issuers)
	}
	if p, err := got.Authenticate(context.Background(), "dev"); err != nil || p.Subject != "dev" {
		t.Errorf("the dev token must still work: %+v %v", p, err)
	}
}
