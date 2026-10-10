// Package server is the Forge API: the service catalog, auth discovery,
// templates and operations, over Connect, gRPC and gRPC-Web from the same handlers.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/internal/version"
	"github.com/showmino/forge/pkg/client"
)

// OIDC describes the identity provider clients should log in with.
type OIDC struct {
	Issuer   string
	ClientID string
	Scopes   []string
}

// Options configure the API.
type Options struct {
	Version          string
	MinClientVersion string
	OIDC             OIDC
	Authenticator    Authenticator
	Authorizer       Authorizer
	Catalog          *Catalog
	// Templates serves TemplateService; nil answers FAILED_PRECONDITION.
	Templates TemplateSource
	// Repos creates the repos of new services; nil answers FAILED_PRECONDITION
	// to CreateService unless validate_only.
	Repos RepoHost
	// GitHubOrg is the org new repos are created in (default "showmino").
	GitHubOrg string
	// Operations holds long-running work; nil creates an empty store.
	Operations *Operations
	// Environments serves EnvironmentService; nil answers FAILED_PRECONDITION.
	// It must share Catalog and Operations with these Options.
	Environments *Environments
	// Deploys serves DeployService; nil answers FAILED_PRECONDITION. It must
	// share Catalog and Operations with these Options.
	Deploys *Deploys
	// Resources serves ResourceService; nil answers FAILED_PRECONDITION. It
	// must share Catalog and Operations with these Options.
	Resources *Resources
	// CLIDir holds forge CLI builds (forge-linux-amd64, forge-linux-arm64)
	// served at GET /cli/<name>; empty serves none.
	CLIDir string
	// Audit stores the audit log; nil keeps it in memory.
	Audit  store.AuditStore
	Logger *slog.Logger
}

// New returns the API's HTTP handler.
func New(o Options) (http.Handler, error) {
	if o.Authenticator == nil {
		return nil, errors.New("server: an Authenticator is required")
	}
	if o.Catalog == nil {
		o.Catalog = NewCatalog(nil)
	}
	if o.GitHubOrg == "" {
		o.GitHubOrg = "showmino"
	}
	if o.Operations == nil {
		o.Operations = NewOperations(nil)
	}
	if o.Audit == nil {
		o.Audit = store.NewMemory()
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Version == "" {
		o.Version = version.Get().Version
	}
	if o.MinClientVersion == "" {
		o.MinClientVersion = "0.1.0"
	}
	if len(o.OIDC.Scopes) == 0 {
		o.OIDC.Scopes = []string{"openid", "email", "profile", "groups", "offline_access"}
	}

	aud := &auditor{store: o.Audit, log: o.Logger}
	o.Operations.setAuditor(aud)
	if o.Resources != nil {
		o.Resources.setAuditor(aud)
		o.Catalog.resources = o.Resources.o.Store
	}
	opts := connect.WithInterceptors(
		&logInterceptor{log: o.Logger},
		&auditInterceptor{a: aud},
		&authInterceptor{authn: o.Authenticator, authz: o.Authorizer},
	)
	mux := http.NewServeMux()
	create := &creator{
		catalog: o.Catalog, templates: o.Templates, repos: o.Repos, org: o.GitHubOrg, log: o.Logger,
		results: map[string]*outcome{},
	}
	mux.Handle(forgev1connect.NewServiceCatalogServiceHandler(&catalogService{Catalog: o.Catalog, create: create}, opts))
	mux.Handle(forgev1connect.NewAuthServiceHandler(&authService{o: o}, opts))
	mux.Handle(forgev1connect.NewTemplateServiceHandler(&templateService{src: o.Templates, log: o.Logger}, opts))
	mux.Handle(forgev1connect.NewOperationServiceHandler(&operationService{ops: o.Operations}, opts))
	mux.Handle(forgev1connect.NewEnvironmentServiceHandler(o.Environments, opts))
	mux.Handle(forgev1connect.NewDeployServiceHandler(o.Deploys, opts))
	mux.Handle(forgev1connect.NewAuditServiceHandler(&auditService{store: o.Audit}, opts))
	mux.Handle(forgev1connect.NewResourceServiceHandler(o.Resources, opts))
	mux.HandleFunc("GET /cli/{name}", cliDownload(o.CLIDir))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("ok\n"))
	})
	return versionHeaders(o.Version, o.MinClientVersion, mux), nil
}

// HTTPServer wraps the handler with timeouts and HTTP/2 without TLS (h2c),
// which gRPC clients need when TLS terminates at an ingress.
func HTTPServer(addr string, h http.Handler) *http.Server {
	s := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	s.Protocols = new(http.Protocols)
	s.Protocols.SetHTTP1(true)
	s.Protocols.SetHTTP2(true)
	s.Protocols.SetUnencryptedHTTP2(true)
	return s
}

func versionHeaders(v, minClient string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(client.HeaderServerVersion, v)
		w.Header().Set(client.HeaderMinClientVersion, minClient)
		if id := r.Header.Get(client.HeaderRequestID); id != "" {
			w.Header().Set(client.HeaderRequestID, id)
		}
		next.ServeHTTP(w, r)
	})
}

type authService struct{ o Options }

func (a *authService) GetServerInfo(context.Context, *connect.Request[forgev1.GetServerInfoRequest]) (*connect.Response[forgev1.GetServerInfoResponse], error) {
	return connect.NewResponse(&forgev1.GetServerInfoResponse{
		ServerVersion:    a.o.Version,
		MinClientVersion: a.o.MinClientVersion,
		Oidc: &forgev1.OIDCConfig{
			Issuer:   a.o.OIDC.Issuer,
			ClientId: a.o.OIDC.ClientID,
			Scopes:   slices.Clone(a.o.OIDC.Scopes),
		},
	}), nil
}

func (a *authService) GetStatus(ctx context.Context, _ *connect.Request[forgev1.GetStatusRequest]) (*connect.Response[forgev1.GetStatusResponse], error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("not authenticated"))
	}
	return connect.NewResponse(&forgev1.GetStatusResponse{
		Subject:     p.Subject,
		Email:       p.Email,
		Groups:      slices.Clone(p.Groups),
		Permissions: slices.Clone(p.Permissions),
	}), nil
}

type logInterceptor struct{ log *slog.Logger }

func (l *logInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := time.Now()
		resp, err := next(ctx, req)
		attrs := []any{
			"procedure", req.Spec().Procedure,
			"request_id", req.Header().Get(client.HeaderRequestID),
			"client_version", req.Header().Get(client.HeaderClientVersion),
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if err != nil {
			l.log.InfoContext(ctx, "rpc", append(attrs, "code", connect.CodeOf(err).String(), "error", err.Error())...)
		} else {
			l.log.InfoContext(ctx, "rpc", append(attrs, "code", "ok")...)
		}
		return resp, err
	}
}

func (l *logInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (l *logInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
