package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect"
	"gopkg.in/yaml.v3"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/infrastructure"
	"github.com/showmino/forge/internal/manifest"
	"github.com/showmino/forge/internal/store"
	"github.com/showmino/forge/policy"
)

// OIDCSubjects sets what a repo's Actions OIDC tokens name as their subject
// (*github.Client on GitHub).
type OIDCSubjects interface {
	SetOIDCSubjectClaims(ctx context.Context, owner, repo string, claims []string) error
}

var _ OIDCSubjects = (*github.Client)(nil)

// infraPlan is what an infrastructure template's create sets up, worked out
// before anything is created.
type infraPlan struct {
	target infrastructure.Target
	envs   []string
	// files are the deploy repo's files for the repo (its Record and its
	// Crossplane objects), by path.
	files map[string][]byte
}

// infraReady reports whether this API can make infrastructure repos.
func (c *creator) infraReady() error {
	switch {
	case c.infra == nil:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("infrastructure repos are not configured on this Forge API (no --infrastructure-config)"))
	case c.deploys == nil || c.deploys.o.Repo == nil || c.deploys.o.Policy == nil:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("infrastructure repos need deploys (the deploy repo) on this Forge API"))
	case c.oidc == nil:
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("infrastructure repos need the GitHub App on this Forge API"))
	}
	return nil
}

// planInfrastructure checks the request's target and environments against
// the target rules and renders what the deploy repo will hold.
func (c *creator) planInfrastructure(ctx context.Context, req *forgev1.CreateServiceRequest) (*infraPlan, error) {
	if err := c.infraReady(); err != nil {
		return nil, err
	}
	name := req.GetTarget()
	if name == "" {
		return nil, invalid(fmt.Sprintf("an infrastructure template needs a target: one of %s", strings.Join(c.targetNames(), ", ")))
	}
	t, ok := c.infra.Target(name)
	if !ok {
		return nil, invalid(fmt.Sprintf("there is no target %q: the targets are %s", name, strings.Join(c.targetNames(), ", ")))
	}
	envs := req.GetEnvironments()
	if len(envs) == 0 {
		envs = t.EnvironmentNames()
	}
	// In deploy order, each once.
	var ordered []string
	for _, e := range infrastructure.Environments {
		if slices.Contains(envs, e) {
			ordered = append(ordered, e)
		}
	}
	for _, e := range envs {
		if !slices.Contains(infrastructure.Environments, e) {
			ordered = append(ordered, e) // the rules name it
		}
	}
	full, err := c.fullEnvironments(ctx, t)
	if err != nil {
		return nil, err
	}
	team := req.GetOwnerTeam()
	res, err := c.deploys.o.Policy.EvaluateTarget(ctx, policy.Target{
		Service: req.GetServiceId(), Team: team, Target: t.Name, Teams: t.Teams,
		Environments: ordered, Offered: t.EnvironmentNames(), Full: full,
		Actor: policy.Actor{Kind: actorUser, Subject: subjectOf(ctx)},
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("check the target rules: %w", err))
	}
	noteAuditDetail(ctx, "policy_rules", c.deploys.o.Policy.Fingerprint())
	if res.Denied() {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the target rules refuse it: %s", strings.Join(res.Deny, "; ")))
	}
	files, err := c.infra.Files(t, infrastructure.Repo{Org: c.org, Name: req.GetServiceId(), Team: team, Environments: ordered})
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target %s: %w", t.Name, err))
	}
	return &infraPlan{target: t, envs: ordered, files: files}, nil
}

func (c *creator) targetNames() []string {
	var out []string
	for _, t := range c.infra.Targets {
		out = append(out, t.Name)
	}
	return out
}

// fullEnvironments are the environments of t that have as many repos as
// their maxRepos.
func (c *creator) fullEnvironments(ctx context.Context, t infrastructure.Target) ([]string, error) {
	counts, err := c.catalog.infrastructureCounts(ctx, t.Name)
	if err != nil {
		return nil, err
	}
	var full []string
	for _, env := range t.EnvironmentNames() {
		if max := t.Environments[env].MaxRepos; max > 0 && counts[env] >= max {
			full = append(full, env)
		}
	}
	return full, nil
}

