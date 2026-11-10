package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
	"github.com/showmino/forge/internal/store"
)

// The golden path (phase 15, milestone C): five checks Forge works out for
// each service from what it already knows.

const (
	// checksFresh is how long what a repo holds (its workflows, its
	// runbook) is kept before GitHub is asked again. The catalog, the
	// audit log and the teams' settings are read on every call.
	checksFresh = 5 * time.Minute
	// checksRetry is how soon a repo GitHub could not answer is tried again.
	checksRetry = 30 * time.Second
	// ciWindow is how recent a deploy from CI must be to count.
	ciWindow = 30 * 24 * time.Hour
)

// repoFacts is what a service's repo holds that the checks need.
type repoFacts struct {
	workflows bool // a .yml or .yaml file in .github/workflows
	runbook   bool // docs/runbook.md
	err       error
	until     time.Time
}

// checker serves ListServiceChecks.
type checker struct {
	catalog *Catalog
	audit   store.AuditStore
	teams   store.TeamStore
	gh      DocsGitHub
	org     string
	log     *slog.Logger
	now     func() time.Time

	mu    sync.Mutex
	repos map[string]repoFacts // by "owner/repo"
}

func newChecker(catalog *Catalog, audit store.AuditStore, teams store.TeamStore, gh DocsGitHub, org string, log *slog.Logger) *checker {
	return &checker{catalog: catalog, audit: audit, teams: teams, gh: gh, org: org, log: log, now: time.Now, repos: map[string]repoFacts{}}
}

func (c *checker) ListServiceChecks(ctx context.Context, req *connect.Request[forgev1.ListServiceChecksRequest]) (*connect.Response[forgev1.ListServiceChecksResponse], error) {
	if err := require(ctx, PermServicesRead); err != nil {
		return nil, err
	}
	var services []*forgev1.Service
	if name := req.Msg.GetService(); name != "" {
		n, err := parseName(name)
		if err != nil {
			return nil, err
		}
		s, err := c.catalog.get(ctx, n)
		if err != nil {
			return nil, err
		}
		services = []*forgev1.Service{s}
	} else {
		all, err := c.catalog.all(ctx)
		if err != nil {
			return nil, err
		}
		services = all
	}
	oncall := map[string]store.TeamSetting{}
	if c.teams != nil {
		set, err := c.teams.TeamSettings(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the teams' settings: %w", err))
		}
		oncall = set
	}
	out := make([]*forgev1.ServiceChecks, len(services))
	var g errgroup.Group
	g.SetLimit(8)
	for i, s := range services {
		g.Go(func() error {
			sc, err := c.checks(ctx, s, oncall)
			out[i] = sc
			return err
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&forgev1.ListServiceChecksResponse{Services: out}), nil
}

// checks works out one service's checks.
func (c *checker) checks(ctx context.Context, s *forgev1.Service, oncall map[string]store.TeamSetting) (*forgev1.ServiceChecks, error) {
	owner, repo, hasRepo := parseRepoURL(s.GetRepoUrl())
	var facts repoFacts
	if hasRepo {
		facts = c.repoFacts(ctx, owner, repo)
	}
	ci, err := c.ciCheck(ctx, s, hasRepo, facts)
	if err != nil {
		return nil, err
	}
	list := []*forgev1.GoldenCheck{
		ci,
		{Name: "slo", Title: "SLO defined", Applies: true, Passed: s.GetSlo() != "", Detail: pick(s.GetSlo() != "",
			"availability target "+s.GetSlo(),
			"no availability target: set one with 'forge service update "+serviceIDOf(s)+" --slo 99.9%'")},
		{Name: "tracing", Title: "Tracing with OpenTelemetry", Applies: true, Passed: s.GetTracing(), Detail: pick(s.GetTracing(),
			"it sends traces (service.tracing)",
			"service.tracing is not set: add tracing, then 'forge service update "+serviceIDOf(s)+" --tracing'")},
		runbookCheck(hasRepo, facts),
		{Name: "oncall", Title: "On-call rotation", Applies: true, Passed: oncall[s.GetOwnerTeam()].Oncall != "", Detail: pick(oncall[s.GetOwnerTeam()].Oncall != "",
			"team "+s.GetOwnerTeam()+" is on call as "+oncall[s.GetOwnerTeam()].Oncall,
			"team "+s.GetOwnerTeam()+" has no on-call rotation: an admin sets one with 'forge team update "+s.GetOwnerTeam()+" --oncall <rotation>'")},
	}
	out := &forgev1.ServiceChecks{Service: s.GetName(), Checks: list}
	for _, ch := range list {
		if ch.GetApplies() {
			out.Applies++
			if ch.GetPassed() {
				out.Passed++
			}
		}
	}
	return out, nil
}

func pick(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}

func serviceIDOf(s *forgev1.Service) string { return strings.TrimPrefix(s.GetName(), servicePrefix) }

// ciCheck passes when CI deployed the service to staging in the last 30
// days, or its repo has a workflow. A service without a repo runs what
// someone else built: the check does not apply.
func (c *checker) ciCheck(ctx context.Context, s *forgev1.Service, hasRepo bool, facts repoFacts) (*forgev1.GoldenCheck, error) {
	ch := &forgev1.GoldenCheck{Name: "ci", Title: "CI pipeline", Applies: hasRepo}
	if !hasRepo {
		ch.Detail = "no repo: it runs an image or a chart someone else built"
		return ch, nil
	}
	if c.audit != nil {
		events, err := c.audit.ListAuditEvents(ctx, store.AuditQuery{
			Service: s.GetName(), Actor: "workload", Action: ActDeployStart, Since: c.now().Add(-ciWindow), Limit: 20,
		})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("read the audit log: %w", err))
		}
		for _, e := range events {
			if e.GetKind() == "request" && e.GetResult().GetCode() == "ok" {
				ch.Passed = true
				ch.Detail = "CI deployed it " + e.GetTime().AsTime().Format("2006-01-02")
				return ch, nil
			}
		}
	}
	switch {
	case facts.workflows:
		ch.Passed = true
		ch.Detail = "a workflow in .github/workflows"
	case facts.err != nil:
		ch.Detail = "no deploy from CI in 30 days, and GitHub could not be asked for the repo's workflows"
	default:
		ch.Detail = "no deploy from CI in 30 days and no workflow in .github/workflows"
	}
	return ch, nil
}

