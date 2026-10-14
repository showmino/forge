package cli

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/showmino/forge/internal/github"
)

// memRepos is an in-memory GitHub org for CLI tests.
type memRepos struct {
	mu        sync.Mutex
	repos     map[string]int // name -> files pushed
	pushErr   error
	deleteErr error
	onPush    func() // runs once, before the first push
}

func (m *memRepos) RepoExists(_ context.Context, _, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.repos[name]
	return ok, nil
}

func (m *memRepos) CreateOrgRepo(_ context.Context, org, name, _ string) (*github.Repo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repos[name] = 0
	return &github.Repo{FullName: org + "/" + name, DefaultBranch: "main"}, nil
}

func (m *memRepos) DeleteRepo(_ context.Context, _, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	delete(m.repos, name)
	return nil
}

func (m *memRepos) PushFiles(_ context.Context, _, repo, _, _ string, files []github.File) (string, error) {
	m.mu.Lock()
	hook := m.onPush
	m.onPush = nil
	m.mu.Unlock()
	if hook != nil {
		hook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pushErr != nil {
		return "", m.pushErr
	}
	m.repos[repo] = len(files)
	return "abc1234def", nil
}

func (m *memRepos) ProposeFiles(_ context.Context, owner, repo string, _ github.Proposal) (string, error) {
	return "https://github.com/" + owner + "/" + repo + "/pull/7", nil
}

func (m *memRepos) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.repos)
}

// createHarness serves templateRepo (milestone 2's fixture: "svc" needs
// --set tier=gold|silver) and an in-memory GitHub org.
func createHarness(t *testing.T) (*harness, *memRepos) {
	t.Helper()
	h := newHarness(t)
	writeTree(t, filepath.Join(h.dir, "repo"), templateRepo)
	repos := &memRepos{repos: map[string]int{}}
	h.repos = repos
	h.serveTemplates(filepath.Join(h.dir, "repo"))
	return h, repos
}

