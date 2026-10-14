package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateFromFileTemplate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	writeTree(t, filepath.Join(h.dir, "repo"), map[string]string{
		"lz/template.yaml": "apiVersion: forge.dev/v1\nkind: Template\nname: lz\ntitle: Landing zone\ndescription: A landing zone\n" +
			"type: file\ntarget: showmino/landingzone\nparameters:\n  - name: kind\n    type: enum\n    values: [corp, online]\n" +
			"  - name: envs\n    type: list\n    values: [prod, dev]\n  - name: ips\n    type: int\n    min: 1\n    max: 1024\n",
		"lz/skeleton/zones/{{ .Params.kind }}/lz-{{ .Service.Name }}.bicepparam": "envs={{ .Params.envs }}\n",
	})
	h.repos = &memRepos{repos: map[string]int{}}
	h.serveTemplates(filepath.Join(h.dir, "repo"))

	r := h.ok("create", "infosystems", "--template", "lz", "--team", "platform", "--set", "kind=corp", "--set", "envs=prod,dev", "--set", "ips=100", "--dry-run")
	if !strings.Contains(r.out, "Would add 1 file to https://github.com/showmino/landingzone with lz@dev:") || !strings.Contains(r.out, "  zones/corp/lz-infosystems.bicepparam") {
		t.Errorf("dry run:\n%s", r.out)
	}
	r = h.ok("create", "infosystems", "--template", "lz", "--team", "platform", "--set", "kind=corp", "--set", "envs=prod", "--set", "ips=100")
	if r.out != "https://github.com/showmino/landingzone/pull/7\n" {
		t.Errorf("stdout %q", r.out)
	}
	if !strings.Contains(r.err, "✓ opened pull request https://github.com/showmino/landingzone/pull/7") {
		t.Errorf("stderr:\n%s", r.err)
	}
	// The int's bounds are checked before the API is asked.
	r = h.fails(2, "create", "infosystems", "--template", "lz", "--team", "platform", "--set", "kind=corp", "--set", "envs=prod", "--set", "ips=2000")
	if !strings.Contains(r.err, "at most 1024") {
		t.Errorf("bounds:\n%s", r.err)
	}
}
