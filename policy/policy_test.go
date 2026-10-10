package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
)

// cluster is the object a kubernetes_manifest change holds for a database.
func cluster(ns string, gi, instances int, class string) map[string]any {
	return map[string]any{"manifest": map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": "main", "namespace": ns},
		"spec":     map[string]any{"instances": instances, "storage": map[string]any{"size": fmt.Sprintf("%dGi", gi), "storageClass": class}},
	}}
}

func plan(t *testing.T, actions []string, before, after any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"format_version": "1.2", "resource_changes": []any{map[string]any{
		"address": "kubernetes_manifest.database", "type": "kubernetes_manifest", "name": "database",
		"change": map[string]any{"actions": actions, "before": before, "after": after},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const retain = "longhorn-retain"
	stg := Forge{Service: "payments", Environment: "staging"}
	prod := Forge{Service: "payments", Environment: "production"}
	destroy := stg
	destroy.AllowDestroy = true
	for _, tc := range []struct {
		name       string
		plan       []byte
		f          Forge
		deny, warn string
	}{
		{"a create passes", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 1, retain)), stg, "", ""},
		{"20Gi passes in staging", plan(t, []string{"create"}, nil, cluster("payments-staging", 20, 1, retain)), stg, "", ""},
		{"21Gi is too much for staging", plan(t, []string{"create"}, nil, cluster("payments-staging", 21, 1, retain)), stg, "storage 21Gi is above the 20Gi limit for staging", ""},
		{"50Gi passes in production", plan(t, []string{"create"}, nil, cluster("payments-production", 50, 2, retain)), prod, "", ""},
		{"200Gi is too much for production", plan(t, []string{"create"}, nil, cluster("payments-production", 200, 2, retain)), prod, "storage 200Gi is above the 50Gi limit for production", ""},
		{"production needs two instances", plan(t, []string{"create"}, nil, cluster("payments-production", 5, 1, retain)), prod, "production needs at least 2 instances, not 1", ""},
		{"four instances are too many", plan(t, []string{"create"}, nil, cluster("payments-production", 5, 4, retain)), prod, "4 instances is above the limit of 3", ""},
		{"another storage class", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 1, "local-path")), stg, `storage class "local-path" is not longhorn-retain`, ""},
		{"no storage class", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 1, "")), stg, `storage class "" is not longhorn-retain`, ""},
		{"a shrink", plan(t, []string{"update"}, cluster("payments-staging", 10, 1, retain), cluster("payments-staging", 5, 1, retain)), stg, "storage cannot shrink from 10Gi to 5Gi", ""},
		{"growth passes", plan(t, []string{"update"}, cluster("payments-staging", 5, 1, retain), cluster("payments-staging", 10, 1, retain)), stg, "", ""},
		{"a delete", plan(t, []string{"delete"}, cluster("payments-staging", 5, 1, retain), nil), stg, "the plan deletes database main; plan again with --allow-destroy to remove it", ""},
		{"a delete that was asked for", plan(t, []string{"delete"}, cluster("payments-staging", 5, 1, retain), nil), destroy, "", ""},
		{"a replace is a delete", plan(t, []string{"delete", "create"}, cluster("payments-staging", 5, 1, retain), cluster("payments-staging", 5, 1, retain)), stg, "the plan deletes database main; plan again with --allow-destroy to remove it", ""},
		{"another namespace", plan(t, []string{"create"}, nil, cluster("ledger-staging", 5, 1, retain)), stg, "the plan changes a database in namespace ledger-staging, not payments-staging", ""},
		{"two instances in staging warn", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 2, retain)), stg, "", "staging has 2 instances; 1 is usually enough"},
		{"a no-op is not checked", plan(t, []string{"no-op"}, cluster("ledger-staging", 500, 9, "x"), cluster("ledger-staging", 500, 9, "x")), stg, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := e.Evaluate(ctx, tc.plan, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(r.Deny, "|"); got != tc.deny {
				t.Errorf("deny %q, want %q", got, tc.deny)
			}
			if got := strings.Join(r.Warn, "|"); got != tc.warn {
				t.Errorf("warn %q, want %q", got, tc.warn)
			}
			if r.Denied() != (tc.deny != "") {
				t.Errorf("Denied() = %v", r.Denied())
			}
		})
	}
}

