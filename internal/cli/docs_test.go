package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/showmino/forge/internal/envtest"
	"github.com/showmino/forge/internal/exitcode"
)

func docsHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	gh := &envtest.DocsGitHub{}
	gh.Push("showmino", "payments", map[string]string{
		"forge.yaml":         "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\ndocs:\n  - path: runbook.md\n    title: When it breaks\n",
		"docs/index.md":      "# Payments\n\nCharges and refunds.\n",
		"docs/runbook.md":    "# Runbook\n\nRestart it.",
		"docs/api/errors.md": "# Errors\n",
	}, time.Now().Add(-3*time.Hour))
	h.docs = gh
	h.startAPI(h.authn)
	h.ok("service", "register", "payments", "--team", "billing", "--repo", "https://github.com/showmino/payments")
	h.ok("service", "register", "ledger", "--team", "billing", "--repo", "https://github.com/showmino/ledger")
	return h
}

func TestDocsListAndGet(t *testing.T) {
	t.Parallel()
	h := docsHarness(t)
	golden(t, "docs_list_table", h.ok("docs", "list").out)
	if r := h.ok("docs", "ls", "-o", "name"); r.out != "services/payments\n" {
		t.Errorf("names: %q", r.out)
	}

	if r := h.ok("docs", "get", "payments"); r.out != "# Payments\n\nCharges and refunds.\n" {
		t.Errorf("start page: %q", r.out)
	}
	if got := printable("a\x1b]0;owned\x07b\tc\r\n\u009bd"); got != "a]0;ownedb\tc\nd" {
		t.Errorf("printable = %q", got)
	}
	// A page without a final newline gets one; "docs/" before the path is allowed.
	if r := h.ok("docs", "get", "payments", "docs/runbook.md"); r.out != "# Runbook\n\nRestart it.\n" {
		t.Errorf("runbook: %q", r.out)
	}
	r := h.ok("docs", "get", "payments", "runbook.md", "-o", "json")
	for _, want := range []string{`"title": "When it breaks"`, `"editUrl": "https://github.com/showmino/payments/edit/main/docs/runbook.md"`} {
		if !strings.Contains(r.out, want) {
			t.Errorf("json lacks %s:\n%s", want, r.out)
		}
	}
	golden(t, "docs_get_pages", h.ok("docs", "get", "payments", "--pages").out)

	if r := h.fails(int(exitcode.NotFound), "docs", "get", "ledger"); !strings.Contains(r.err, "has no docs") {
		t.Errorf("ledger: %q", r.err)
	}
	if r := h.fails(int(exitcode.NotFound), "docs", "get", "payments", "gone.md"); !strings.Contains(r.err, "no page docs/gone.md") {
		t.Errorf("missing page: %q", r.err)
	}
	h.fails(int(exitcode.Usage), "docs", "get", "payments", "../forge.yaml")
	h.fails(int(exitcode.Usage), "docs", "get", "payments", "runbook.md", "--pages")
}

func TestDocsWithoutGitHub(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if r := h.fails(int(exitcode.Conflict), "docs", "list"); !strings.Contains(r.err, "no GitHub App") {
		t.Errorf("stderr: %q", r.err)
	}
}

func TestDocsLint(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	write := func(dir string, files map[string]string) string {
		for p, c := range files {
			full := filepath.Join(dir, p)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	good := write(filepath.Join(h.dir, "good"), map[string]string{
		"forge.yaml":      "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\ndocs:\n  - path: runbook.md\n",
		"docs/index.md":   "# Payments\nSee the [runbook](runbook.md).\n",
		"docs/runbook.md": "# Runbook\n",
	})
	if r := h.ok("docs", "lint", good); r.out != "" || !strings.Contains(r.err, "no problems") {
		t.Errorf("good: %+v", r)
	}

	bad := write(filepath.Join(h.dir, "bad"), map[string]string{
		"forge.yaml":      "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\ndocs:\n  - path: gone.md\n",
		"docs/runbook.md": "# Runbook\n![flow](img/flow.png)\n[back](../README.md)\n",
	})
	golden(t, "docs_lint_table", h.fails(int(exitcode.Unexpected), "docs", "lint", bad).out)
	golden(t, "docs_lint_json", h.fails(int(exitcode.Unexpected), "docs", "lint", bad, "-o", "json").out)

	broken := write(filepath.Join(h.dir, "broken"), map[string]string{"forge.yaml": "apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\ndocs:\n  - path: /etc/passwd\n", "docs/index.md": "# x\n"})
	if r := h.fails(int(exitcode.Unexpected), "docs", "lint", broken); !strings.Contains(r.out, "forge.yaml: docs[0].path") {
		t.Errorf("bad forge.yaml: %q", r.out)
	}
	h.fails(int(exitcode.Usage), "docs", "lint", filepath.Join(h.dir, "nope"))
}
