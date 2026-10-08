package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/template"
)

// RepoHost creates, fills and deletes the repos of new services: GitHub, or
// a fake in tests.
type RepoHost interface {
	RepoExists(ctx context.Context, owner, name string) (bool, error)
	CreateOrgRepo(ctx context.Context, org, name, description string) (*github.Repo, error)
	DeleteRepo(ctx context.Context, owner, name string) error
	PushFiles(ctx context.Context, owner, repo, branch, message string, files []github.File) (string, error)
}

var _ RepoHost = (*github.Client)(nil)

// catalogService adds CreateService to the Catalog's handlers.
type catalogService struct {
	*Catalog
	create *creator
}

var _ forgev1connect.ServiceCatalogServiceHandler = (*catalogService)(nil)

func (s *catalogService) CreateService(ctx context.Context, req *connect.Request[forgev1.CreateServiceRequest], stream *connect.ServerStream[forgev1.CreateServiceResponse]) error {
	return s.create.run(ctx, req.Msg, stream)
}

// createStream is the part of the server stream the pipeline uses.
type createStream interface {
	Send(*forgev1.CreateServiceResponse) error
}

// maxParams caps the number of parameters in one request.
const maxParams = 100

// creator runs CreateService: render a template, create the repo, push the
// first commit and register the service.
type creator struct {
	catalog   *Catalog
	templates TemplateSource
	repos     RepoHost // nil: repos cannot be created; validate_only still works
	org       string
	log       *slog.Logger

	mu sync.Mutex
	// results holds the last message of each create by request_id; nil while it runs.
	results map[string]*forgev1.CreateServiceResponse
}

func validateCreate(req *forgev1.CreateServiceRequest) error {
	if !manifest.ValidID(req.GetServiceId()) {
		return invalid(fmt.Sprintf("service_id %q must be a lowercase DNS label (a-z, 0-9, '-', max 63)", req.GetServiceId()))
	}
	if !manifest.ValidID(strings.TrimPrefix(req.GetTemplate(), templatePrefix)) {
		return invalid(fmt.Sprintf("template %q must look like templates/{template}", req.GetTemplate()))
	}
	if strings.TrimSpace(req.GetOwnerTeam()) == "" {
		return invalid("owner_team is required")
	}
	for _, o := range req.GetOwners() {
		if strings.TrimSpace(o) == "" {
			return invalid("owners must not be empty strings")
		}
	}
	if len(req.GetParameters()) > maxParams {
		return invalid(fmt.Sprintf("at most %d parameters", maxParams))
	}
	for k, v := range req.GetParameters() {
		if len(v) > template.MaxParamLen {
			return invalid(fmt.Sprintf("parameter %q is longer than %d bytes", k, template.MaxParamLen))
		}
	}
	return nil
}

// run checks the caller and the request, applies request_id idempotency and
// runs the pipeline.
func (c *creator) run(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) error {
	if err := require(ctx, PermServicesWrite); err != nil {
		return err
	}
	if err := validateCreate(req); err != nil {
		return err
	}
	if c.templates == nil {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("templates are not configured on this Forge API"))
	}
	if c.repos == nil && !req.GetValidateOnly() {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("creating repos is not configured on this Forge API (no GitHub App)"))
	}
	key := req.GetRequestId()
	if key != "" && !req.GetValidateOnly() {
		c.mu.Lock()
		final, seen := c.results[key]
		if !seen {
			c.results[key] = nil
		}
		c.mu.Unlock()
		if seen {
			if final == nil {
				return connect.NewError(connect.CodeAborted, errors.New("a create with this request_id is still running"))
			}
			return stream.Send(final)
		}
	}
	final, err := c.create(ctx, req, stream)
	if key != "" && !req.GetValidateOnly() {
		c.mu.Lock()
		if final != nil {
			c.results[key] = final
		} else {
			delete(c.results, key) // nothing happened yet: a retry starts over
		}
		c.mu.Unlock()
	}
	return err
}

// create checks that the name is free in the catalog and on GitHub, then
// runs the pipeline. It returns the last message (nil when it ended with an
// error before any side effect).
func (c *creator) create(ctx context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	id := req.GetServiceId()
	if c.catalog.has(servicePrefix + id) {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s%s already exists", servicePrefix, id))
	}
	if c.repos != nil {
		exists, err := c.repos.RepoExists(ctx, c.org, id)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("check github.com/%s/%s: %w", c.org, id, err))
		}
		if exists {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("github.com/%s/%s already exists", c.org, id))
		}
	}
	return c.scaffold(ctx, req, stream)
}

func (c *creator) scaffold(_ context.Context, req *forgev1.CreateServiceRequest, stream createStream) (*forgev1.CreateServiceResponse, error) {
	if !req.GetValidateOnly() {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("create is not implemented yet"))
	}
	final := &forgev1.CreateServiceResponse{Event: &forgev1.CreateServiceResponse_Plan{Plan: &forgev1.CreatePlan{}}}
	return final, stream.Send(final)
}
