package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/infrastructure"
)

// RepoGetter reads a repo's GitHub IDs (*github.Client).
type RepoGetter interface {
	GetRepo(ctx context.Context, owner, name string) (*github.Repo, error)
}

var _ RepoGetter = (*github.Client)(nil)

// ReconcileOptions configure ReconcileInfrastructure.
type ReconcileOptions struct {
	Catalog *Catalog
	Deploys *Deploys
	Infra   *infrastructure.Config
	GitHub  RepoGetter
	OIDC    OIDCSubjects
	Org     string
	Log     *slog.Logger
}

// ReconcileInfrastructure writes every infrastructure repo's folder in the
// deploy repo again from the current targets, and its OIDC subject claims:
// a changed target (its access, its secrets, the shared workflow) reaches
// the repos made before, and a repo recorded without GitHub's IDs gets them.
// It changes only what differs, one commit per repo, and returns the repos
// it changed. forge-api runs it when it starts.
func ReconcileInfrastructure(ctx context.Context, o ReconcileOptions) ([]string, error) {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	ids, err := o.Catalog.infrastructureIDs(ctx)
	if err != nil {
		return nil, err
	}
	var changed []string
	var errs []error
	for _, id := range ids {
		did, err := reconcileRepo(ctx, o, id)
		if err != nil {
			o.Log.WarnContext(ctx, "infrastructure repo not reconciled", "service", id, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		if did {
			changed = append(changed, id)
		}
	}
	return changed, errors.Join(errs...)
}

func reconcileRepo(ctx context.Context, o ReconcileOptions, id string) (bool, error) {
	// Not while a delete or a deploy of it runs: a delete must not see its
	// folder come back.
	unlock, err := o.Deploys.lockServiceFor(id, "a reconcile of its infrastructure")
	if err != nil {
		return false, err
	}
	defer unlock()
	name := servicePrefix + id
	if err := o.Catalog.refuseDeleting(name); err != nil {
		return false, err
	}
	// The catalog decides what the repo gets, not the deploy repo's record.
	svc, err := o.Catalog.get(ctx, name)
	if err != nil {
		return false, err
	}
	infra := svc.GetInfrastructure()
	t, ok := o.Infra.Target(infra.GetTarget())
	switch {
	case !ok:
		return false, fmt.Errorf("its target %s is no longer configured", infra.GetTarget())
	case !t.Allows(svc.GetOwnerTeam()):
		return false, fmt.Errorf("target %s no longer allows team %s; its access is left as it is for the platform team to decide", t.Name, svc.GetOwnerTeam())
	}
	for range 3 {
		head, tree, err := o.Deploys.o.Repo.Head(ctx)
		if err != nil {
			return false, fmt.Errorf("deploy repo head: %w", err)
		}
		rec, err := o.Deploys.readInfrastructureAt(ctx, id, head)
		if err != nil || rec == nil {
			return false, err // no folder: being made, or removed
		}
		hadIDs := rec.OwnerID != 0 && rec.RepoID != 0
		if !hadIDs {
			r, err := o.GitHub.GetRepo(ctx, o.Org, id)
			if err != nil {
				return false, fmt.Errorf("read github.com/%s/%s: %w", o.Org, id, err)
			}
			if !strings.EqualFold(r.FullName, o.Org+"/"+id) {
				return false, fmt.Errorf("github.com/%s/%s is now %s: renamed or moved, so its IDs are not read", o.Org, id, r.FullName)
			}
			rec.OwnerID, rec.RepoID = r.Owner.ID, r.ID
		}
		files, err := o.Infra.Files(t, infrastructure.Repo{
			Org: o.Org, Name: id, Team: svc.GetOwnerTeam(), Environments: infra.GetEnvironments(), OwnerID: rec.OwnerID, RepoID: rec.RepoID,
		})
		if err != nil {
			return false, err
		}
		diff := map[string][]byte{}
		for _, p := range slices.Sorted(maps.Keys(files)) {
			cur, err := o.Deploys.o.Repo.Read(ctx, p, head)
			if err != nil && !errors.Is(err, github.ErrNotFound) {
				return false, fmt.Errorf("read %s: %w", p, err)
			}
			if !bytes.Equal(cur, files[p]) {
				diff[p] = files[p]
			}
		}
		if len(diff) == 0 {
			return false, nil
		}
		sha, err := o.Deploys.o.Repo.CommitFiles(ctx, head, tree, diff, fmt.Sprintf("update the infrastructure repo %s to the current target %s (forge-api)", id, t.Name))
		if errors.Is(err, github.ErrNotFastForward) {
			continue // the deploy repo moved: read the record again
		}
		if err != nil {
			return false, err
		}
		// Only now: a repo that sent the old subject keeps working until its
		// credential trusts the new one.
		if !hadIDs && o.OIDC != nil {
			if err := o.OIDC.SetOIDCSubjectClaims(ctx, o.Org, id, infrastructure.SubjectClaims); err != nil {
				return true, fmt.Errorf("set github.com/%s/%s's OIDC subject: %w", o.Org, id, err)
			}
		}
		o.Log.InfoContext(ctx, "infrastructure repo reconciled", "service", id, "commit", shortSHA(sha), "files", len(diff))
		return true, nil
	}
	return false, &OpError{Code: "git_conflict", Message: "the deploy repo kept moving"}
}
