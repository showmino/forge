package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

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
// the target rules and renders what the deploy repo will hold. It holds the
// target's lock until release is called, after the repo is registered: the
// count of the target's repos (maxRepos) stays right while creates of it
// run at the same time. forge-api runs as one pod.
func (c *creator) planInfrastructure(ctx context.Context, req *forgev1.CreateServiceRequest) (p *infraPlan, release func(), err error) {
	if err := c.infraReady(); err != nil {
		return nil, nil, err
	}
	if req.GetRunOptions() != nil {
		return nil, nil, invalid("run options are for a service: an infrastructure repo deploys itself and has no run")
	}
	team := req.GetOwnerTeam()
	if !manifest.ValidID(team) {
		return nil, nil, invalid(fmt.Sprintf("owner_team %q must be a team's slug", team))
	}
	name := req.GetTarget()
	if name == "" {
		return nil, nil, invalid(fmt.Sprintf("an infrastructure template needs a target: one of %s", strings.Join(c.targetNames(), ", ")))
	}
	t, ok := c.infra.Target(name)
	if !ok {
		return nil, nil, invalid(fmt.Sprintf("there is no target %q: the targets are %s", name, strings.Join(c.targetNames(), ", ")))
	}
	mu, _ := c.targetLocks.LoadOrStore(t.Name, &sync.Mutex{})
	unlock := mu.(*sync.Mutex).Unlock
	mu.(*sync.Mutex).Lock()
	defer func() {
		if err != nil {
			unlock()
		}
	}()
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
		return nil, nil, err
	}
	res, err := c.deploys.o.Policy.EvaluateTarget(ctx, policy.Target{
		Service: req.GetServiceId(), Team: team, Target: t.Name, Teams: t.Teams,
		Environments: ordered, Offered: t.EnvironmentNames(), Full: full,
		Actor: policy.Actor{Kind: actorUser, Subject: subjectOf(ctx)},
	})
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInternal, fmt.Errorf("check the target rules: %w", err))
	}
	noteAuditDetail(ctx, "policy_rules", c.deploys.o.Policy.Fingerprint())
	if res.Denied() {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the target rules refuse it: %s", strings.Join(res.Deny, "; ")))
	}
	files, err := c.infra.Files(t, infrastructure.Repo{Org: c.org, Name: req.GetServiceId(), Team: team, Environments: ordered})
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("target %s: %w", t.Name, err))
	}
	return &infraPlan{target: t, envs: ordered, files: files}, unlock, nil
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

// setOIDCSubject makes the new repo's OIDC subject name the workflow, before
// its first push: the scaffold's first run then already gets that subject.
func (c *creator) setOIDCSubject(ctx context.Context, id string, started func(string), done func(string, string)) error {
	started("oidc_subject")
	if err := c.oidc.SetOIDCSubjectClaims(ctx, c.org, id, infrastructure.SubjectClaims); err != nil {
		return fmt.Errorf("make github.com/%s/%s's OIDC subject name the workflow: %w", c.org, id, err)
	}
	done("oidc_subject", strings.Join(infrastructure.SubjectClaims, ", "))
	return nil
}

// writeInfrastructure writes the repo's Crossplane objects into the deploy
// repo.
func (c *creator) writeInfrastructure(ctx context.Context, id string, p *infraPlan, started func(string), done func(string, string)) error {
	started("write_infrastructure")
	sha, err := c.deploys.commitFiles(ctx, p.files, fmt.Sprintf("set up the infrastructure repo %s for target %s (%s)", id, p.target.Name, actorOf(ctx)))
	if err != nil {
		return fmt.Errorf("write %s in the deploy repo: %w", infrastructure.Folder(id), err)
	}
	done("write_infrastructure", fmt.Sprintf("forge-deploys %s: %s (%s)", shortSHA(sha), infrastructure.Folder(id), strings.Join(p.envs, ", ")))
	return nil
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

// infrastructureFiles are the paths in the deploy repo's folder of id that
// Forge may have written, whatever its Record says.
func infrastructureFiles(id string) []string {
	out := []string{infrastructure.RecordPath(id)}
	for _, env := range infrastructure.Environments {
		out = append(out, infrastructure.Folder(id)+"/"+env+".yaml")
	}
	return out
}

// infrastructureLeft returns the files of the folder of id the deploy repo
// holds.
func (d *Deploys) infrastructureLeft(ctx context.Context, id string) ([]string, error) {
	var there []string
	for _, p := range infrastructureFiles(id) {
		_, err := d.o.Repo.Read(ctx, p, "")
		switch {
		case err == nil:
			there = append(there, p)
		case !errors.Is(err, github.ErrNotFound):
			return nil, fmt.Errorf("read %s in the deploy repo: %w", p, err)
		}
	}
	return there, nil
}

// removeInfrastructure deletes the infrastructure repo id's folder from the
// deploy repo: Argo CD then deletes its Crossplane objects, and Crossplane
// its GitHub environments and cloud access. Safe to repeat.
func (d *Deploys) removeInfrastructure(ctx context.Context, id string) error {
	there, err := d.infrastructureLeft(ctx, id)
	if err != nil || len(there) == 0 {
		return err
	}
	files := map[string][]byte{}
	for _, p := range there {
		files[p] = nil
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
