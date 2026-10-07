package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mapEnv(m map[string]string) Env {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func sample() *Config {
	return &Config{
		CurrentContext: "staging-eu",
		Contexts: []Context{
			{Name: "staging-eu", API: "https://forge.staging.example.com", Namespace: "billing"},
			{Name: "prod", API: "https://forge.example.com", Namespace: "billing"},
		},
		Preferences: Preferences{Output: "table"},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "forge", "config.yaml")
	if err := sample().Save(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", st.Mode().Perm())
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentContext != "staging-eu" || len(got.Contexts) != 2 || got.Contexts[1].API != "https://forge.example.com" {
		t.Errorf("round trip mismatch: %+v", got)
	}
}

func TestLoadMissingAndEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c, err := Load(filepath.Join(dir, "nope.yaml"))
	if err != nil || len(c.Contexts) != 0 {
		t.Fatalf("missing file: %v %+v", err, c)
	}
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(empty); err != nil {
		t.Fatalf("empty file: %v", err)
	}
}

func TestLoadRejectsUnknownFieldsAndBadCurrent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"unknown": "current-context: a\ncontexts:\n  - name: a\n    api: x\n    tokn: secret\n",
		"current": "current-context: missing\ncontexts:\n  - name: a\n    api: x\n",
		"dup":     "contexts:\n  - name: a\n    api: x\n  - name: a\n    api: y\n",
	} {
		p := filepath.Join(dir, name+".yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDefaultPath(t *testing.T) {
	t.Parallel()
	p, _ := DefaultPath(mapEnv(map[string]string{"XDG_CONFIG_HOME": "/x", "HOME": "/h"}))
	if p != filepath.Join("/x", "forge", "config.yaml") {
		t.Errorf("xdg: %s", p)
	}
	p, _ = DefaultPath(mapEnv(map[string]string{"HOME": "/h"}))
	if p != filepath.Join("/h", ".config", "forge", "config.yaml") {
		t.Errorf("home: %s", p)
	}
	p, _ = DefaultPath(mapEnv(map[string]string{"FORGE_CONFIG": "/c.yaml", "HOME": "/h"}))
	if p != "/c.yaml" {
		t.Errorf("override: %s", p)
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Parallel()
	c := sample()
	d := Defaults{Output: "table"}

	r, err := Resolve(c, mapEnv(nil), Overrides{}, d)
	if err != nil {
		t.Fatal(err)
	}
	if r.ContextName != "staging-eu" || r.APIURL != "https://forge.staging.example.com" || r.Namespace != "billing" || r.Output != "table" {
		t.Errorf("config layer: %+v", r)
	}

	env := mapEnv(map[string]string{"FORGE_CONTEXT": "prod", "FORGE_OUTPUT": "json", "FORGE_TOKEN": "t0k"})
	r, _ = Resolve(c, env, Overrides{}, Defaults{Namespace: "payments", Output: "table"})
	if r.ContextName != "prod" || r.APIURL != "https://forge.example.com" || r.Output != "json" || r.Token != "t0k" {
		t.Errorf("env layer: %+v", r)
	}
	if r.Namespace != "payments" || r.Sources["namespace"] != "forge.yaml" {
		t.Errorf("manifest should beat context namespace: %+v", r)
	}

	r, _ = Resolve(c, env, Overrides{Context: "staging-eu", APIURL: "http://localhost:8080", Output: "yaml"}, d)
	if r.ContextName != "staging-eu" || r.APIURL != "http://localhost:8080" || r.Output != "yaml" {
		t.Errorf("flag layer: %+v", r)
	}
	if r.Sources["api"] != "flag --api-url" {
		t.Errorf("source: %v", r.Sources)
	}
}

func TestResolveUnknownContext(t *testing.T) {
	t.Parallel()
	_, err := Resolve(sample(), mapEnv(nil), Overrides{Context: "nope"}, Defaults{})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("want unknown context error, got %v", err)
	}
}

func TestSetContext(t *testing.T) {
	t.Parallel()
	c := sample()
	c.SetContext(Context{Name: "prod", API: "https://new"})
	c.SetContext(Context{Name: "dev", API: "http://localhost:8080"})
	if len(c.Contexts) != 3 {
		t.Fatalf("contexts: %+v", c.Contexts)
	}
	if got, _ := c.Context("prod"); got.API != "https://new" {
		t.Errorf("replace failed: %+v", got)
	}
}
