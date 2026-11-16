package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/store"
)

const applicationPrefix = "applications/"

// applicationService serves ApplicationService. An application is computed
// from the catalog on every call: it is the services that name it, and
// nothing else is stored.
type applicationService struct {
	catalog *Catalog
	// deploys says what each part runs; nil, or one that is not set up,
	// leaves the parts' deployments empty.
	deploys *Deploys
	// releases keeps what each promotion and rollback of an application
	// left in production; nil keeps none, and a rollback is refused.
	releases store.ReleaseStore
	now      func() time.Time
}

// applications groups the catalog's services by application, ordered by
// application and, within one, by service name. only "" means every
// application.
func (c *Catalog) applications(ctx context.Context, only string) ([]*forgev1.Application, error) {
	byName := map[string]*forgev1.Application{}
	after := ""
	for {
		svcs, err := c.store.List(ctx, store.ListQuery{After: after, Limit: 500})
		if err != nil {
			return nil, storeErr(err)
		}
		for _, s := range svcs {
			id := s.GetApplication()
			if id == "" || (only != "" && id != only) {
				continue
			}
			app := byName[id]
			if app == nil {
				app = &forgev1.Application{Name: applicationPrefix + id, OwnerTeam: s.GetOwnerTeam()}
				byName[id] = app
			}
			app.Parts = append(app.Parts, &forgev1.ApplicationPart{Service: s.GetName(), DisplayName: s.GetDisplayName(), RepoUrl: s.GetRepoUrl()})
		}
		if len(svcs) < 500 {
			break
		}
		after = svcs[len(svcs)-1].GetName()
	}
	out := make([]*forgev1.Application, 0, len(byName))
	for _, app := range byName {
		out = append(out, app)
	}
	slices.SortFunc(out, func(a, b *forgev1.Application) int { return strings.Compare(a.GetName(), b.GetName()) })
	return out, nil
}

func (s *applicationService) ListApplications(ctx context.Context, _ *connect.Request[forgev1.ListApplicationsRequest]) (*connect.Response[forgev1.ListApplicationsResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	apps, err := s.catalog.applications(ctx, "")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.ListApplicationsResponse{Applications: apps}), nil
}

func (s *applicationService) GetApplication(ctx context.Context, req *connect.Request[forgev1.GetApplicationRequest]) (*connect.Response[forgev1.GetApplicationResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	id, ok := strings.CutPrefix(name, applicationPrefix)
	if !ok || !manifest.ValidID(id) {
		return nil, invalid(fmt.Sprintf("name %q must be an application name like applications/shop", name))
	}
	apps, err := s.catalog.applications(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(apps) == 0 {
		return nil, notFound(name)
	}
	app := apps[0]
	if s.deploys != nil && s.deploys.ready() == nil {
		head, _, err := s.deploys.o.Repo.Head(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the deploy repo: %w", err))
		}
		for _, p := range app.GetParts() {
			deps, err := s.deploys.listService(ctx, head, strings.TrimPrefix(p.GetService(), servicePrefix), "")
			if err != nil {
				return nil, err
			}
			p.Deployments = deps
		}
	}
	return connect.NewResponse(&forgev1.GetApplicationResponse{Application: app}), nil
}