func TestRulesGuardThePlatform(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stg := Forge{Service: "payments", Environment: "staging"}

	// Anything that is not a database is refused, whatever it is.
	secret, _ := json.Marshal(map[string]any{"resource_changes": []any{map[string]any{
		"address": "kubernetes_manifest.x", "type": "kubernetes_manifest",
		"change": map[string]any{"actions": []string{"create"}, "before": nil, "after": map[string]any{"manifest": map[string]any{
			"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": "s", "namespace": "payments-staging"}}}},
	}}})
	r, err := e.Evaluate(ctx, secret, stg)
	if err != nil || len(r.Deny) != 1 || r.Deny[0] != "the plan changes kubernetes_manifest.x, which is not a CloudNativePG cluster" {
		t.Errorf("a Secret: %v %v", r, err)
	}
	other, _ := json.Marshal(map[string]any{"resource_changes": []any{map[string]any{
		"address": "null_resource.x", "type": "null_resource",
		"change": map[string]any{"actions": []string{"create"}, "before": nil, "after": map[string]any{"id": "1"}},
	}}})
	if r, err := e.Evaluate(ctx, other, stg); err != nil || !r.Denied() {
		t.Errorf("another resource type: %v %v", r, err)
	}

	// A database the rules cannot read is refused, never waved through.
	odd, _ := json.Marshal(map[string]any{"resource_changes": []any{map[string]any{
		"address": "kubernetes_manifest.database", "type": "kubernetes_manifest",
		"change": map[string]any{"actions": []string{"create"}, "before": nil, "after": map[string]any{"manifest": map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
			"metadata": map[string]any{"name": "main", "namespace": "payments-staging"}, "spec": map[string]any{}}}},
	}}})
	r, err = e.Evaluate(ctx, odd, stg)
	if err != nil || !strings.Contains(strings.Join(r.Deny, "|"), "has no readable storage size or instance count") {
		t.Errorf("an unreadable database: %v %v", r, err)
	}

	// A plan with nothing in it passes: there is nothing to refuse.
	for _, body := range []string{`{"resource_changes":[]}`, `{}`} {
		if r, err := e.Evaluate(ctx, []byte(body), stg); err != nil || r.Denied() || len(r.Warn) != 0 {
			t.Errorf("%s: %v %v", body, r, err)
		}
	}
	for _, body := range []string{``, `tofu`} {
		if _, err := e.Evaluate(ctx, []byte(body), stg); err == nil {
			t.Errorf("%q: want an error", body)
		}
	}
}

func TestEngine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Fingerprint()) != 12 || a.Fingerprint() != b.Fingerprint() {
		t.Errorf("fingerprints %q %q", a.Fingerprint(), b.Fingerprint())
	}
	rule := "package forge.resources\nimport rego.v1\ndeny contains \"no\" if true\nwarn contains \"hm\" if true\n"
	c, err := NewFromFS(ctx, fstest.MapFS{"x.rego": {Data: []byte(rule)}})
	if err != nil {
		t.Fatal(err)
	}
	if c.Fingerprint() == a.Fingerprint() {
		t.Error("other rules must have another fingerprint")
	}
	if r, err := c.Evaluate(ctx, []byte(`{}`), Forge{}); err != nil || len(r.Deny) != 1 || r.Deny[0] != "no" || r.Warn[0] != "hm" {
		t.Errorf("custom rules: %v %v", r, err)
	}
	if _, err := NewFromFS(ctx, fstest.MapFS{"bad.rego": {Data: []byte("package forge.resources\ndeny contains msg if {")}}); err == nil {
		t.Error("rules that do not compile must be an error")
	}
	if _, err := NewFromFS(ctx, fstest.MapFS{}); err == nil {
		t.Error("no rules must be an error")
	}
	// Rules whose messages are not strings are an error, not a pass.
	nums := "package forge.resources\nimport rego.v1\ndeny contains 1 if true\nwarn contains \"x\" if false\n"
	d, err := NewFromFS(ctx, fstest.MapFS{"n.rego": {Data: []byte(nums)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Evaluate(ctx, []byte(`{}`), Forge{}); err == nil {
		t.Error("a non-string message must be an error")
	}
}
