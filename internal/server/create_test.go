package server

import (
	"context"
	"errors"
	"io/fs"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

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

func (f *fakeRepos) PushFiles(ctx context.Context, _, repo, _, _ string, files []github.File) (string, error) {
	if f.pushGate != nil {
		<-f.pushGate
	}
	if err := ctx.Err(); err != nil { // like a real HTTP call
		return "", err
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

var errBoom = errors.New("boom")

func (f *fakeRepos) files(name string) ([]github.File, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	files, ok := f.repos[name]
	return files, ok
}

// steps returns "name:state:detail" for each step message and the last message.
func steps(msgs []*forgev1.CreateServiceResponse) ([]string, *forgev1.CreateServiceResponse) {
	var out []string
	for _, m := range msgs {
		if s := m.GetStep(); s != nil {
			out = append(out, s.GetName()+":"+strings.TrimPrefix(s.GetState().String(), "STATE_")+":"+s.GetDetail())
		}
	}
	if len(msgs) == 0 {
		return out, nil
	}
	return out, msgs[len(msgs)-1]
}

func TestCreateService(t *testing.T) {
	t.Parallel()
	e := newCreateEnv(t, createRelease, newFakeRepos())
	msgs, err := e.create(context.Background(), "writer", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	got, last := steps(msgs)
	want := []string{
		"fetch:STARTED:", "fetch:DONE:svc@v1.0.0",
		"render:STARTED:", "render:DONE:3 files",
		"create_repo:STARTED:", "create_repo:DONE:github.com/showmino/payments",
		"push:STARTED:", "push:DONE:abc1234 (CI started)",
		"register:STARTED:", "register:DONE:services/payments",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("steps:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	svc := last.GetService()
	if svc.GetName() != "services/payments" || svc.GetOwnerTeam() != "billing" || svc.GetRepoUrl() != "https://github.com/showmino/payments" ||
		svc.GetLifecycle() != forgev1.Lifecycle_LIFECYCLE_EXPERIMENTAL || strings.Join(svc.GetOwners(), ",") != "ana@example.com" {
		t.Errorf("service: %v", svc)
	}
	files, _ := e.repos.files("payments")
	byPath := map[string]github.File{}
	for _, f := range files {
		byPath[f.Path] = f
	}
	if string(byPath["main.go"].Content) != "package main // 8080 gold\n" || !byPath["run.sh"].Executable || byPath["main.go"].Executable {
		t.Errorf("pushed files: %+v", files)
	}
	if !e.catalog.has("services/payments") {
		t.Error("service not registered")
	}
}

func TestCreateValidateOnly(t *testing.T) {
	t.Parallel()
	e := newCreateEnv(t, createRelease, newFakeRepos())
	r := validRequest()
	r.ValidateOnly = true
	msgs, err := e.create(context.Background(), "writer", r)
	if err != nil {
		t.Fatal(err)
	}
	got, last := steps(msgs)
	if strings.Join(got, ",") != "fetch:STARTED:,fetch:DONE:svc@v1.0.0,render:STARTED:,render:DONE:3 files" {
		t.Errorf("steps: %v", got)
	}
	p := last.GetPlan()
	if p.GetTemplateVersion() != "v1.0.0" || p.GetRepoUrl() != "https://github.com/showmino/payments" || strings.Join(p.GetFiles(), ",") != "forge.yaml,main.go,run.sh" {
		t.Errorf("plan: %v", p)
	}
	if _, ok := e.repos.files("payments"); ok || e.catalog.has("services/payments") {
		t.Error("validate_only must not create anything")
	}
}

func TestCreateBadParameters(t *testing.T) {
	t.Parallel()
	e := newCreateEnv(t, createRelease, newFakeRepos())
	for name, params := range map[string]map[string]string{
		"missing required": {},
		"wrong type":       {"tier": "gold", "port": "http"},
		"unknown":          {"tier": "gold", "nope": "x"},
	} {
		r := validRequest()
		r.Parameters = params
		_, err := e.create(context.Background(), "writer", r)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	if e.repos.createCount() != 0 {
		t.Error("bad parameters must not create a repo")
	}
}

func TestCreateFailuresRollBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	pushFails := newFakeRepos()
	pushFails.pushErr = errBoom
	e := newCreateEnv(t, createRelease, pushFails)
	msgs, err := e.create(ctx, "writer", validRequest())
	if err != nil {
		t.Fatal(err)
	}
	_, last := steps(msgs)
	if f := last.GetFailure(); f.GetStep() != "push" || !f.GetRolledBack() || !strings.Contains(f.GetMessage(), "boom") {
		t.Errorf("push failure: %v", last)
	}
	if _, ok := pushFails.files("payments"); ok {
		t.Error("the repo must be deleted again")
	}

	cannotDelete := newFakeRepos()
	cannotDelete.pushErr, cannotDelete.deleteErr = errBoom, errBoom
	msgs, _ = newCreateEnv(t, createRelease, cannotDelete).create(ctx, "writer", validRequest())
	if _, last := steps(msgs); last.GetFailure().GetRolledBack() {
		t.Error("rolled_back must be false when the delete failed")
	}

	registerFails := newFakeRepos()
	e = newCreateEnv(t, createRelease, registerFails)
	r := validRequest()
	r.Template, r.Parameters = "templates/dep", nil
	msgs, err = e.create(ctx, "writer", r)
	if err != nil {
		t.Fatal(err)
	}
	if _, last := steps(msgs); last.GetFailure().GetStep() != "register" || !last.GetFailure().GetRolledBack() {
		t.Errorf("register failure: %v", last)
	}

	raced := newFakeRepos()
	raced.createErr = github.ErrRepoExists
	if _, err := newCreateEnv(t, createRelease, raced).create(ctx, "writer", validRequest()); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("repo created by someone else meanwhile: %v", err)
	}
	down := newFakeRepos()
	down.createErr = errBoom
	if _, err := newCreateEnv(t, createRelease, down).create(ctx, "writer", validRequest()); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("GitHub down at create_repo: %v", err)
	}
}

func TestCreateIdempotency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repos := newFakeRepos()
	e := newCreateEnv(t, createRelease, repos)
	if _, err := e.create(ctx, "writer", validRequest()); err != nil {
		t.Fatal(err)
	}
	msgs, err := e.create(ctx, "writer", validRequest()) // same request_id
	if err != nil || len(msgs) != 1 || msgs[0].GetService().GetName() != "services/payments" {
		t.Errorf("replay: %v %v", msgs, err)
	}
	if n := repos.createCount(); n != 1 {
		t.Errorf("%d repos created, want 1", n)
	}

	// A retry while the first attempt still runs is refused.
	slow := newFakeRepos()
	slow.pushGate = make(chan struct{})
	e = newCreateEnv(t, createRelease, slow)
	done := make(chan error, 1)
	go func() {
		_, err := e.create(ctx, "writer", validRequest())
		done <- err
	}()
	for slow.createCount() != 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := e.create(ctx, "writer", validRequest()); connect.CodeOf(err) != connect.CodeAborted {
		t.Errorf("retry while running: %v", err)
	}
	close(slow.pushGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A client that disconnects after create_repo does not leave a half-made service.
func TestCreateSurvivesDisconnect(t *testing.T) {
	t.Parallel()
	repos := newFakeRepos()
	e := newCreateEnv(t, createRelease, repos)
	ctx, cancel := context.WithCancel(context.Background())
	repos.onCreate = func() { // the client goes away while the repo is being created
		cancel()
		time.Sleep(200 * time.Millisecond) // until the server has noticed
	}
	_, _ = e.create(ctx, "writer", validRequest())
	// The server finishes in the background; wait for the registration.
	for i := 0; !e.catalog.has("services/payments"); i++ {
		if i > 200 {
			t.Fatal("the create did not finish after the client left")
		}
		time.Sleep(10 * time.Millisecond)
	}
	msgs, err := e.create(context.Background(), "writer", validRequest()) // the CLI's retry
	if err != nil || len(msgs) != 1 || msgs[0].GetService() == nil {
		t.Errorf("retry after disconnect: %v %v", msgs, err)
	}
}
