package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/server"
	"github.com/showmino/forge/internal/store"
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
	// publicDomains is --public-domains, publicTunnel --public-dns-tunnel.
	publicDomains, publicTunnel string
	// argoURL is --argocd-url, Argo CD's web address.
	argoURL string
	// resources says which database a service has, for DATABASE_URL.
	resources store.ResourceStore
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
	public, err := server.ParsePublicDomains(f.publicDomains)
	if err != nil {
		return nil, fmt.Errorf("--public-domains: %w", err)
	}
	if len(public) > 0 {
		if !manifest.ValidID(f.publicTunnel) {
			return nil, fmt.Errorf("--public-dns-tunnel %q must be a name such as production", f.publicTunnel)
		}
		log.Info("services get public addresses", "domains", f.publicDomains, "tunnel", f.publicTunnel)
	}
	gh := &github.Client{Token: app.Token}
	argoURL := strings.TrimSuffix(f.argoURL, "/")
	if u, err := url.Parse(argoURL); argoURL != "" && (err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil) {
		return nil, fmt.Errorf("--argocd-url %q must be a scheme and a host, like https://argocd.example.com", f.argoURL)
	}
	return server.NewDeploys(server.DeployOptions{
		PublicDomains: public, DNSTunnel: f.publicTunnel, HostTaken: server.ResolvesInDNS, ArgoURL: argoURL,
		Cluster: cluster, GitHub: gh, Images: images,
		Repo:    &github.RepoFiles{Client: gh, Owner: owner, Name: name, Branch: "main"},
		Catalog: catalog, Operations: ops, Resources: f.resources, Releases: gh,
		Org: org, Domain: domain, SyncTimeout: sync, NamespaceTimeout: nsTimeout, Log: log,
	}), nil
}
