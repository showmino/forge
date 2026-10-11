package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

// workloadProcedures are the only RPCs a CI caller may make; their handlers
// narrow each one to the caller's own service, staging deploys, resource
// plans and operations.
var workloadProcedures = []string{
	forgev1connect.DeployServiceStartDeployProcedure,
	forgev1connect.DeployServiceGetDeploymentProcedure,
	forgev1connect.OperationServiceGetOperationProcedure,
	forgev1connect.OperationServiceWatchOperationProcedure,
	forgev1connect.ResourceServicePlanResourcesProcedure,
	forgev1connect.ResourceServiceGetResourcePlanProcedure,
}

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
		ctx, err := i.authenticate(ctx, req.Spec().Procedure, req.Header().Get("Authorization"))
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
		ctx, err := i.authenticate(ctx, conn.Spec().Procedure, conn.RequestHeader().Get("Authorization"))
		if err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

func (i *authInterceptor) authenticate(ctx context.Context, procedure, header string) (context.Context, error) {
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok || token == "" {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
	}
	p, err := i.authn.Authenticate(ctx, token)
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return ctx, err // the Authenticator chose the code (a CI token out of scope)
		}
		return ctx, connect.NewError(connect.CodeUnauthenticated, err)
	}
	if r := auditFrom(ctx); r != nil {
		r.setActor(p)
	}
	if p.Workload != nil {
		if !slices.Contains(workloadProcedures, procedure) {
			return ctx, connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("CI token %s may only start and follow its own staging deploy and plan its own resources, not call %s", p.Subject, procedure))
		}
	} else {
		i.authz.Grant(p)
	}
	return context.WithValue(ctx, principalKey{}, p), nil
}

// OIDCAuthenticator verifies ID tokens issued by an OIDC provider such as Dex.
type OIDCAuthenticator struct {
	verifier  *oidc.IDTokenVerifier
	audiences []string
}

// NewOIDCAuthenticator discovers the provider at issuer. Tokens must have one
// of clientIDs in their audience: the CLI's, and the web dashboard's when the
// server accepts its logins. Empty IDs are skipped.
func NewOIDCAuthenticator(ctx context.Context, issuer string, clientIDs ...string) (*OIDCAuthenticator, error) {
	var audiences []string
	for _, id := range clientIDs {
		if id != "" && !slices.Contains(audiences, id) {
			audiences = append(audiences, id)
		}
	}
	if len(audiences) == 0 {
		return nil, errors.New("an OIDC client ID is required")
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discover OIDC issuer %s: %w", issuer, err)
	}
	// The verifier takes one client ID; the audience is checked in Authenticate.
	return &OIDCAuthenticator{verifier: provider.Verifier(&oidc.Config{SkipClientIDCheck: true}), audiences: audiences}, nil
}

// Authenticate verifies the token signature, issuer, audience and expiry.
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, token string) (*Principal, error) {
	idt, err := a.verifier.Verify(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	if !slices.ContainsFunc(idt.Audience, func(aud string) bool { return slices.Contains(a.audiences, aud) }) {
		return nil, errors.New("invalid token: it was not issued to a Forge client")
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

// ByIssuer sends a JWT whose iss claim is a key of Issuers to that
// Authenticator, and every other token, JWT or not, to Default. The claim is
// read unverified: the chosen Authenticator verifies the token, issuer included.
type ByIssuer struct {
	Default Authenticator
	Issuers map[string]Authenticator
}

// Authenticate routes the token.
func (b ByIssuer) Authenticate(ctx context.Context, token string) (*Principal, error) {
	if a, ok := b.Issuers[unverifiedIssuer(token)]; ok {
		return a.Authenticate(ctx, token)
	}
	return b.Default.Authenticate(ctx, token)
}

// unverifiedIssuer reads a JWT's iss claim without checking anything; "" when
// the token is not a JWT.
func unverifiedIssuer(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	return c.Iss
}
