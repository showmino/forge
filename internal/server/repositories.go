package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/repositories"
	"github.com/showmino/forge/internal/store"
)

// RepositoryOptions configure protection of existing Forge-created repos.
type RepositoryOptions struct {
	Catalog *Catalog
	Deploys *Deploys
	Config  *repositories.Config
	Audit   store.AuditStore
	GitHub  RepoGetter
	Org     string
	Log     *slog.Logger
}

// ReconcileRepositories backfills protection only for repos whose successful
// template create is recorded in the audit log. Connected repos are not adopted.
func ReconcileRepositories(ctx context.Context, o RepositoryOptions) ([]string, error) {
	ids, err := o.Catalog.ids(ctx)
	if err != nil {
		return nil, err
	}
	var changed []string
	var errs []error
	for _, id := range ids {
		did, err := reconcileRepository(ctx, o, id)
		if err != nil {
			if o.Log != nil {
				o.Log.WarnContext(ctx, "repository protection not reconciled", "service", id, "error", err)
			}
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
		} else if did {
			changed = append(changed, id)
		}
	}
	return changed, errors.Join(errs...)
}

func reconcileRepository(ctx context.Context, o RepositoryOptions, id string) (bool, error) {
	unlock, err := o.Deploys.lockServiceFor(id, "repository protection")
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := o.Catalog.refuseDeleting(servicePrefix + id); err != nil {
		return false, err
	}
	svc, err := o.Catalog.get(ctx, servicePrefix+id)
	if err != nil {
		return false, err
	}
	if svc.GetRepoUrl() == "" {
		return false, nil
	}
	created, err := createdRepository(ctx, o.Audit, svc)
	if err != nil || !created {
		return false, err
	}
	if store.RepoKey(svc.GetRepoUrl()) != strings.ToLower(o.Org+"/"+id) {
		return false, errors.New("the created repository was renamed or moved; confirm its new ownership before adopting protection")
	}
	repo, err := o.GitHub.GetRepo(ctx, o.Org, id)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(repo.FullName, o.Org+"/"+id) || repo.DefaultBranch != "main" {
		return false, errors.New("the created repository must still belong to this organization and use main")
	}
	data, err := o.Config.Rules(id, svc.GetInfrastructure() != nil)
	if err != nil {
		return false, err
	}
	path := repositories.Path(id)
	head, _, err := o.Deploys.o.Repo.Head(ctx)
	if err != nil {
		return false, err
	}
	current, err := o.Deploys.o.Repo.Read(ctx, path, head)
	if err != nil && !errors.Is(err, github.ErrNotFound) {
		return false, err
	}
	if bytes.Equal(data, current) {
		return false, nil
	}
	_, err = o.Deploys.commitFiles(ctx, map[string][]byte{path: data}, "require pull requests in "+o.Org+"/"+id+" (forge-api)")
	return err == nil, err
}

func createdRepository(ctx context.Context, audit store.AuditStore, svc *forgev1.Service) (bool, error) {
	q := store.AuditQuery{Action: "service.create", Service: svc.GetName(), Limit: listAllPage}
	if svc.GetCreateTime() != nil {
		q.Since = svc.GetCreateTime().AsTime()
	}
	for {
		events, err := audit.ListAuditEvents(ctx, q)
		if err != nil {
			return false, fmt.Errorf("read repository creation provenance: %w", err)
		}
		for _, event := range events {
			// Audit omits false flags; an explicit true denotes a dry run.
			validate := event.GetDetails()["validate_only"]
			if event.GetResult().GetCode() == "ok" && event.GetDetails()["template"] != "" && (validate == "" || validate == "false") {
				return true, nil
			}
		}
		if len(events) < q.Limit {
			return false, nil
		}
		q.BeforeID, _ = store.ParseAuditEventName(events[len(events)-1].GetName())
	}
}
