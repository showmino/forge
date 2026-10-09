package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
)

const (
	defaultPageSize = 50
	maxPageSize     = 500
	servicePrefix   = "services/"
)

// Catalog is an in-memory ServiceCatalogService. Phase 2 replaces the storage
// with PostgreSQL behind the same handler.
type Catalog struct {
	mu       sync.Mutex
	services map[string]*forgev1.Service // by resource name
	revision map[string]int64
	// idempotency remembers RegisterService results by request_id.
	idempotency map[string]*forgev1.Service
	now         func() time.Time
}

// NewCatalog returns an empty catalog.
func NewCatalog(now func() time.Time) *Catalog {
	if now == nil {
		now = time.Now
	}
	return &Catalog{
		services:    map[string]*forgev1.Service{},
		revision:    map[string]int64{},
		idempotency: map[string]*forgev1.Service{},
		now:         now,
	}
}

// has reports whether a service is registered under name.
func (c *Catalog) has(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.services[name]
	return ok
}

// ids returns the registered service ids, sorted.
func (c *Catalog) ids() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.services))
	for name := range c.services {
		out = append(out, strings.TrimPrefix(name, servicePrefix))
	}
	slices.Sort(out)
	return out
}

// get returns a copy of the service registered under name.
func (c *Catalog) get(name string) (*forgev1.Service, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.services[name]
	if !ok {
		return nil, false
	}
	return cloneSvc(s), true
}

