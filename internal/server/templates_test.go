package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"connectrpc.com/connect"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// staticSource serves fixed trees, or fails the way a source can.
type staticSource struct {
	versions []string
	trees    map[string]fs.FS
	err      error
}

func (s staticSource) Versions(context.Context) ([]string, error) { return s.versions, s.err }

func (s staticSource) Fetch(_ context.Context, v string) (fs.FS, error) {
	if s.err != nil {
		return nil, s.err
	}
	if t, ok := s.trees[v]; ok {
		return t, nil
	}
	return nil, ErrVersionNotFound
}

// fetchFails knows one version but cannot fetch it.
type fetchFails struct{ err error }

func (fetchFails) Versions(context.Context) ([]string, error)     { return []string{"v1.0.0"}, nil }
func (f fetchFails) Fetch(context.Context, string) (fs.FS, error) { return nil, f.err }

func templateYAML(name, tags string, params string) string {
	return "apiVersion: forge.dev/v1\nkind: Template\nname: " + name + "\ntitle: " + name + " title\ndescription: " + name + " description\ntags: [" + tags + "]\nowner: platform\n" + params
}

func release(files map[string]string) fs.FS {
	m := fstest.MapFS{}
	for k, v := range files {
		m[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return m
}

func newTemplateService(src TemplateSource) (*templateService, *bytes.Buffer) {
	var logs bytes.Buffer
	return &templateService{src: src, log: slog.New(slog.NewTextHandler(&logs, nil))}, &logs
}

var twoReleases = staticSource{
	versions: []string{"v1.1.0", "v1.0.0"},
	trees: map[string]fs.FS{
		"v1.1.0": release(map[string]string{
			"go-grpc/template.yaml":  templateYAML("go-grpc", "go, grpc", "parameters:\n  - name: port\n    type: int\n    default: 8080\n    description: Port\n  - name: tier\n    type: enum\n    values: [gold, silver]\n  - name: module\n    type: string\n    pattern: '^[a-z]+$'\n"),
			"node-api/template.yaml": templateYAML("node-api", "node", ""),
			"broken/template.yaml":   "apiVersion: forge.dev/v1\nkind: Template\nname: broken\nnope: 1\n",
			"README.md":              "not a template",
		}),
		"v1.0.0": release(map[string]string{"go-grpc/template.yaml": templateYAML("go-grpc", "go", "")}),
	},
}

func TestListTemplates(t *testing.T) {
	t.Parallel()
	s, logs := newTemplateService(twoReleases)
	resp, err := s.ListTemplates(writerCtx(), connect.NewRequest(&forgev1.ListTemplatesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tp := range resp.Msg.GetTemplates() {
		names = append(names, tp.GetName()+"@"+tp.GetVersion())
	}
	if strings.Join(names, ",") != "templates/go-grpc@v1.1.0,templates/node-api@v1.1.0" {
		t.Errorf("newest release, broken one skipped: %v", names)
	}
	if !strings.Contains(logs.String(), "broken") {
		t.Errorf("a broken template must be logged: %q", logs.String())
	}

	resp, err = s.ListTemplates(writerCtx(), connect.NewRequest(&forgev1.ListTemplatesRequest{Tag: "node"}))
	if err != nil || len(resp.Msg.GetTemplates()) != 1 || resp.Msg.GetTemplates()[0].GetName() != "templates/node-api" {
		t.Errorf("tag filter: %v %v", resp, err)
	}
	resp, err = s.ListTemplates(writerCtx(), connect.NewRequest(&forgev1.ListTemplatesRequest{Version: "v1.0.0"}))
	if err != nil || len(resp.Msg.GetTemplates()) != 1 || resp.Msg.GetTemplates()[0].GetVersion() != "v1.0.0" {
		t.Errorf("pinned version: %v %v", resp, err)
	}
}

func TestGetTemplate(t *testing.T) {
	t.Parallel()
	s, _ := newTemplateService(twoReleases)
	resp, err := s.GetTemplate(writerCtx(), connect.NewRequest(&forgev1.GetTemplateRequest{Name: "templates/go-grpc"}))
	if err != nil {
		t.Fatal(err)
	}
	tp := resp.Msg.GetTemplate()
	if tp.GetTitle() != "go-grpc title" || tp.GetOwner() != "platform" || len(tp.GetParameters()) != 3 {
		t.Fatalf("template: %v", tp)
	}
	port, tier, module := tp.GetParameters()[0], tp.GetParameters()[1], tp.GetParameters()[2]
	if port.GetType() != forgev1.TemplateParameter_TYPE_INT || port.GetRequired() || port.GetDefaultValue() != "8080" || port.GetDescription() != "Port" {
		t.Errorf("port: %v", port)
	}
	if tier.GetType() != forgev1.TemplateParameter_TYPE_ENUM || !tier.GetRequired() || strings.Join(tier.GetValues(), ",") != "gold,silver" {
		t.Errorf("tier: %v", tier)
	}
	if module.GetPattern() != "^[a-z]+$" || module.GetType() != forgev1.TemplateParameter_TYPE_STRING {
		t.Errorf("module: %v", module)
	}
	// A bare name works too, as for services.
	if _, err := s.GetTemplate(writerCtx(), connect.NewRequest(&forgev1.GetTemplateRequest{Name: "node-api"})); err != nil {
		t.Errorf("bare name: %v", err)
	}
}

func TestTemplateErrors(t *testing.T) {
	t.Parallel()
	get := func(src TemplateSource, ctx context.Context, name, version string) error {
		s, _ := newTemplateService(src)
		_, err := s.GetTemplate(ctx, connect.NewRequest(&forgev1.GetTemplateRequest{Name: name, Version: version}))
		return err
	}
	ctx := writerCtx()
	tests := []struct {
		name string
		err  error
		want connect.Code
		msg  string
	}{
		{"unknown template", get(twoReleases, ctx, "templates/nope", ""), connect.CodeNotFound, "templates/nope not found in v1.1.0"},
		{"unknown version", get(twoReleases, ctx, "templates/go-grpc", "v9.0.0"), connect.CodeNotFound, "v9.0.0"},
		{"broken template", get(twoReleases, ctx, "templates/broken", ""), connect.CodeFailedPrecondition, "nope"},
		{"bad name", get(twoReleases, ctx, "templates/Bad_Name", ""), connect.CodeInvalidArgument, "must look like templates/{template}"},
		{"not configured", get(nil, ctx, "templates/go-grpc", ""), connect.CodeFailedPrecondition, "templates are not configured"},
		{"no releases", get(staticSource{}, ctx, "templates/go-grpc", ""), connect.CodeFailedPrecondition, "no release tags"},
		{"source down", get(staticSource{err: errors.New("github: 503")}, ctx, "templates/go-grpc", ""), connect.CodeUnavailable, "503"},
		{"source down on fetch", get(fetchFails{fmt.Errorf("download: %w", errors.New("timeout"))}, ctx, "templates/go-grpc", ""), connect.CodeUnavailable, "template source unavailable"},
		{"invalid release", get(fetchFails{fmt.Errorf("x@v1.0.0: %w: symlink", ErrInvalidRelease)}, ctx, "templates/go-grpc", ""), connect.CodeFailedPrecondition, "release is invalid"},
		{"anonymous", get(twoReleases, context.Background(), "templates/go-grpc", ""), connect.CodeUnauthenticated, ""},
	}
	for _, tt := range tests {
		if connect.CodeOf(tt.err) != tt.want || !strings.Contains(tt.err.Error(), tt.msg) {
			t.Errorf("%s: got %v, want %v containing %q", tt.name, tt.err, tt.want, tt.msg)
		}
	}
}

// The service is reachable through New with any authenticated caller.
func TestTemplateServiceWired(t *testing.T) {
	t.Parallel()
	if _, err := New(Options{Authenticator: StaticAuthenticator{}, Templates: twoReleases}); err != nil {
		t.Fatal(err)
	}
}
