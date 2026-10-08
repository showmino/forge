package server

import (
	"context"
	"io/fs"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
	"github.com/showmino/forge/gen/forge/v1/forgev1connect"
	"github.com/showmino/forge/internal/github"
)

// fakeRepos is an in-memory GitHub org.
type fakeRepos struct {
	mu        sync.Mutex
	repos     map[string][]github.File // name -> pushed files (nil until pushed)
	creates   int
	createErr error
	pushErr   error
	deleteErr error
	onCreate  func() // runs inside CreateOrgRepo, for disconnect tests
	pushGate  chan struct{}
}

func newFakeRepos(existing ...string) *fakeRepos {
	f := &fakeRepos{repos: map[string][]github.File{}}
	for _, n := range existing {
		f.repos[n] = nil
	}
	return f
}

func (f *fakeRepos) RepoExists(_ context.Context, owner, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.repos[name]
	return ok && owner == "showmino", nil
}

func (f *fakeRepos) CreateOrgRepo(_ context.Context, org, name, _ string) (*github.Repo, error) {
	if f.onCreate != nil {
		f.onCreate()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	if _, ok := f.repos[name]; ok {
		return nil, github.ErrRepoExists
	}
	f.creates++
	f.repos[name] = nil
	return &github.Repo{FullName: org + "/" + name, DefaultBranch: "main"}, nil
}

func (f *fakeRepos) DeleteRepo(_ context.Context, _, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.repos, name)
	return nil
}

func (f *fakeRepos) PushFiles(_ context.Context, _, repo, _, _ string, files []github.File) (string, error) {
	if f.pushGate != nil {
		<-f.pushGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pushErr != nil {
		return "", f.pushErr
	}
	f.repos[repo] = files
	return "abc1234def", nil
}

func (f *fakeRepos) createCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

// createRelease has template "svc" (a required enum, an int default, an
// executable file) and "dep" (its forge.yaml depends on a missing service).
var createRelease = staticSource{
	versions: []string{"v1.0.0"},
	trees: map[string]fs.FS{"v1.0.0": fstest.MapFS{
		"svc/template.yaml": {Data: []byte(templateYAML("svc", "go", "parameters:\n  - name: port\n    type: int\n    default: 8080\n  - name: tier\n    type: enum\n    values: [gold, silver]\n"))},
		"svc/skeleton/forge.yaml": {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n" +
			"{{- if .Service.Owners }}\n  owners:\n{{- range .Service.Owners }}\n    - {{ . }}\n{{- end }}\n{{- end }}\n  lifecycle: experimental\n  repo: {{ .Repo.URL }}\n")},
		"svc/skeleton/main.go":    {Data: []byte("package main // {{ .Params.port }} {{ .Params.tier }}\n")},
		"svc/skeleton/run.sh":     {Data: []byte("#!/bin/sh\n"), Mode: 0o755},
		"dep/template.yaml":       {Data: []byte(templateYAML("dep", "go", ""))},
		"dep/skeleton/forge.yaml": {Data: []byte("apiVersion: forge.dev/v1\nservice:\n  name: {{ .Service.Name }}\n  team: {{ .Service.Team }}\n  dependencies: [missing]\n")},
	}},
}

type createEnv struct {
	catalog *Catalog
	repos   *fakeRepos
	client  forgev1connect.ServiceCatalogServiceClient
}

func newCreateEnv(t *testing.T, src TemplateSource, repos *fakeRepos) *createEnv {
	t.Helper()
	e := &createEnv{catalog: NewCatalog(nil), repos: repos}
	opts := Options{
		Authenticator: StaticAuthenticator{
			"writer": {Subject: "ana", Groups: []string{"platform"}},
			"reader": {Subject: "bo", Groups: []string{"dev"}},
		},
		Authorizer: Authorizer{WriterGroups: []string{"platform"}},
		Catalog:    e.catalog,
		Templates:  src,
	}
	if repos != nil {
		opts.Repos = repos
	}
	h, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e.client = forgev1connect.NewServiceCatalogServiceClient(srv.Client(), srv.URL)
	return e
}