// setUpInfrastructure ties the new repo's production sign-in to the shared
// workflow and writes its Crossplane objects into the deploy repo. undo takes
// back the deploy repo's part.
func (c *creator) setUpInfrastructure(ctx context.Context, id string, p *infraPlan, started func(string), done func(string, string)) (undo func(), err error) {
	started("oidc_subject")
	if err := c.oidc.SetOIDCSubjectClaims(ctx, c.org, id, infrastructure.SubjectClaims); err != nil {
		return nil, fmt.Errorf("make github.com/%s/%s's OIDC subject name the workflow: %w", c.org, id, err)
	}
	done("oidc_subject", strings.Join(infrastructure.SubjectClaims, ", "))

	started("write_infrastructure")
	sha, err := c.deploys.commitFiles(ctx, p.files, fmt.Sprintf("set up the infrastructure repo %s for target %s (%s)", id, p.target.Name, actorOf(ctx)))
	if err != nil {
		return nil, fmt.Errorf("write %s in the deploy repo: %w", infrastructure.Folder(id), err)
	}
	done("write_infrastructure", fmt.Sprintf("forge-deploys %s: %s (%s)", shortSHA(sha), infrastructure.Folder(id), strings.Join(p.envs, ", ")))
	return func() {
		if err := c.deploys.removeInfrastructure(ctx, id); err != nil {
			c.log.WarnContext(ctx, "take back an infrastructure repo's setup", "service", id, "error", err)
		}
	}, nil
}

// infrastructureApp is the Argo CD application of an infrastructure repo's
// Crossplane objects (the forge-infrastructure ApplicationSet).
func infrastructureApp(id string) string { return id + "-infrastructure" }

// awaitInfrastructure waits for Argo CD to have synced the repo's objects
// and for them to be healthy (Crossplane says Ready). It does not fail the
// create: the repo is made, and what is not ready yet is said.
func (c *creator) awaitInfrastructure(ctx context.Context, id string, p *infraPlan, started func(string), done func(string, string)) {
	if c.deploys.o.Cluster == nil {
		return
	}
	started("setup")
	head, _, err := c.deploys.o.Repo.Head(ctx)
	if err == nil {
		err = c.deploys.awaitApp(ctx, id, "infrastructure", infrastructureApp(id), head, p.files, "setup", func(string, string) {})
	}
	if err != nil {
		done("setup", "not ready yet: "+err.Error()+"; Crossplane keeps trying, see the Argo CD application "+infrastructureApp(id))
		return
	}
	done("setup", "GitHub environments and cloud access are ready")
}

// readInfrastructure returns the Record of the infrastructure repo id, or
// nil when the deploy repo has none.
func (d *Deploys) readInfrastructure(ctx context.Context, id string) (*infrastructure.Record, error) {
	data, err := d.o.Repo.Read(ctx, infrastructure.RecordPath(id), "")
	if errors.Is(err, github.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s in the deploy repo: %w", infrastructure.RecordPath(id), err)
	}
	var rec infrastructure.Record
	if err := yaml.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("%s in the deploy repo: %w", infrastructure.RecordPath(id), err)
	}
	return &rec, nil
}

// removeInfrastructure deletes the infrastructure repo id's folder from the
// deploy repo: Argo CD then deletes its Crossplane objects, and Crossplane
// its GitHub environments and cloud access. Safe to repeat.
func (d *Deploys) removeInfrastructure(ctx context.Context, id string) error {
	rec, err := d.readInfrastructure(ctx, id)
	if err != nil || rec == nil {
		return err
	}
	files := map[string][]byte{infrastructure.RecordPath(id): nil}
	for _, env := range rec.Environments {
		files[infrastructure.Folder(id)+"/"+env+".yaml"] = nil
	}
	_, err = d.commitFiles(ctx, files, fmt.Sprintf("remove the infrastructure repo %s (%s)", id, actorOf(ctx)))
	return err
}

// listAllPage is how many services one page of a walk over the catalog reads.
const listAllPage = 500

// infrastructureCounts counts the infrastructure repos of target, by
// environment.
func (c *Catalog) infrastructureCounts(ctx context.Context, target string) (map[string]int, error) {
	counts := map[string]int{}
	after := ""
	for {
		svcs, err := c.store.List(ctx, store.ListQuery{After: after, Limit: listAllPage})
		if err != nil {
			return nil, storeErr(err)
		}
		for _, s := range svcs {
			if i := s.GetInfrastructure(); i != nil && i.GetTarget() == target {
				for _, env := range i.GetEnvironments() {
					counts[env]++
				}
			}
		}
		if len(svcs) < listAllPage {
			return counts, nil
		}
		after = svcs[len(svcs)-1].GetName()
	}
}

// infrastructureOf is the catalog's record of an infrastructure repo.
func infrastructureOf(m *manifest.Manifest, p *infraPlan) *forgev1.InfrastructureRepo {
	return &forgev1.InfrastructureRepo{Tool: m.Infrastructure.Tool, Target: p.target.Name, Environments: p.envs}
}
