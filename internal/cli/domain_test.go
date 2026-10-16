package cli

import (
	"strings"
	"testing"

	"github.com/showmino/forge/internal/server"
)

var testDomains = []server.PublicDomain{{Name: "showmino.com", Zone: "showmino-com"}, {Name: "forgegate.io", Zone: "forgegate-io"}}

func TestDomainList(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.publicDomains = testDomains
	h.serveDeploys(nil)
	h.env["FORGE_TOKEN"] = readerToken // reading needs no team

	r := h.ok("domain", "list")
	if r.out != "DOMAIN         DEFAULT\nshowmino.com   yes\nforgegate.io   -\n" {
		t.Errorf("table:\n%q", r.out)
	}
	if r := h.ok("domain", "ls", "-o", "name"); r.out != "showmino.com\nforgegate.io\n" {
		t.Errorf("-o name: %q", r.out)
	}
	if r := h.ok("domain", "list", "-o", "json"); !strings.Contains(r.out, `"name": "forgegate.io"`) || !strings.Contains(r.out, `"isDefault": true`) {
		t.Errorf("-o json:\n%s", r.out)
	}
}

func TestDomainListNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serveDeploys(nil)
	r := h.ok("domain", "list")
	if r.out != "" || !strings.Contains(r.err, "no public domains") {
		t.Errorf("%s", r)
	}
	if r := h.ok("domain", "list", "-o", "json"); !strings.Contains(r.out, `"domains": []`) {
		t.Errorf("-o json: %q", r.out)
	}
}

func TestCreateWithDomain(t *testing.T) {
	t.Parallel()
	h, _ := createHarness(t)
	h.deploys = server.NewDeploys(server.DeployOptions{PublicDomains: testDomains})
	h.startAPI(h.authn)
	h.ok("create", "gate", "--template", "svc", "--team", "platform", "--set", "tier=gold", "--domain", "forgegate.io")
	if r := h.fails(2, "create", "other", "--template", "svc", "--team", "platform", "--set", "tier=gold", "--domain", "example.org"); !strings.Contains(r.err, "use showmino.com or forgegate.io") {
		t.Errorf("unknown domain:\n%s", r)
	}
	if r := h.fails(2, "create", "other", "--template", "svc", "--team", "platform", "--set", "tier=gold", "--domain", "forgegate.io", "--internal"); !strings.Contains(r.err, "--domain") {
		t.Errorf("--domain with --internal:\n%s", r)
	}
}