func TestCreate(t *testing.T) {
	t.Parallel()
	h, repos := createHarness(t)
	r := h.ok("create", "payments", "--template", "svc", "--team", "platform", "--owner", "ana@example.com", "--set", "tier=gold")
	if r.out != "services/payments created\n" {
		t.Errorf("stdout: %q", r.out)
	}
	for _, want := range []string{"✓ fetched template svc@dev", "✓ rendered 1 files", "✓ created github.com/showmino/payments", "✓ pushed initial commit abc1234 (CI started)", "✓ registered services/payments"} {
		if !strings.Contains(r.err, want) {
			t.Errorf("stderr lacks %q:\n%s", want, r.err)
		}
	}
	if !strings.Contains(h.ok("service", "get", "payments", "-o", "json").out, `"repoUrl": "https://github.com/showmino/payments"`) {
		t.Error("the catalog entry has no repo URL")
	}
	repos.mu.Lock()
	pushed := repos.repos["payments"]
	repos.mu.Unlock()
	if pushed != 1 {
		t.Errorf("files pushed: %d, want 1", pushed)
	}
	h.fails(6, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=gold") // exists now

	r = h.ok("create", "ledger", "--template", "svc", "--team", "platform", "--set", "tier=silver", "-o", "json", "-q")
	if !strings.Contains(r.out, `"name": "services/ledger"`) || r.err != "" {
		t.Errorf("-o json -q: out %q err %q", r.out, r.err)
	}
	// Any service template may be reached only on the network.
	h.ok("create", "intranet", "--template", "svc", "--team", "platform", "--set", "tier=gold", "--network-only")
}

func TestCreateDryRun(t *testing.T) {
	t.Parallel()
	h, repos := createHarness(t)
	r := h.ok("create", "web-app", "--template", "web", "--team", "platform", "--dry-run")
	if !strings.Contains(r.out, "Would create https://github.com/showmino/web-app from web@dev with 1 file:") || !strings.Contains(r.out, "  forge.yaml") {
		t.Errorf("dry run output:\n%s", r.out)
	}
	if repos.count() != 0 {
		t.Error("--dry-run must not create a repo")
	}
	h.fails(3, "service", "get", "web-app")
}

func TestCreateUsage(t *testing.T) {
	t.Parallel()
	h, _ := createHarness(t)
	r := h.fails(2, "create", "payments", "--template", "svc", "--team", "platform") // tier is required, no terminal
	if !strings.Contains(r.err, `parameter "tier" is required`) || !strings.Contains(r.err, "--set tier=") {
		t.Errorf("missing parameter:\n%s", r.err)
	}
	h.fails(2, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=bronze")
	h.fails(2, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=gold", "--set", "nope=1")
	h.fails(2, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier")
	h.fails(2, "create", "payments", "--team", "platform")
	h.fails(2, "create", "payments", "--template", "svc", "--set", "tier=gold") // --team, no terminal
	h.fails(2, "create", "Pay_ments", "--template", "svc", "--team", "platform")
	h.fails(2, "create")
	h.fails(3, "create", "payments", "--template", "nope", "--team", "platform")

	// Run options: badly written flags, and an option the template does not offer.
	base := []string{"create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=gold"}
	for _, bad := range [][]string{
		{"--resources", "cpu=1"}, {"--env", "NOEQUALS"}, {"--secret", "stripe=vaults/A/items/b"},
		{"--secret", "stripe=vaults/A/items/b:KEY"}, {"--job", "cleanup=@daily"}, {"--job", "cleanup"},
	} {
		h.fails(2, append(base, bad...)...)
	}
	if r := h.fails(2, append(base, "--replicas", "2")...); !strings.Contains(r.err, "does not offer the run option replicas (it offers: none)") {
		t.Errorf("not offered:\n%s", r.err)
	}
}

func TestRunFlags(t *testing.T) {
	t.Parallel()
	r := runFlags{
		replicas: 2, autoscalingMax: 5, resources: []string{"requests.cpu=100m", "limits.memory=512Mi"}, env: []string{"LOG_LEVEL=info", "EMPTY="},
		secrets: []string{"stripe=vaults/Kube/items/shop-stripe:STRIPE_KEY=credential,STRIPE_HOOK=webhook"},
		jobs:    []string{"cleanup=0 3 * * *:node scripts/cleanup.js --all"},
	}
	ro, err := r.options()
	if err != nil {
		t.Fatal(err)
	}
	if ro.GetReplicas() != 2 || ro.GetAutoscalingMax() != 5 || ro.GetResources().GetRequestsCpu() != "100m" || ro.GetResources().GetLimitsMemory() != "512Mi" ||
		ro.GetEnv()["LOG_LEVEL"] != "info" || ro.GetEnv()["EMPTY"] != "" || len(ro.GetEnv()) != 2 ||
		ro.GetSecrets()[0].GetItem() != "vaults/Kube/items/shop-stripe" || ro.GetSecrets()[0].GetEnv()["STRIPE_HOOK"] != "webhook" ||
		ro.GetJobs()[0].GetSchedule() != "0 3 * * *" || strings.Join(ro.GetJobs()[0].GetCommand(), "|") != "node|scripts/cleanup.js|--all" {
		t.Errorf("options: %v", ro)
	}
	if ro, err := (&runFlags{}).options(); ro != nil || err != nil {
		t.Errorf("none set: %v %v", ro, err)
	}
}

func TestCreatePrompts(t *testing.T) {
	t.Parallel()
	h, _ := createHarness(t)
	h.tty = true
	h.stdin = "platform\ngold\n"
	r := h.ok("create", "payments", "--template", "svc")
	if !strings.Contains(r.err, "Owning team: ") || !strings.Contains(r.err, "tier (enum (gold, silver)): ") {
		t.Errorf("prompts:\n%s", r.err)
	}
	h.tty, h.stdin = false, ""
	if !strings.Contains(h.ok("service", "get", "payments", "-o", "json").out, `"ownerTeam": "platform"`) {
		t.Error("the prompted team was not used")
	}
}

func TestCreateFailures(t *testing.T) {
	t.Parallel()
	h, repos := createHarness(t)
	repos.pushErr = errors.New("boom")
	r := h.fails(7, "create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=gold")
	if !strings.Contains(r.err, "push failed: boom") || !strings.Contains(r.err, "deleted again") {
		t.Errorf("rolled back:\n%s", r.err)
	}
	repos.mu.Lock()
	repos.deleteErr = errors.New("denied")
	repos.mu.Unlock()
	r = h.fails(7, "create", "ledger", "--template", "svc", "--team", "platform", "--set", "tier=gold")
	if !strings.Contains(r.err, "could not be deleted") {
		t.Errorf("not rolled back:\n%s", r.err)
	}

	noGitHub := newHarness(t)
	writeTree(t, filepath.Join(noGitHub.dir, "repo"), templateRepo)
	noGitHub.serveTemplates(filepath.Join(noGitHub.dir, "repo"))
	r = noGitHub.fails(6, "create", "payments", "--template", "web", "--team", "platform")
	if !strings.Contains(r.err, "not configured") {
		t.Errorf("no GitHub App:\n%s", r.err)
	}
	noGitHub.ok("create", "payments", "--template", "web", "--team", "platform", "--dry-run")
}

// A stream that drops mid-create is retried with the same request_id; the
// retry waits for the server's still-running create and reports its outcome.
func TestCreateRetryAfterDrop(t *testing.T) {
	t.Parallel()
	h, repos := createHarness(t)
	repos.mu.Lock()
	repos.onPush = func() { h.api.CloseClientConnections() }
	repos.mu.Unlock()
	r := h.ok("create", "payments", "--template", "svc", "--team", "platform", "--set", "tier=gold")
	if r.out != "services/payments created\n" || !strings.Contains(r.err, "connection dropped") {
		t.Errorf("retry after a drop:\nstdout %q\nstderr %s", r.out, r.err)
	}
	if repos.count() != 1 {
		t.Errorf("%d repos, want exactly 1", repos.count())
	}
}

// A create can take longer than the API client's overall timeout; the
// stream must only be bounded by --timeout.
func TestStreamingHTTPClient(t *testing.T) {
	t.Parallel()
	base := &http.Client{Timeout: time.Minute, Transport: http.DefaultTransport}
	c := withoutTimeout(base)
	if c.Timeout != 0 || c.Transport != base.Transport || base.Timeout != time.Minute {
		t.Errorf("got timeout %v transport %v; base timeout %v", c.Timeout, c.Transport, base.Timeout)
	}
}
