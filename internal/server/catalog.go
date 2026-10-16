package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// resources, when set, is asked before a service is deleted: a service
	// that still has resources stays.
	resources store.ResourceStore
	// teardown, when set, takes down what a service runs before it is deleted.
	teardown *teardown
	// access, when set, gives a service's owner team its repo in accessOrg.
	access    TeamAccess
	accessOrg string

	// deleting maps a service being deleted to the operation doing it. While
	// a service is in it nothing new may be started for it: no deploy, no
	// preview, no resource run.
	delMu    sync.Mutex
	deleting map[string]string
}

// refuseDeleting returns FAILED_PRECONDITION while the service called name
// is being deleted.
func (c *Catalog) refuseDeleting(name string) error {
	c.delMu.Lock()
	defer c.delMu.Unlock()
	if op, ok := c.deleting[name]; ok {
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s is being deleted (%s); nothing can be started for it", name, op))
	}
	return nil
}

// markDeleting claims name for a delete, or returns false when one is running.
func (c *Catalog) markDeleting(name, by string) bool {
	c.delMu.Lock()
	defer c.delMu.Unlock()
	if _, ok := c.deleting[name]; ok {
		return false
	}
	if c.deleting == nil {
		c.deleting = map[string]string{}
	}
	c.deleting[name] = by
	return true
}

func (c *Catalog) unmarkDeleting(name string) {
	c.delMu.Lock()
	defer c.delMu.Unlock()
	delete(c.deleting, name)
}

// checkNoResources refuses while the service still has resource records.
func (c *Catalog) checkNoResources(ctx context.Context, name string) error {
	if c.resources == nil {
		return nil
	}
	rs, err := c.resources.ListResources(ctx, name, "")
	if err != nil {
		return storeErr(err)
	}
	if len(rs) == 0 {
		return nil
	}
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.GetName()
	}
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(
		"%s still has resources (%s); remove them from forge.yaml, then run 'forge resource plan --env <env> --allow-destroy' and apply the plan",
		name, strings.Join(names, ", ")))
}

// GuardResources makes DeleteService refuse a service that still has
// resource records in s. forge-api sets it whenever it has a database, also
// with resources turned off: records made while they were on must still
// keep their service.
func (c *Catalog) GuardResources(s store.ResourceStore) { c.resources = s }

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

// teamOf returns the owner team of the service name ("services/<id>").
func (c *Catalog) teamOf(ctx context.Context, name string) (string, error) {
	s, err := c.get(ctx, name)
	if err != nil {
		return "", err
	}
	return s.GetOwnerTeam(), nil
}

// OwnerTeams returns the distinct owner teams of the catalog's services, sorted.
func (c *Catalog) OwnerTeams(ctx context.Context) ([]string, error) {
	var out []string
	after := ""
	for {
		svcs, err := c.store.List(ctx, store.ListQuery{After: after, Limit: 500})
		if err != nil {
			return nil, err
		}
		for _, s := range svcs {
			out = append(out, s.GetOwnerTeam())
		}
		if len(svcs) < 500 {
			break
		}
		after = svcs[len(svcs)-1].GetName()
	}
	slices.Sort(out)
	return slices.Compact(out), nil
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
	if err := authorize(ctx, ActServiceRegister, ""); err != nil {
		return nil, err
	}
	return c.register(ctx, req, true)
}

// register adds a service to the catalog without checking who asks. Its
// callers check: RegisterService for an existing repo (an admin's), and
// CreateService for the repo it has just made (service.create). With grant
// it also gives the owner team its repo, before the service is stored: a
// service is not registered when its team cannot get the repo.
func (c *Catalog) register(ctx context.Context, req *connect.Request[forgev1.RegisterServiceRequest], grant bool) (*connect.Response[forgev1.RegisterServiceResponse], error) {
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
		// Only SetAddress sets an address: it checks the names it takes.
		s.Address = nil
		now := timestamppb.New(c.now())
		s.CreateTime, s.UpdateTime = now, now
		s.Etag = etag(1)
		out = s
		if m.GetValidateOnly() {
			return nil
		}
		if grant {
			if err := c.grantTeam(ctx, s.GetOwnerTeam(), s.GetRepoUrl()); err != nil {
				return err
			}
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
	if _, ok := PrincipalFrom(ctx); !ok {
		return nil, require(ctx, PermServicesWrite)
	}
	in := req.Msg.GetService()
	if in == nil {
		return nil, invalid("service is required")
	}
	name, err := parseName(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := authorize(ctx, ActServiceUpdate, name); err != nil {
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

	var out, prev *forgev1.Service
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
		// Two changes are an admin's, decided on the row this write changes:
		// moving the service to another team (the team decides who may work
		// on it), and pointing it at another repo (that is registering the
		// repo, and the repo decides which service its CI may deploy).
		// authorize decides both without reading the store.
		if slices.Contains(paths, "owner_team") && in.GetOwnerTeam() != cur.GetOwnerTeam() {
			if err := authorize(ctx, ActServiceUpdateTeam, name); err != nil {
				return err
			}
		}
		if slices.Contains(paths, "repo_url") && in.GetRepoUrl() != cur.GetRepoUrl() {
			if err := authorize(ctx, ActServiceRegister, name); err != nil {
				return err
			}
		}
		prev = cloneSvc(cur)
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
		if movedRepo(prev, next) {
			if err := c.grantTeam(ctx, next.GetOwnerTeam(), next.GetRepoUrl()); err != nil {
				return err
			}
		}
		return tx.Put(ctx, next, rev+1)
	})
	if err != nil {
		return nil, storeErr(err)
	}
	if !req.Msg.GetValidateOnly() && movedRepo(prev, out) {
		c.revokeTeam(ctx, prev.GetOwnerTeam(), prev.GetRepoUrl())
	}
	return connect.NewResponse(&forgev1.UpdateServiceResponse{Service: out}), nil
}

// movedRepo reports whether an update changed the team or the repo, and
// with it who should have access to which repo.
func movedRepo(prev, next *forgev1.Service) bool {
	return prev.GetOwnerTeam() != next.GetOwnerTeam() || prev.GetRepoUrl() != next.GetRepoUrl()
}

func (c *Catalog) DeleteService(ctx context.Context, req *connect.Request[forgev1.DeleteServiceRequest]) (*connect.Response[forgev1.DeleteServiceResponse], error) {
	if err := authorize(ctx, ActServiceDelete, ""); err != nil {
		return nil, err
	}
	name, err := parseName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	if err := c.checkNoResources(ctx, name); err != nil {
		return nil, err
	}
	if c.teardown != nil {
		op, removes, err := c.startTeardown(ctx, name, req.Msg.GetEtag(), req.Msg.GetValidateOnly())
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(&forgev1.DeleteServiceResponse{Operation: op, Removes: removes}), nil
	}
	if err := c.deleteChecked(ctx, name, req.Msg.GetEtag(), req.Msg.GetValidateOnly()); err != nil {
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
