package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/repositories"
	"github.com/showmino/forge/internal/store"
)

type RepositoryGitHub interface {
	RepoGetter
	ExceptionGitHub
	FileAt(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
	TeamRepoPermission(ctx context.Context, org, slug, owner, repo string) (string, error)
}

// RepositoryOptions configure protection of existing Forge-created repos.
type RepositoryOptions struct {
	Catalog          *Catalog
	Deploys          *Deploys
	Config           *repositories.Config
	Audit            store.AuditStore
	GitHub           RepositoryGitHub
	Org              string
	DeployOwner      string
	DeployName       string
	ReviewAdminTeams []string
	Log              *slog.Logger
}

// ReconcileRepositories protects audited template-created repos and explicitly
// adopted repos. Connected repos without an adoption record are not adopted.
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
	created, err := createdRepository(ctx, o.Audit, svc, repo)
	if err != nil {
		return false, err
	}
	if !created {
		adopted, err := adoptedRepository(ctx, o, svc, repo)
		if err != nil || !adopted {
			return false, err
		}
	}
	reviewer, err := reviewTeam(ctx, o, svc, id)
	if err != nil {
		return false, err
	}
	role, err := o.GitHub.TeamRepoPermission(ctx, o.Org, reviewer, o.Org, id)
	if err != nil {
		return false, fmt.Errorf("check %s access to %s/%s: %w", reviewer, o.Org, id, err)
	}
	if slices.Index(repoRoles, role) < slices.Index(repoRoles, teamRepoRole) {
		return false, fmt.Errorf("%s needs maintain access to %s/%s before it can be a code owner", reviewer, o.Org, id)
	}
	codeowners, err := repositories.Codeowners(o.Org, reviewer)
	if err != nil {
		return false, err
	}
	codeownersPath, current, err := currentCodeowners(ctx, o.GitHub, o.Org, id)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(codeowners, current) {
		sum := sha256.Sum256(codeowners)
		branch := fmt.Sprintf("forge/team-review-%s-%s", id, hex.EncodeToString(sum[:4]))
		p := github.Proposal{
			Branch: branch, Title: "Require team approval on " + id,
			Message: "require the service's team to review changes",
			Body: fmt.Sprintf("Forge requires an approval from the service's code-owner team before merging to `main`.\n\n"+
				"This change makes **@%s/%s** the sole code owner for all files. The repository ruleset will require one approving review and a code-owner approval after this change is merged.",
				o.Org, reviewer),
		}
		f := github.File{Path: codeownersPath, Content: codeowners}
		if len(current) == 0 {
			p.Files = []github.File{f}
		} else {
			p.Update = []github.File{f}
		}
		if _, err := o.GitHub.ProposeFiles(ctx, o.Org, id, p); err != nil {
			return false, fmt.Errorf("propose CODEOWNERS update for %s: %w", id, err)
		}
		if o.Log != nil {
			o.Log.InfoContext(ctx, "team review CODEOWNERS change proposed", "service", id, "team", reviewer)
		}
		return false, nil
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
	current, err = o.Deploys.o.Repo.Read(ctx, path, head)
	if err != nil && !errors.Is(err, github.ErrNotFound) {
		return false, err
	}
	if bytes.Equal(data, current) {
		return false, nil
	}
	_, err = o.Deploys.commitFiles(ctx, map[string][]byte{path: data}, "require pull requests in "+o.Org+"/"+id+" (forge-api)")
	return err == nil, err
}

func currentCodeowners(ctx context.Context, gh RepositoryGitHub, owner, repo string) (string, []byte, error) {
	for _, path := range []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"} {
		data, err := gh.FileAt(ctx, owner, repo, path, "main")
		if errors.Is(err, github.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("read %s in %s/%s: %w", path, owner, repo, err)
		}
		return path, data, nil
	}
	return "CODEOWNERS", nil, nil
}

