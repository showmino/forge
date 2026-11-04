package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

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
	rec, err := o.Deploys.readInfrastructure(ctx, id)
	if err != nil || rec == nil {
		return false, err // no folder: being made, or removed
	}
	t, ok := o.Infra.Target(rec.Target)
	if !ok {
		return false, fmt.Errorf("its target %s is no longer configured", rec.Target)
	}
	if rec.OwnerID == 0 || rec.RepoID == 0 {
		r, err := o.GitHub.GetRepo(ctx, o.Org, id)
		if err != nil {
			return false, fmt.Errorf("read github.com/%s/%s: %w", o.Org, id, err)
		}
		rec.OwnerID, rec.RepoID = r.Owner.ID, r.ID
	}
	if o.OIDC != nil {
		if err := o.OIDC.SetOIDCSubjectClaims(ctx, o.Org, id, infrastructure.SubjectClaims); err != nil {
			return false, fmt.Errorf("set github.com/%s/%s's OIDC subject: %w", o.Org, id, err)
		}
	}
	files, err := o.Infra.Files(t, infrastructure.Repo{Org: o.Org, Name: id, Team: rec.Team, Environments: rec.Environments, OwnerID: rec.OwnerID, RepoID: rec.RepoID})
	if err != nil {
		return false, err
	}
	diff := map[string][]byte{}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		cur, err := o.Deploys.o.Repo.Read(ctx, p, "")
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
	sha, err := o.Deploys.commitFiles(ctx, diff, fmt.Sprintf("update the infrastructure repo %s to the current target %s (forge-api)", id, t.Name))
	if err != nil {
		return false, err
	}
	o.Log.InfoContext(ctx, "infrastructure repo reconciled", "service", id, "commit", shortSHA(sha), "files", len(diff))
	return true, nil
}
