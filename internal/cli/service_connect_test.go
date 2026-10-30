package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/github"
)

// readRepos is memRepos that can also read repos, for forge service connect.
type readRepos struct {
	*memRepos
	files map[string]map[string]string // repo -> path -> content
}

func (r *readRepos) DefaultBranch(_ context.Context, _, repo string) (string, error) {
	if _, ok := r.files[repo]; !ok {
		return "", github.ErrNotFound
	}
	return "main", nil
}

func (r *readRepos) Tree(_ context.Context, _, repo, _ string, _ bool) (*github.Tree, error) {
	t := &github.Tree{}
	for p := range r.files[repo] {
		t.Entries = append(t.Entries, github.TreeEntry{Path: p, Type: "blob"})
	}
	return t, nil
}

func (r *readRepos) FileAt(_ context.Context, _, repo, path, _ string) ([]byte, error) {
	c, ok := r.files[repo][path]
	if !ok {
		return nil, github.ErrNotFound
	}
	return []byte(c), nil
}

func TestServiceConnect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.repos = &readRepos{memRepos: &memRepos{repos: map[string]int{}}, files: map[string]map[string]string{
		"tool":  {"go.mod": "module x\ngo 1.25\n", "main.go": ""},
		"blank": {"README.md": ""},
	}}
	h.startAPI(h.authn)

	h.fails(2, "service", "connect")
	h.fails(2, "service", "connect", "github.com/showmino/tool")                                     // no team
	h.fails(2, "service", "connect", "github.com/showmino/tool", "--team", "web", "--port", "70000") // bad port

	r := h.ok("service", "connect", "github.com/showmino/tool", "--team", "web", "--dry-run")
	for _, want := range []string{"Would connect github.com/showmino/tool as services/tool", "forge.yaml", ".github/workflows/forge.yml", "Dockerfile (proposed for go)"} {
		if !strings.Contains(r.out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, r.out)
		}
	}
	h.fails(3, "service", "get", "tool")
	// --name names the service; -o json prints the plan.
	r = h.ok("service", "connect", "github.com/showmino/tool", "--name", "tool2", "--team", "web", "--dry-run", "-o", "json")
	if !strings.Contains(r.out, `"dockerfile": "go"`) {
		t.Errorf("json plan: %s", r.out)
	}

	r = h.ok("service", "connect", "https://github.com/showmino/tool", "--team", "web", "--port", "9090", "--health", "/ready")
	if !strings.Contains(r.out, "services/tool connected") || !strings.Contains(r.out, "https://github.com/showmino/tool/pull/7") || !strings.Contains(r.out, "Merge it") {
		t.Errorf("stdout: %q", r.out)
	}
	if !strings.Contains(r.err, "✓ checked github.com/showmino/tool on main") || !strings.Contains(r.err, "✓ opened pull request") {
		t.Errorf("stderr: %q", r.err)
	}
	if !strings.Contains(h.ok("service", "get", "tool", "-o", "json").out, `"repoUrl": "https://github.com/showmino/tool"`) {
		t.Error("the service is not in the catalog")
	}
	if r := h.fails(6, "service", "connect", "github.com/showmino/blank", "--team", "web"); !strings.Contains(r.err, "add a Dockerfile") {
		t.Errorf("a repo with nothing to build: %v", r)
	}
}