func (c *Catalog) ListServices(ctx context.Context, req *connect.Request[forgev1.ListServicesRequest]) (*connect.Response[forgev1.ListServicesResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	m := req.Msg
	size := int(m.GetPageSize())
	switch {
	case size < 0:
		return nil, invalid("page_size must not be negative")
	case size == 0:
		size = defaultPageSize
	case size > maxPageSize:
		size = maxPageSize
	}
	after := ""
	if m.GetPageToken() != "" {
		b, err := base64.RawURLEncoding.DecodeString(m.GetPageToken())
		if err != nil {
			return nil, invalid("page_token is malformed")
		}
		after = string(b)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	names := make([]string, 0, len(c.services))
	for n, s := range c.services {
		if m.GetOwnerTeam() != "" && s.GetOwnerTeam() != m.GetOwnerTeam() {
			continue
		}
		if n > after {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	resp := &forgev1.ListServicesResponse{}
	for i, n := range names {
		if i == size {
			resp.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(names[i-1]))
			break
		}
		resp.Services = append(resp.Services, cloneSvc(c.services[n]))
	}
	return connect.NewResponse(resp), nil
}

func (c *Catalog) GetService(ctx context.Context, req *connect.Request[forgev1.GetServiceRequest]) (*connect.Response[forgev1.GetServiceResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	name, err := parseName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.services[name]
	if !ok {
		return nil, notFound(name)
	}
	return connect.NewResponse(&forgev1.GetServiceResponse{Service: cloneSvc(s)}), nil
}

func (c *Catalog) RegisterService(ctx context.Context, req *connect.Request[forgev1.RegisterServiceRequest]) (*connect.Response[forgev1.RegisterServiceResponse], error) {
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	m := req.Msg
	if !manifest.ValidID(m.GetServiceId()) {
		return nil, invalid(fmt.Sprintf("service_id %q must be a lowercase DNS label (a-z, 0-9, '-', max 63)", m.GetServiceId()))
	}
	in := m.GetService()
	if in == nil {
		return nil, invalid("service is required")
	}
	if err := validateService(in); err != nil {
		return nil, err
	}
	name := servicePrefix + m.GetServiceId()

	c.mu.Lock()
	defer c.mu.Unlock()
	if key := m.GetRequestId(); key != "" {
		if prev, ok := c.idempotency[key]; ok {
			return connect.NewResponse(&forgev1.RegisterServiceResponse{Service: cloneSvc(prev)}), nil
		}
	}
	if _, exists := c.services[name]; exists {
		return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", name))
	}
	if err := c.checkDependencies(name, in); err != nil {
		return nil, err
	}
	s := cloneSvc(in)
	s.Name = name
	now := timestamppb.New(c.now())
	s.CreateTime, s.UpdateTime = now, now
	s.Etag = etag(1)
	if m.GetValidateOnly() {
		return connect.NewResponse(&forgev1.RegisterServiceResponse{Service: s}), nil
	}
	c.services[name] = s
	c.revision[name] = 1
	if key := m.GetRequestId(); key != "" {
		c.idempotency[key] = s
	}
	return connect.NewResponse(&forgev1.RegisterServiceResponse{Service: cloneSvc(s)}), nil
}

// updatable lists the field mask paths UpdateService accepts.
var updatable = []string{"display_name", "owner_team", "owners", "lifecycle", "repo_url", "dependencies", "labels"}

func (c *Catalog) UpdateService(ctx context.Context, req *connect.Request[forgev1.UpdateServiceRequest]) (*connect.Response[forgev1.UpdateServiceResponse], error) {
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	in := req.Msg.GetService()
	if in == nil {
		return nil, invalid("service is required")
	}
	name, err := parseName(in.GetName())
	if err != nil {
		return nil, err
	}
	paths := req.Msg.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, invalid("update_mask must name at least one field")
	}
	for _, p := range paths {
		if !slices.Contains(updatable, p) {
			return nil, invalid(fmt.Sprintf("update_mask path %q is not updatable (allowed: %s)", p, strings.Join(updatable, ", ")))
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.services[name]
	if !ok {
		return nil, notFound(name)
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
		return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("%s changed since it was read (etag %s, now %s)", name, in.GetEtag(), cur.GetEtag()))
	}
	next := cloneSvc(cur)
	for _, p := range paths {
		switch p {
		case "display_name":
			next.DisplayName = in.GetDisplayName()
		case "owner_team":
			next.OwnerTeam = in.GetOwnerTeam()
		case "owners":
			next.Owners = slices.Clone(in.GetOwners())
		case "lifecycle":
			next.Lifecycle = in.GetLifecycle()
		case "repo_url":
			next.RepoUrl = in.GetRepoUrl()
		case "dependencies":
			next.Dependencies = slices.Clone(in.GetDependencies())
		case "labels":
			next.Labels = in.GetLabels()
		}
	}
	if err := validateService(next); err != nil {
		return nil, err
	}
	if err := c.checkDependencies(name, next); err != nil {
		return nil, err
	}
	rev := c.revision[name] + 1
	next.UpdateTime = timestamppb.New(c.now())
	next.Etag = etag(rev)
	if req.Msg.GetValidateOnly() {
		return connect.NewResponse(&forgev1.UpdateServiceResponse{Service: next}), nil
	}
	c.services[name] = next
	c.revision[name] = rev
	return connect.NewResponse(&forgev1.UpdateServiceResponse{Service: cloneSvc(next)}), nil
}

func (c *Catalog) DeleteService(ctx context.Context, req *connect.Request[forgev1.DeleteServiceRequest]) (*connect.Response[forgev1.DeleteServiceResponse], error) {
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	name, err := parseName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, ok := c.services[name]
	if !ok {
		return nil, notFound(name)
	}
	if e := req.Msg.GetEtag(); e != "" && e != cur.GetEtag() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s changed since it was read (etag %s, now %s)", name, e, cur.GetEtag()))
	}
	for other, s := range c.services {
		if other != name && slices.Contains(s.GetDependencies(), name) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is a dependency of %s", name, other))
		}
	}
	if !req.Msg.GetValidateOnly() {
		delete(c.services, name)
		delete(c.revision, name)
	}
	return connect.NewResponse(&forgev1.DeleteServiceResponse{}), nil
}

// checkDependencies requires every dependency of the service called name to be
// another registered service, so DeleteService's check is never undermined by
// dangling references. The caller holds c.mu.
func (c *Catalog) checkDependencies(name string, s *forgev1.Service) error {
	for _, d := range s.GetDependencies() {
		if d == name {
			return invalid(fmt.Sprintf("%s cannot depend on itself", name))
		}
		if _, ok := c.services[d]; !ok {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("dependency %s is not registered", d))
		}
	}
	return nil
}

func validateService(s *forgev1.Service) error {
	if s.GetOwnerTeam() == "" {
		return invalid("service.owner_team is required")
	}
	for _, d := range s.GetDependencies() {
		if !strings.HasPrefix(d, servicePrefix) || !manifest.ValidID(strings.TrimPrefix(d, servicePrefix)) {
			return invalid(fmt.Sprintf("dependency %q must be a service name like services/payments", d))
		}
	}
	if s.GetRepoUrl() != "" && !strings.Contains(s.GetRepoUrl(), "://") && !strings.HasPrefix(s.GetRepoUrl(), "git@") {
		return invalid(fmt.Sprintf("repo_url %q must be a URL", s.GetRepoUrl()))
	}
	return nil
}

// parseName accepts "services/x" (and, for convenience, bare "x").
func parseName(n string) (string, error) {
	id := strings.TrimPrefix(n, servicePrefix)
	if !manifest.ValidID(id) {
		return "", invalid(fmt.Sprintf("name %q must look like services/{service}", n))
	}
	return servicePrefix + id, nil
}

func etag(rev int64) string { return `W/"` + strconv.FormatInt(rev, 10) + `"` }

func invalid(msg string) error { return connect.NewError(connect.CodeInvalidArgument, errors.New(msg)) }

func notFound(name string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s not found", name))
}

func cloneSvc(s *forgev1.Service) *forgev1.Service { return proto.Clone(s).(*forgev1.Service) }
