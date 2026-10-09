package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/server"
)

// withCITokens makes forge-api accept GitHub Actions OIDC tokens next to
// base's: tokens issued by GitHub go to the GitHub verifier, all others to base.
func withCITokens(ctx context.Context, base server.Authenticator, audience string, branches server.DefaultBranches,
	catalog *server.Catalog, org string) server.Authenticator {
	gh := server.NewGitHubOIDCAuthenticator(ctx, server.GitHubOIDCOptions{
		Audience: audience, Org: org, Catalog: catalog, Branches: branches,
	})
	return server.ByIssuer{Default: base, Issuers: map[string]server.Authenticator{server.GitHubIssuer: gh}}
}

// deployFlags are forge-api's deploy settings.
type deployFlags struct {
	on                                          bool
	repo, domain, syncTimeout, namespaceTimeout string
}

// deploys returns the deploy service, or nil when deploys are off. It reuses
// the previews' cluster and image checker. The default domain drops the
// "preview." label from previewDomain: preview.10.150.1.222.nip.io gives
// <service>.<env>.10.150.1.222.nip.io.
func deploys(f deployFlags, app *github.App, previewDomain string, cluster *server.Cluster, images server.ImageChecker,
	catalog *server.Catalog, ops *server.Operations, org string, log *slog.Logger) (*server.Deploys, error) {
	if !f.on {
		return nil, nil
	}
	if app == nil {
		return nil, errors.New("--deploys needs the GitHub App (--github-app-id and --github-app-key-file)")
	}
	if cluster == nil {
		return nil, errors.New("--deploys needs --previews (the cluster connection and image checks)")
	}
	owner, name, ok := strings.Cut(f.repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("--deploy-repo %q must be owner/name", f.repo)
	}
	sync, err := positiveDuration("FORGE_DEPLOY_SYNC_TIMEOUT", f.syncTimeout)
	if err != nil {
		return nil, err
	}
	nsTimeout, err := positiveDuration("FORGE_DEPLOY_NAMESPACE_TIMEOUT", f.namespaceTimeout)
	if err != nil {
		return nil, err
	}
	domain := f.domain
	if domain == "" {
		var found bool
		if domain, found = strings.CutPrefix(previewDomain, "preview."); !found {
			return nil, fmt.Errorf("set --deploy-domain: cannot derive it from preview domain %q", previewDomain)
		}
	}
	gh := &github.Client{Token: app.Token}
	return server.NewDeploys(server.DeployOptions{
		Cluster: cluster, GitHub: gh, Images: images,
		Repo:    &github.RepoFiles{Client: gh, Owner: owner, Name: name, Branch: "main"},
		Catalog: catalog, Operations: ops, Org: org, Domain: domain, SyncTimeout: sync, NamespaceTimeout: nsTimeout, Log: log,
	}), nil
}
