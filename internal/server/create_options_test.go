package server

import (
	"context"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/internal/manifest"
)

// optionsRelease has "web", which offers replicas, env, secrets and jobs and
// already asks for one replica of its own, and "plain", which offers none.
var optionsRelease = staticSource{
	versions: []string{"v1.0.0"},
	trees: map[string]fs.FS{"v1.0.0": fstest.MapFS{
		"web/template.yaml": {Data: []byte(templateYAML("web", "web", "options: [replicas, env, secrets, jobs]\n"))},
		"web/skeleton/forge.yaml": {Data: []byte("# The service's manifest.\napiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n" +
			"run:\n  port: 3000 # the app's port\n  replicas: 1\n")},
		"plain/template.yaml":       {Data: []byte(templateYAML("plain", "go", ""))},
		"plain/skeleton/forge.yaml": {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n")},
	}},
}

func TestCreateRunOptions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	req := func(tmpl string, ro *forgev1.RunOptions) *forgev1.CreateServiceRequest {
		return &forgev1.CreateServiceRequest{ServiceId: "shop", Template: "templates/" + tmpl, OwnerTeam: "billing", RunOptions: ro}
	}
	e := newCreateEnv(t, optionsRelease, newFakeRepos())
	_, err := e.create(ctx, "writer", req("web", &forgev1.RunOptions{
		Replicas: 3,
		Env:      map[string]string{"LOG_LEVEL": "info"},
		Secrets:  []*forgev1.RunSecret{{Name: "stripe", Item: "vaults/Kube/items/shop-stripe", Env: map[string]string{"STRIPE_KEY": "credential"}}},
		Jobs:     []*forgev1.RunJob{{Name: "cleanup", Schedule: "0 3 * * *", Command: []string{"node", "cleanup.js"}}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	files, _ := e.repos.files("shop")
	var data []byte
	for _, f := range files {
		if f.Path == "forge.yaml" {
			data = f.Content
		}
	}
	m, err := manifest.Parse(data)
	if err != nil {
		t.Fatalf("%v:\n%s", err, data)
	}
	if r := m.Run; r.Port != 3000 || r.Replicas != 3 || r.Env["LOG_LEVEL"] != "info" || r.Secrets[0].Item != "vaults/Kube/items/shop-stripe" || r.Jobs[0].Name != "cleanup" {
		t.Errorf("run: %+v", r)
	}
	// The template's own lines and comments stay.
	if s := string(data); !strings.Contains(s, "# The service's manifest.") || !strings.Contains(s, "port: 3000 # the app's port") {
		t.Errorf("forge.yaml:\n%s", s)
	}

	for name, tc := range map[string]struct {
		tmpl string
		ro   *forgev1.RunOptions
		msg  string
	}{
		"not offered":          {"web", &forgev1.RunOptions{AutoscalingMax: 4}, "does not offer the run option autoscaling (it offers: replicas, env, secrets, jobs)"},
		"template offers none": {"plain", &forgev1.RunOptions{Replicas: 2}, "(it offers: none)"},
		"against the rules":    {"web", &forgev1.RunOptions{Replicas: 11}, "run.replicas 11 must be 1 to 10"},
		"bad job":              {"web", &forgev1.RunOptions{Jobs: []*forgev1.RunJob{{Name: "c", Schedule: "soon", Command: []string{"x"}}}}, "schedule"},
	} {
		r := req(tc.tmpl, tc.ro)
		r.ServiceId = "shop-" + strings.ReplaceAll(name, " ", "-")
		if _, err := e.create(ctx, "writer", r); connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%s: %v, want %q", name, err, tc.msg)
		}
	}
}

func TestTemplateOptionsInAPI(t *testing.T) {
	t.Parallel()
	_, tree, err := resolveRelease(context.Background(), optionsRelease, "")
	if err != nil {
		t.Fatal(err)
	}
	tp, err := templateFromRelease(tree, "web", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := templateProto(tp, "v1.0.0").GetOptions(); strings.Join(got, ",") != "replicas,env,secrets,jobs" {
		t.Errorf("options: %v", got)
	}
}

func TestCreateNetworkOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newCreateEnv(t, optionsRelease, newFakeRepos())
	// Every service template allows it, also one that offers no run options.
	if _, err := e.create(ctx, "writer", &forgev1.CreateServiceRequest{ServiceId: "intranet", Template: "templates/plain", OwnerTeam: "billing", RunOptions: &forgev1.RunOptions{NetworkOnly: true}}); err != nil {
		t.Fatal(err)
	}
	files, _ := e.repos.files("intranet")
	for _, f := range files {
		if f.Path != "forge.yaml" {
			continue
		}
		m, err := manifest.Parse(f.Content)
		if err != nil || m.RunDomain() != "none" {
			t.Errorf("forge.yaml: %v\n%s", err, f.Content)
		}
	}
}
