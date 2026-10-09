package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
)

// Permissions. Phase 1 has two; RBAC with roles and policy arrives with the
// resources that need it.
const (
	PermServicesRead  = "services.read"
	PermServicesWrite = "services.write"
)

// Principal is an authenticated caller.
type Principal struct {
	Subject     string
	Email       string
	Groups      []string
	Permissions []string
	// Workload is set for a CI caller. Its Permissions come from its token,
	// not from the Authorizer.
	Workload *Workload
}

// Workload is a CI job of a service's repo, from its GitHub Actions token.
type Workload struct {
	// Repository is "<org>/<repo>" as the token spells it.
	Repository string
	// Service is the catalog id, without "services/".
	Service string
	// SHA is the commit the job runs at.
	SHA string
}

// PermDeploy is a CI caller's permission to deploy service to env.
func PermDeploy(service, env string) string { return "deploy:" + service + ":" + env }

// workloadOf returns the caller's Workload, or nil for a human.
//
//nolint:unused // used by the CI handlers (later task)
func workloadOf(ctx context.Context) *Workload {
	if p, ok := PrincipalFrom(ctx); ok {
		return p.Workload
	}
	return nil
}

// Authenticator turns a bearer token into a Principal.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (*Principal, error)
}

// Authorizer grants permissions to a principal.
type Authorizer struct {
	// WriterGroups may write the catalog. Empty means every authenticated caller may.
	WriterGroups []string
}

// Grant fills p.Permissions.
func (a Authorizer) Grant(p *Principal) {
	p.Permissions = []string{PermServicesRead}
	if len(a.WriterGroups) == 0 || slices.ContainsFunc(p.Groups, func(g string) bool { return slices.Contains(a.WriterGroups, g) }) {
		p.Permissions = append(p.Permissions, PermServicesWrite)
	}
}

type principalKey struct{}

// PrincipalFrom returns the caller set by the auth interceptor.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok
}

func require(ctx context.Context, perm string) error {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	if !slices.Contains(p.Permissions, perm) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s lacks permission %s", p.Subject, perm))
	}
	return nil
}

// public procedures need no credentials.
var public = []string{forgev1connect.AuthServiceGetServerInfoProcedure}

// authInterceptor authenticates every call except the public ones.
type authInterceptor struct {
	authn Authenticator
	authz Authorizer
}

func (i *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if slices.Contains(public, req.Spec().Procedure) {
			return next(ctx, req)
		}
		ctx, err := i.authenticate(ctx, req.Header().Get("Authorization"))
		if err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		ctx, err := i.authenticate(ctx, conn.RequestHeader().Get("Authorization"))
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

func (i *authInterceptor) authenticate(ctx context.Context, header string) (context.Context, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
	}
	p, err := i.authn.Authenticate(ctx, token)
	if err != nil {
		return ctx, connect.NewError(connect.CodeUnauthenticated, err)
	}
	i.authz.Grant(p)
	return context.WithValue(ctx, principalKey{}, p), nil
}

// OIDCAuthenticator verifies ID tokens issued by an OIDC provider such as Dex.
type OIDCAuthenticator struct {
	verifier *oidc.IDTokenVerifier
}

// NewOIDCAuthenticator discovers the provider at issuer. Tokens must have
// clientID in their audience.
func NewOIDCAuthenticator(ctx context.Context, issuer, clientID string) (*OIDCAuthenticator, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC issuer %s: %w", issuer, err)
	}
	return &OIDCAuthenticator{verifier: provider.Verifier(&oidc.Config{ClientID: clientID})}, nil
}

// Authenticate verifies the token signature, issuer, audience and expiry.
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, token string) (*Principal, error) {
	idt, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	var claims struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := idt.Claims(&claims); err != nil {
		return nil, fmt.Errorf("invalid token claims: %w", err)
	}
	return &Principal{Subject: idt.Subject, Email: claims.Email, Groups: claims.Groups}, nil
}

// StaticAuthenticator maps fixed tokens to principals. For tests and local
// development only; the server refuses to use it unless explicitly asked.
type StaticAuthenticator map[string]Principal

// Authenticate looks the token up.
func (s StaticAuthenticator) Authenticate(_ context.Context, token string) (*Principal, error) {
	p, ok := s[token]
	if !ok {
		return nil, errors.New("unknown token")
	}
	c := p
	c.Groups = slices.Clone(p.Groups)
	return &c, nil
}
