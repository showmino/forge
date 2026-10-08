package template

import (
	"errors"
	"strings"
	"testing"
)

func mustParse(t *testing.T, doc string) *Template {
	t.Helper()
	tp, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func sampleBuiltins() Values {
	return Values{
		Service:  ServiceValues{Name: "billing-api", Team: "billing"},
		Repo:     RepoValues{Owner: "showmino", Name: "billing-api", URL: "https://github.com/showmino/billing-api"},
		Template: TemplateValues{Name: "svc", Version: "v1.0.0"},
	}
}

func TestResolveParams(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	got, err := tp.ResolveParams(sampleBuiltins(), map[string]string{"tier": "gold", "debug": "true"})
	if err != nil {
		t.Fatal(err)
	}
	if got["goModule"] != "github.com/showmino/billing-api" {
		t.Errorf("goModule default rendered with built-ins: %v", got["goModule"])
	}
	if got["port"] != 8080 || got["debug"] != true || got["tier"] != "gold" {
		t.Errorf("typed values: %#v", got)
	}
}

func TestResolveParamsErrors(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, validYAML)
	tests := []struct {
		raw  map[string]string
		want string
	}{
		{map[string]string{}, `parameter "tier": required`},
		{map[string]string{"tier": "gold", "nope": "x"}, `parameter "nope": unknown parameter`},
		{map[string]string{"tier": "bronze"}, `"bronze" must be one of gold, silver`},
		{map[string]string{"tier": "gold", "port": "http"}, `"http" is not an int`},
		{map[string]string{"tier": "gold", "debug": "maybe"}, `"maybe" is not a bool`},
		{map[string]string{"tier": "gold", "goModule": "Bad Module"}, `does not match`},
		{map[string]string{"tier": "gold", "goModule": strings.Repeat("a", MaxParamLen+1)}, "longer than 1024 bytes"},
	}
	for _, tt := range tests {
		_, err := tp.ResolveParams(sampleBuiltins(), tt.raw)
		var pe *ParamError
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%v: got %v, want a ParamError containing %q", tt.raw, err, tt.want)
		}
	}
}

func TestResolveParamsBadDefaultTemplate(t *testing.T) {
	t.Parallel()
	tp := mustParse(t, strings.Replace(validYAML, "{% .Repo.Name %}", "{% .Repo.Nope %}", 1))
	if _, err := tp.ResolveParams(sampleBuiltins(), map[string]string{"tier": "gold"}); err == nil || !strings.Contains(err.Error(), "goModule") {
		t.Errorf("default using an unknown field: %v", err)
	}
}