// create runs CreateService as token and collects every message.
func (e *createEnv) create(ctx context.Context, token string, req *forgev1.CreateServiceRequest) ([]*forgev1.CreateServiceResponse, error) {
	r := connect.NewRequest(req)
	r.Header().Set("Authorization", "Bearer "+token)
	stream, err := e.client.CreateService(ctx, r)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var msgs []*forgev1.CreateServiceResponse
	for stream.Receive() {
		msgs = append(msgs, stream.Msg())
	}
	return msgs, stream.Err()
}

func validRequest() *forgev1.CreateServiceRequest {
	return &forgev1.CreateServiceRequest{
		ServiceId:  "payments",
		Template:   "templates/svc",
		Parameters: map[string]string{"tier": "gold"},
		OwnerTeam:  "billing",
		Owners:     []string{"ana@example.com"},
		RequestId:  "req-1",
	}
}

func TestCreatePreflight(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	with := func(mut func(*forgev1.CreateServiceRequest)) *forgev1.CreateServiceRequest {
		r := validRequest()
		mut(r)
		return r
	}
	tests := []struct {
		name  string
		env   *createEnv
		token string
		req   *forgev1.CreateServiceRequest
		want  connect.Code
		msg   string
	}{
		{"anonymous", newCreateEnv(t, createRelease, newFakeRepos()), "nope", validRequest(), connect.CodeUnauthenticated, ""},
		{"reader", newCreateEnv(t, createRelease, newFakeRepos()), "reader", validRequest(), connect.CodePermissionDenied, "services.write"},
		{"bad id", newCreateEnv(t, createRelease, newFakeRepos()), "writer", with(func(r *forgev1.CreateServiceRequest) { r.ServiceId = "Pay_ments" }), connect.CodeInvalidArgument, "service_id"},
		{"bad template", newCreateEnv(t, createRelease, newFakeRepos()), "writer", with(func(r *forgev1.CreateServiceRequest) { r.Template = "templates/Bad" }), connect.CodeInvalidArgument, "template"},
		{"no team", newCreateEnv(t, createRelease, newFakeRepos()), "writer", with(func(r *forgev1.CreateServiceRequest) { r.OwnerTeam = " " }), connect.CodeInvalidArgument, "owner_team"},
		{"empty owner", newCreateEnv(t, createRelease, newFakeRepos()), "writer", with(func(r *forgev1.CreateServiceRequest) { r.Owners = []string{""} }), connect.CodeInvalidArgument, "owners"},
		{"no templates", newCreateEnv(t, nil, newFakeRepos()), "writer", validRequest(), connect.CodeFailedPrecondition, "templates are not configured"},
		{"no GitHub", newCreateEnv(t, createRelease, nil), "writer", validRequest(), connect.CodeFailedPrecondition, "no GitHub App"},
		{"repo taken on GitHub", newCreateEnv(t, createRelease, newFakeRepos("payments")), "writer", validRequest(), connect.CodeAlreadyExists, "github.com/showmino/payments already exists"},
	}
	for _, tt := range tests {
		_, err := tt.env.create(ctx, tt.token, tt.req)
		if connect.CodeOf(err) != tt.want || !strings.Contains(err.Error(), tt.msg) {
			t.Errorf("%s: got %v, want %v containing %q", tt.name, err, tt.want, tt.msg)
		}
	}

	// A name taken in the catalog is refused before GitHub is touched.
	e := newCreateEnv(t, createRelease, newFakeRepos())
	register(t, e.catalog, "payments")
	if _, err := e.create(ctx, "writer", validRequest()); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("catalog conflict: %v", err)
	}
	if e.repos.createCount() != 0 {
		t.Error("a conflict must not create a repo")
	}
}

func TestCreateWithoutGitHubCanStillValidate(t *testing.T) {
	t.Parallel()
	e := newCreateEnv(t, createRelease, nil)
	r := validRequest()
	r.ValidateOnly = true
	msgs, err := e.create(context.Background(), "writer", r)
	if err != nil {
		t.Fatal(err)
	}
	if last := msgs[len(msgs)-1]; last.GetPlan() == nil {
		t.Errorf("validate_only without GitHub must still plan: %v", last)
	}
}
