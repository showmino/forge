package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/store"
)

const (
	defaultPageSize = 50
	maxPageSize     = 500
	servicePrefix   = "services/"
)

// Catalog is the ServiceCatalogService. Its rules live here; a store.Store
// keeps the services (in memory, or PostgreSQL in production).
type Catalog struct {
	store store.Store
	now   func() time.Time
}

// NewCatalog returns an empty catalog in memory.
func NewCatalog(now func() time.Time) *Catalog { return NewCatalogWithStore(store.NewMemory(), now) }

// NewCatalogWithStore returns a catalog kept in s.
func NewCatalogWithStore(s store.Store, now func() time.Time) *Catalog {
	if now == nil {
		now = time.Now
	}
	return &Catalog{store: s, now: now}
}

// storeErr passes connect errors through and turns a storage failure into
// Unavailable.
func storeErr(err error) error {
	var ce *connect.Error
	if err == nil || errors.As(err, &ce) {
		return err
	}
	return connect.NewError(connect.CodeUnavailable, fmt.Errorf("service catalog storage: %w", err))
}

// has reports whether a service is registered under name.
func (c *Catalog) has(ctx context.Context, name string) (bool, error) {
	_, _, err := c.store.Get(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, storeErr(err)
}

// ids returns the registered service ids, sorted.
func (c *Catalog) ids(ctx context.Context) ([]string, error) {
	names, err := c.store.Names(ctx)
	if err != nil {
		return nil, storeErr(err)
	}
	for i, n := range names {
		names[i] = strings.TrimPrefix(n, servicePrefix)
	}
	return names, nil
}

// get returns the service registered under name: a NotFound error when
// there is none.
func (c *Catalog) get(ctx context.Context, name string) (*forgev1.Service, error) {
	s, _, err := c.store.Get(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound(name)
	}
	return s, storeErr(err)
}

// servicesForRepo returns the ids, sorted, of the services whose repo_url is
// https://github.com/<org>/<repo>, both compared case-insensitively.
func (c *Catalog) servicesForRepo(ctx context.Context, org, repo string) ([]string, error) {
	names, err := c.store.ForRepo(ctx, org, repo)
	if err != nil {
		return nil, storeErr(err)
	}
	for i, n := range names {
		names[i] = strings.TrimPrefix(n, servicePrefix)
	}
	return names, nil
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

	svcs, err := c.store.List(ctx, store.ListQuery{OwnerTeam: m.GetOwnerTeam(), After: after, Limit: size + 1})
	if err != nil {
		return nil, storeErr(err)
	}
	resp := &forgev1.ListServicesResponse{Services: svcs}
	if len(svcs) > size {
		resp.Services = svcs[:size]
		resp.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(svcs[size-1].GetName()))
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
	s, err := c.get(ctx, name)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.GetServiceResponse{Service: s}), nil
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

	var out *forgev1.Service
	err := c.store.Write(ctx, func(tx store.Tx) error {
		if key := m.GetRequestId(); key != "" {
			prev, err := tx.Recall(ctx, key)
			if err == nil {
				out = prev
				return nil
			}
			if !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		switch _, _, err := tx.Get(ctx, name); {
		case err == nil:
			return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("%s already exists", name))
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		if err := checkDependencies(ctx, tx, name, in); err != nil {
			return err
		}
		s := cloneSvc(in)
		s.Name = name
		now := timestamppb.New(c.now())
		s.CreateTime, s.UpdateTime = now, now
		s.Etag = etag(1)
		out = s
		if m.GetValidateOnly() {
			return nil
		}
		if err := tx.Put(ctx, s, 1); err != nil {
			return err
		}
		if key := m.GetRequestId(); key != "" {
			return tx.Remember(ctx, key, s)
		}
		return nil
	})
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.RegisterServiceResponse{Service: out}), nil
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

	var out *forgev1.Service
	err = c.store.Write(ctx, func(tx store.Tx) error {
		cur, rev, err := tx.Get(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return notFound(name)
		}
		if err != nil {
			return err
		}
		if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
			return connect.NewError(connect.CodeAborted, fmt.Errorf("%s changed since it was read (etag %s, now %s)", name, in.GetEtag(), cur.GetEtag()))
		}
		next := cur // the store returned a copy
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
			return err
		}
		if err := checkDependencies(ctx, tx, name, next); err != nil {
			return err
		}
		next.UpdateTime = timestamppb.New(c.now())
		next.Etag = etag(rev + 1)
		out = next
		if req.Msg.GetValidateOnly() {
			return nil
		}
		return tx.Put(ctx, next, rev+1)
	})
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.UpdateServiceResponse{Service: out}), nil
}

func (c *Catalog) DeleteService(ctx context.Context, req *connect.Request[forgev1.DeleteServiceRequest]) (*connect.Response[forgev1.DeleteServiceResponse], error) {
	if err := require(ctx, PermServicesWrite); err != nil {
		return nil, err
	}
	name, err := parseName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	err = c.store.Write(ctx, func(tx store.Tx) error {
		cur, _, err := tx.Get(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return notFound(name)
		}
		if err != nil {
			return err
		}
		if e := req.Msg.GetEtag(); e != "" && e != cur.GetEtag() {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s changed since it was read (etag %s, now %s)", name, e, cur.GetEtag()))
		}
		deps, err := tx.Dependents(ctx, name)
		if err != nil {
			return err
		}
		if len(deps) > 0 {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is a dependency of %s", name, deps[0]))
		}
		if req.Msg.GetValidateOnly() {
			return nil
		}
		return tx.Delete(ctx, name)
	})
	if err != nil {
		return nil, storeErr(err)
	}
	return connect.NewResponse(&forgev1.DeleteServiceResponse{}), nil
}

// checkDependencies requires every dependency of the service called name to be
// another registered service, so DeleteService's check is never undermined by
// dangling references.
func checkDependencies(ctx context.Context, tx store.Tx, name string, s *forgev1.Service) error {
	for _, d := range s.GetDependencies() {
		if d == name {
			return invalid(fmt.Sprintf("%s cannot depend on itself", name))
		}
		_, _, err := tx.Get(ctx, d)
		if errors.Is(err, store.ErrNotFound) {
			return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("dependency %s is not registered", d))
		}
		if err != nil {
			return err
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