type reviewExceptionFile struct {
	Service       string    `yaml:"service"`
	Team          string    `yaml:"team"`
	AlternateTeam string    `yaml:"alternateTeam"`
	Reason        string    `yaml:"reason"`
	RequestedBy   string    `yaml:"requestedBy"`
	RequestedAt   time.Time `yaml:"requestedAt"`
}

func reviewTeam(ctx context.Context, o RepositoryOptions, svc *forgev1.Service, id string) (string, error) {
	team := svc.GetOwnerTeam()
	path := repositories.ReviewExceptionPath(id)
	data, err := o.Deploys.o.Repo.Read(ctx, path, "")
	if errors.Is(err, github.ErrNotFound) {
		return team, nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var f reviewExceptionFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return ignoredReviewException(o, id, team, fmt.Sprintf("%s is invalid: %v", path, err)), nil
	}
	if f.Service != id || f.Team != team || !manifest.ValidID(f.AlternateTeam) || f.AlternateTeam == team ||
		strings.TrimSpace(f.Reason) == "" || strings.TrimSpace(f.RequestedBy) == "" || f.RequestedAt.IsZero() ||
		(svc.GetCreateTime() != nil && f.RequestedAt.Before(svc.GetCreateTime().AsTime())) {
		return ignoredReviewException(o, id, team, path+" does not match the current service, team, or request"), nil
	}
	if len(o.ReviewAdminTeams) == 0 {
		return ignoredReviewException(o, id, team, "no platform review teams are configured"), nil
	}
	ok, problem, err := adminReviewedFile(ctx, o, path, f.RequestedBy)
	if err != nil {
		return "", err
	}
	if !ok {
		return ignoredReviewException(o, id, team, problem), nil
	}
	role, err := o.GitHub.TeamRepoPermission(ctx, o.Org, f.AlternateTeam, o.Org, id)
	if err != nil {
		return "", fmt.Errorf("check %s access to %s/%s: %w", f.AlternateTeam, o.Org, id, err)
	}
	if slices.Index(repoRoles, role) < slices.Index(repoRoles, teamRepoRole) {
		return ignoredReviewException(o, id, team, fmt.Sprintf("%s does not have maintain access to %s/%s", f.AlternateTeam, o.Org, id)), nil
	}
	return f.AlternateTeam, nil
}

func ignoredReviewException(o RepositoryOptions, id, fallback, problem string) string {
	if o.Log != nil {
		o.Log.Warn("repository review exception ignored", "service", id, "problem", problem)
	}
	return fallback
}

func adminReviewedFile(ctx context.Context, o RepositoryOptions, path, requester string) (bool, string, error) {
	history, err := o.Deploys.o.Repo.History(ctx, path, 1)
	if err != nil {
		return false, "", fmt.Errorf("read the history of %s: %w", path, err)
	}
	if len(history) == 0 {
		return false, path + " has no history in the deploy repo", nil
	}
	pulls, err := o.GitHub.PullsForCommit(ctx, o.DeployOwner, o.DeployName, history[0].SHA)
	if err != nil {
		return false, "", fmt.Errorf("read the pull request of %s: %w", shortSHA(history[0].SHA), err)
	}
	problem := fmt.Sprintf("its last change (%s) was not merged through a pull request", shortSHA(history[0].SHA))
	for _, p := range pulls {
		if p.Merged.IsZero() || p.Base != "main" {
			continue
		}
		if !strings.EqualFold(p.User, requester) {
			problem = fmt.Sprintf("%s was opened by %s, not the recorded requester %s", p.HTMLURL, p.User, requester)
			continue
		}
		if p.MergeCommit != history[0].SHA {
			problem = fmt.Sprintf("its last change (%s) is not the pull request's merge commit", shortSHA(history[0].SHA))
			continue
		}
		by, err := adminApproval(ctx, o.GitHub, o.DeployOwner, o.DeployName, o.Org, o.ReviewAdminTeams, p, requester)
		if err != nil {
			return false, "", err
		}
		if by == "" {
			problem = fmt.Sprintf("%s was merged without an approval of its last commit by a platform reviewer other than %s", p.HTMLURL, requester)
			continue
		}
		return true, "", nil
	}
	return false, problem, nil
}