func runbookCheck(hasRepo bool, facts repoFacts) *forgev1.GoldenCheck {
	ch := &forgev1.GoldenCheck{Name: "runbook", Title: "Runbook", Applies: hasRepo}
	switch {
	case !hasRepo:
		ch.Detail = "no repo to keep one in"
	case facts.runbook:
		ch.Passed = true
		ch.Detail = "docs/runbook.md"
	case facts.err != nil:
		ch.Detail = "GitHub could not be asked for docs/runbook.md"
	default:
		ch.Detail = "no docs/runbook.md: write what to do when it breaks there"
	}
	return ch
}

// repoFacts reads what the checks need from owner/repo's default branch, or
// returns what was read within checksFresh.
func (c *checker) repoFacts(ctx context.Context, owner, repo string) repoFacts {
	key := owner + "/" + repo
	c.mu.Lock()
	f, ok := c.repos[key]
	c.mu.Unlock()
	if ok && c.now().Before(f.until) {
		return f
	}
	f = c.readRepo(ctx, owner, repo)
	if f.err != nil {
		c.log.WarnContext(ctx, "reading a repo for the golden-path checks failed", "repo", key, "error", f.err)
		f.until = c.now().Add(checksRetry)
	} else {
		f.until = c.now().Add(checksFresh)
	}
	c.mu.Lock()
	c.repos[key] = f
	c.mu.Unlock()
	return f
}

// readRepo looks at the head of the default branch: the root, then
// .github/workflows and docs. A missing folder is not an error.
func (c *checker) readRepo(ctx context.Context, owner, repo string) repoFacts {
	if c.gh == nil {
		return repoFacts{err: errors.New("no GitHub App")}
	}
	head, err := c.gh.HeadSHA(ctx, owner, repo)
	if errors.Is(err, github.ErrNotFound) {
		return repoFacts{} // an empty repo holds nothing
	}
	if err != nil {
		return repoFacts{err: err}
	}
	root, err := c.gh.Tree(ctx, owner, repo, head, false)
	if err != nil {
		return repoFacts{err: err}
	}
	var f repoFacts
	if gh := treeEntry(root, ".github", "tree"); gh != "" {
		t, err := c.gh.Tree(ctx, owner, repo, gh, false)
		if err != nil {
			return repoFacts{err: err}
		}
		if wf := treeEntry(t, "workflows", "tree"); wf != "" {
			t, err := c.gh.Tree(ctx, owner, repo, wf, false)
			if err != nil {
				return repoFacts{err: err}
			}
			for _, e := range t.Entries {
				if ext := path.Ext(e.Path); e.Type == "blob" && (ext == ".yml" || ext == ".yaml") {
					f.workflows = true
				}
			}
		}
	}
	if docs := treeEntry(root, "docs", "tree"); docs != "" {
		t, err := c.gh.Tree(ctx, owner, repo, docs, false)
		if err != nil {
			return repoFacts{err: err}
		}
		f.runbook = treeEntry(t, "runbook.md", "blob") != ""
	}
	return f
}

// treeEntry is the SHA of the entry called name of type kind in t, or "".
func treeEntry(t *github.Tree, name, kind string) string {
	for _, e := range t.Entries {
		if e.Path == name && e.Type == kind {
			return e.SHA
		}
	}
	return ""
}

// all returns every service in the catalog, by name.
func (c *Catalog) all(ctx context.Context) ([]*forgev1.Service, error) {
	var out []*forgev1.Service
	after := ""
	for {
		page, err := c.store.List(ctx, store.ListQuery{After: after, Limit: 500})
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("list the catalog: %w", err))
		}
		out = append(out, page...)
		if len(page) < 500 {
			return out, nil
		}
		after = page[len(page)-1].GetName()
	}
}
