package server

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"
)

// GitHubIssuer is GitHub Actions' OIDC issuer.
const GitHubIssuer = "https://token.actions.githubusercontent.com"

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// DefaultBranches reads a repo's default branch (*github.Client).
type DefaultBranches interface {
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
}

// GitHubOIDCOptions configure GitHubOIDCAuthenticator.
type GitHubOIDCOptions struct {
	// Issuer defaults to GitHubIssuer; JWKSURL to Issuer + "/.well-known/jwks".
	Issuer, JWKSURL string
	// Audience the tokens must carry (default "forge").
	Audience string
	// Org the repos must be in.
	Org      string
	Catalog  *Catalog
	Branches DefaultBranches
	// BranchTTL is how long a default branch is cached (default 10m).
	BranchTTL time.Duration
	Now       func() time.Time
}

// GitHubOIDCAuthenticator turns a GitHub Actions OIDC token from a Forge
// service's repo into a workload Principal that may deploy that service to
// staging at the token's commit.
type GitHubOIDCAuthenticator struct {
	o        GitHubOIDCOptions
	verifier *oidc.IDTokenVerifier

	mu       sync.Mutex
	branches map[string]branchEntry // by lowercased "owner/repo"
}

type branchEntry struct {
	name string
	at   time.Time
}

// NewGitHubOIDCAuthenticator fetches signing keys from o.JWKSURL when it first
// needs them, so it starts even while GitHub is unreachable.
func NewGitHubOIDCAuthenticator(ctx context.Context, o GitHubOIDCOptions) *GitHubOIDCAuthenticator {
	if o.Issuer == "" {
		o.Issuer = GitHubIssuer
	}
	if o.JWKSURL == "" {
		o.JWKSURL = o.Issuer + "/.well-known/jwks"
	}
	if o.Audience == "" {
		o.Audience = "forge"
	}
	if o.BranchTTL == 0 {
		o.BranchTTL = 10 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(ctx, &http.Client{Timeout: 30 * time.Second}), o.JWKSURL)
	return &GitHubOIDCAuthenticator{
		o:        o,
		verifier: oidc.NewVerifier(o.Issuer, keys, &oidc.Config{ClientID: o.Audience, Now: o.Now}),
		branches: map[string]branchEntry{},
	}
}

// ciDenied is a token that verified but may not do what it asks; the message names the claim.
func ciDenied(format string, a ...any) error {
	return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("CI token: "+format, a...))
}

// Authenticate verifies the token and checks its claims against the org, the
// catalog and the repo's default branch.
func (a *GitHubOIDCAuthenticator) Authenticate(ctx context.Context, token string) (*Principal, error) {
	idt, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("invalid GitHub Actions token: %w", err)
	}
	var c struct {
		Owner      string `json:"repository_owner"`
		Repository string `json:"repository"`
		Ref        string `json:"ref"`
		SHA        string `json:"sha"`
	}
	if err := idt.Claims(&c); err != nil {
		return nil, fmt.Errorf("invalid GitHub Actions token claims: %w", err)
	}
	if !strings.EqualFold(c.Owner, a.o.Org) {
		return nil, ciDenied("repository_owner %q is not %s", c.Owner, a.o.Org)
	}
	owner, repo, ok := strings.Cut(c.Repository, "/")
	if !ok || !strings.EqualFold(owner, a.o.Org) || repo == "" || strings.Contains(repo, "/") {
		return nil, ciDenied("repository %q is not a repository of %s", c.Repository, a.o.Org)
	}
	ids := a.o.Catalog.servicesForRepo(a.o.Org, repo)
	switch {
	case len(ids) == 0:
		return nil, ciDenied("repository %q is not a Forge service: no catalog entry has repo_url https://github.com/%s/%s", c.Repository, a.o.Org, repo)
	case len(ids) > 1:
		return nil, ciDenied("repository %q is the repo of several services (%s)", c.Repository, strings.Join(ids, ", "))
	}
	if !shaRE.MatchString(c.SHA) {
		return nil, ciDenied("sha %q is not a commit SHA", c.SHA)
	}
	branch, err := a.defaultBranch(ctx, owner, repo)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the default branch of %s: %w", c.Repository, err))
	}
	if c.Ref != "refs/heads/"+branch {
		return nil, ciDenied("ref %q is not the default branch of %s (refs/heads/%s)", c.Ref, c.Repository, branch)
	}
	return &Principal{
		Subject:     "github:" + c.Repository,
		Permissions: []string{PermDeploy(ids[0], "staging")},
		Workload:    &Workload{Repository: c.Repository, Service: ids[0], SHA: c.SHA},
	}, nil
}

// defaultBranch reads owner/repo's default branch, cached for BranchTTL.
// Failures are not cached.
func (a *GitHubOIDCAuthenticator) defaultBranch(ctx context.Context, owner, repo string) (string, error) {
	key := strings.ToLower(owner + "/" + repo)
	a.mu.Lock()
	e, ok := a.branches[key]
	a.mu.Unlock()
	if ok && a.o.Now().Sub(e.at) < a.o.BranchTTL {
		return e.name, nil
	}
	name, err := a.o.Branches.DefaultBranch(ctx, owner, repo)
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.branches[key] = branchEntry{name: name, at: a.o.Now()}
	a.mu.Unlock()
	return name, nil
}
