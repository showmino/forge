package server

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/github"
)

// fileRelease has "lz", a file template for showmino/landingzone, and
// "away", one whose target is outside the org.
var fileRelease = staticSource{
	versions: []string{"v1.0.0"},
	trees: map[string]fs.FS{"v1.0.0": fstest.MapFS{
		"lz/template.yaml": {Data: []byte("apiVersion: forge.dev/v1\nkind: Template\nname: lz\ntitle: Landing zone\ndescription: A landing zone\n" +
			"type: file\ntarget: showmino/landingzone\nparameters:\n  - name: kind\n    type: enum\n    values: [corp, online]\n  - name: envs\n    type: list\n    values: [prod, dev]\n")},
		"lz/skeleton/zones/{{ title .Params.kind }}/lz-{{ .Params.kind }}-{{ .Service.Name }}.bicepparam": {Data: []byte("team={{ .Service.Team }} prod={{ has .Params.envs \"prod\" }}\n")},
		"away/template.yaml":  {Data: []byte("apiVersion: forge.dev/v1\nkind: Template\nname: away\ntitle: Away\ndescription: Elsewhere\ntype: file\ntarget: someone/else\n")},
		"away/skeleton/a.txt": {Data: []byte("a\n")},
	}},
}

func fileRequest() *forgev1.CreateServiceRequest {
	return &forgev1.CreateServiceRequest{
		ServiceId:  "infosystems",
		Template:   "templates/lz",
		Parameters: map[string]string{"kind": "corp", "envs": "prod"},
		OwnerTeam:  "platform-engineering",
		RequestId:  "req-lz",
	}
}

func TestCreateFromFileTemplate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := newFakeRepos()
	e := newCreateEnv(t, fileRelease, repos)

	plan, err := e.create(ctx, "writer", func() *forgev1.CreateServiceRequest { r := fileRequest(); r.ValidateOnly = true; return r }())
	if err != nil {
		t.Fatal(err)
	}
	if p := plan[len(plan)-1].GetPlan(); p.GetRepoUrl() != "https://github.com/showmino/landingzone" || strings.Join(p.GetFiles(), ",") != "zones/Corp/lz-corp-infosystems.bicepparam" {
		t.Errorf("plan: %v", p)
	}
	if len(repos.proposals) != 0 {
		t.Fatal("validate_only proposed a change")
	}

	msgs, err := e.create(ctx, "writer", fileRequest())
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, m := range msgs {
		if s := m.GetStep(); s != nil && s.GetState() == forgev1.CreateStep_STATE_DONE {
			steps = append(steps, s.GetName())
		}
	}
	if got := strings.Join(steps, ","); got != "fetch,render,pull_request" {
		t.Errorf("steps %s", got)
	}
	c := msgs[len(msgs)-1].GetChange()
	if c.GetPullRequestUrl() != "https://github.com/showmino/landingzone/pull/1" || c.GetBranch() != "forge/lz-infosystems" || c.GetRepoUrl() != "https://github.com/showmino/landingzone" {
		t.Errorf("change: %v", c)
	}
	p := repos.proposals[0]
	if p.repo != "showmino/landingzone" || p.Branch != "forge/lz-infosystems" || !strings.Contains(p.Title, "infosystems") ||
		len(p.Files) != 1 || string(p.Files[0].Content) != "team=platform-engineering prod=true\n" {
		t.Errorf("proposal: %+v", p)
	}
	// A file template creates no repo and registers no service.
	if repos.createCount() != 0 {
		t.Error("a repo was created")
	}
	if has, _ := e.catalog.has(ctx, "services/infosystems"); has {
		t.Error("a service was registered")
	}
}

func TestCreateFromFileTemplateRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := newFakeRepos()
	repos.proposeErr = github.ErrFileExists
	e := newCreateEnv(t, fileRelease, repos)
	if _, err := e.create(ctx, "writer", fileRequest()); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("file exists: %v", err)
	}
	away := fileRequest()
	away.Template, away.Parameters = "templates/away", nil
	if _, err := newCreateEnv(t, fileRelease, newFakeRepos()).create(ctx, "writer", away); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "someone/else") {
		t.Errorf("target outside the org: %v", err)
	}
	bad := fileRequest()
	bad.Parameters["envs"] = "prod,qa"
	if _, err := newCreateEnv(t, fileRelease, newFakeRepos()).create(ctx, "writer", bad); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("bad list value: %v", err)
	}
}