type adoptionFile struct {
	Service     string    `yaml:"service"`
	OwnerID     int64     `yaml:"ownerID"`
	RepoID      int64     `yaml:"repoID"`
	Reason      string    `yaml:"reason"`
	RequestedBy string    `yaml:"requestedBy"`
	RequestedAt time.Time `yaml:"requestedAt"`
}

func adoptedRepository(ctx context.Context, o RepositoryOptions, svc *forgev1.Service, repo *github.Repo) (bool, error) {
	path := repositories.AdoptionPath(strings.TrimPrefix(svc.GetName(), servicePrefix))
	head, _, err := o.Deploys.o.Repo.Head(ctx)
	if err != nil {
		return false, fmt.Errorf("read deploy repo head for %s: %w", path, err)
	}
	data, err := o.Deploys.o.Repo.Read(ctx, path, head)
	if errors.Is(err, github.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	var f adoptionFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil || f.Service != strings.TrimPrefix(svc.GetName(), servicePrefix) ||
		f.OwnerID <= 0 || f.RepoID <= 0 || f.OwnerID != repo.Owner.ID || f.RepoID != repo.ID || strings.TrimSpace(f.Reason) == "" ||
		strings.TrimSpace(f.RequestedBy) == "" || f.RequestedAt.IsZero() ||
		(svc.GetCreateTime() != nil && f.RequestedAt.Before(svc.GetCreateTime().AsTime())) {
		if o.Log != nil {
			o.Log.WarnContext(ctx, "repository adoption ignored", "service", svc.GetName(), "reason", "record does not match this repository")
		}
		return false, nil
	}
	member, err := o.GitHub.TeamMember(ctx, o.Org, svc.GetOwnerTeam(), f.RequestedBy)
	if err != nil {
		return false, fmt.Errorf("check whether %s is in %s: %w", f.RequestedBy, svc.GetOwnerTeam(), err)
	}
	if !member {
		if o.Log != nil {
			o.Log.WarnContext(ctx, "repository adoption ignored", "service", svc.GetName(), "reason", "requester is not a member of the owning team")
		}
		return false, nil
	}
	if len(o.ReviewAdminTeams) == 0 {
		if o.Log != nil {
			o.Log.WarnContext(ctx, "repository adoption ignored", "service", svc.GetName(), "reason", "no platform review teams are configured")
		}
		return false, nil
	}
	ok, problem, err := adminReviewedFile(ctx, o, path, f.RequestedBy)
	if err != nil {
		return false, err
	}
	if !ok && o.Log != nil {
		o.Log.WarnContext(ctx, "repository adoption ignored", "service", svc.GetName(), "reason", problem)
	}
	return ok, nil
}

func createdRepository(ctx context.Context, audit store.AuditStore, svc *forgev1.Service, repo *github.Repo) (bool, error) {
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
			if event.GetResult().GetCode() != "ok" || event.GetDetails()["template"] == "" || (validate != "" && validate != "false") {
				continue
			}
			details := event.GetDetails()
			if details["repository_id"] != "" || details["repository_owner_id"] != "" {
				if details["repository_id"] == fmt.Sprint(repo.ID) && details["repository_owner_id"] == fmt.Sprint(repo.Owner.ID) {
					return true, nil
				}
				continue
			}
			// Older audit events predate recording GitHub's immutable IDs. A
			// recreated repo has a creation time after the original create event.
			if event.GetTime() != nil && !repo.CreatedAt.IsZero() && !repo.CreatedAt.After(event.GetTime().AsTime()) {
				return true, nil
			}
		}
		if len(events) < q.Limit {
			return false, nil
		}
		q.BeforeID, _ = store.ParseAuditEventName(events[len(events)-1].GetName())
	}
}
