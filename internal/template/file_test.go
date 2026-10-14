package template

import (
	"errors"
	"strings"
	"testing"
)

const fileYAML = `apiVersion: forge.dev/v1
kind: Template
name: landing-zone
title: Landing zone
description: A subscription and its network
type: file
target: showmino/landingzone
delimiters: ["{%", "%}"]
parameters:
  - name: type
    label: Landing zone type
    type: enum
    values: [corp, online]
  - name: environments
    label: Environments
    type: list
    values: [prod, staging, dev, test]
    default: [prod]
  - name: ipAddresses
    label: IP addresses needed
    type: int
    min: 1
    max: 1024
  - name: costCenter
    label: Cost center
    type: string
    group: Tags
    placeholder: "60168"
`

const fileSkeleton = "using '../../main_{% .Params.type %}.bicep'\n" +
	"param testEnvironment = {% has .Params.environments \"test\" %}\n" +
	"param prodEnvironment = {% has .Params.environments \"prod\" %}\n" +
	"param ipamNumberOfIpAddresses = '{% .Params.ipAddresses %}'\n"

func TestFileTemplate(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, fileYAML)
	if f := tp.Validate(); len(f) != 0 {
		t.Fatalf("findings: %v", f)
	}
	if !tp.IsFile() || tp.Target != "showmino/landingzone" {
		t.Errorf("type %q target %q", tp.Type, tp.Target)
	}
	ip := tp.Parameters[2]
	if ip.Label != "IP addresses needed" || ip.Min == nil || *ip.Min != 1 || *ip.Max != 1024 {
		t.Errorf("ipAddresses: %+v", ip)
	}
	if cc := tp.Parameters[3]; cc.Group != "Tags" || cc.Placeholder != "60168" {
		t.Errorf("costCenter: %+v", cc)
	}
	if d, _ := tp.Parameters[1].DefaultString(); d != "prod" {
		t.Errorf("list default %q", d)
	}

	got, err := tp.ResolveParams(sampleBuiltins(), map[string]string{"type": "corp", "environments": "test, dev", "ipAddresses": "100", "costCenter": "60168"})
	if err != nil {
		t.Fatal(err)
	}
	if env, ok := got["environments"].([]string); !ok || strings.Join(env, ",") != "test,dev" {
		t.Errorf("environments %#v", got["environments"])
	}
	files, err := tp.Render(repo(map[string]string{"landingZones/{% title .Params.type %}/lz-{% .Params.type %}-{% .Service.Name %}.bicepparam": fileSkeleton}), Values{Service: ServiceValues{Name: "infosystems"}, Params: got})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "landingZones/Corp/lz-corp-infosystems.bicepparam" {
		t.Fatalf("files %+v", files)
	}
	want := "using '../../main_corp.bicep'\nparam testEnvironment = true\nparam prodEnvironment = false\nparam ipamNumberOfIpAddresses = '100'\n"
	if string(files[0].Data) != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", files[0].Data, want)
	}
}

func TestFileTemplateParamErrors(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, fileYAML)
	for _, tt := range []struct {
		raw  map[string]string
		want string
	}{
		{map[string]string{"type": "corp", "ipAddresses": "0", "costCenter": "1"}, "at least 1"},
		{map[string]string{"type": "corp", "ipAddresses": "2000", "costCenter": "1"}, "at most 1024"},
		{map[string]string{"type": "corp", "ipAddresses": "5", "costCenter": "1", "environments": "prod,qa"}, `"qa" must be one of prod, staging, dev, test`},
		{map[string]string{"type": "corp", "ipAddresses": "5", "costCenter": "1", "environments": "prod,prod"}, `"prod" is given twice`},
	} {
		_, err := tp.ResolveParams(sampleBuiltins(), tt.raw)
		var pe *ParamError
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: got %v, want %q", tt.raw, err, tt.want)
		}
	}
	// An empty list is a value: no environment ticked.
	got, err := tp.ResolveParams(sampleBuiltins(), map[string]string{"type": "corp", "ipAddresses": "5", "costCenter": "1", "environments": ""})
	if err != nil || len(got["environments"].([]string)) != 0 {
		t.Errorf("empty list: %#v %v", got["environments"], err)
	}
	// Values are one line: a newline could start a line of its own in the file.
	if _, err := tp.ResolveParams(sampleBuiltins(), map[string]string{"type": "corp", "ipAddresses": "5", "costCenter": "1\nparam x = 1"}); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Errorf("newline: %v", err)
	}
}

func TestBicepString(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, fileYAML)
	got, err := tp.renderString("x", `'{% bicepString .Params.s %}'`, Values{Params: map[string]any{"s": `O'Brien \ ${env()}`}})
	if want := `'O\'Brien \\ \${env()}'`; err != nil || got != want {
		t.Errorf("got %s %v, want %s", got, err, want)
	}
}

func TestValidateFileTemplate(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, from, to, want string }{
		{"type", "type: file", "type: library", `type must be "service" or "file"`},
		{"no target", "target: showmino/landingzone\n", "", "a file template needs target"},
		{"bad target", "target: showmino/landingzone", "target: landingzone", `target "landingzone" must be owner/repo`},
		{"target on a service", "type: file", "type: service", "target is only for a file template"},
		{"options", "type: file", "type: file\noptions: [replicas]", "options are only for a service template"},
		{"min on a string", "    group: Tags", "    group: Tags\n    min: 1", "min and max are only allowed for type int"},
		{"min above max", "    min: 1\n    max: 1024", "    min: 10\n    max: 5", "min is above max"},
		{"list values", "    values: [prod, staging, dev, test]", "    values: []", "type list needs values"},
		{"list default", "    default: [prod]", "    default: [qa]", `default "qa" must be one of`},
		{"value with a comma", "    values: [prod, staging, dev, test]", "    values: [prod, \"a,b\"]", "must not be empty, contain a comma"},
		{"default out of bounds", "    min: 1\n    max: 1024", "    min: 1\n    max: 1024\n    default: 2000", "default 2000 is outside min and max"},
		{"dot repo", "target: showmino/landingzone", "target: showmino/..", `target "showmino/.." must be owner/repo`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			doc := strings.Replace(fileYAML, tt.from, tt.to, 1)
			if doc == fileYAML {
				t.Fatalf("replacement %q not found", tt.from)
			}
			tp := mustParse(t, doc)
			if got := messages(tp.Validate()); !strings.Contains(got, tt.want) {
				t.Errorf("findings %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLintFileTemplate(t *testing.T) {
	t.Parallel()
	files, findings := Lint(repo(map[string]string{
		"landing-zone/template.yaml": fileYAML,
		"landing-zone/skeleton/landingZones/{% title .Params.type %}/lz-{% .Params.type %}-{% .Service.Name %}.bicepparam": fileSkeleton,
	}), "landing-zone")
	if len(findings) > 0 {
		t.Fatalf("a file template needs no forge.yaml: %s", messages(findings))
	}
	if len(files) != 1 || files[0].Path != "landingZones/Corp/lz-corp-sample-service.bicepparam" {
		t.Errorf("files %+v", files)
	}
	_, findings = Lint(repo(map[string]string{
		"landing-zone/template.yaml":                    fileYAML,
		"landing-zone/skeleton/.github/workflows/x.yml": "on: push\n",
	}), "landing-zone")
	if !strings.Contains(messages(findings), "may not write under .git or .github") {
		t.Errorf("a workflow in a file template: %s", messages(findings))
	}
}
