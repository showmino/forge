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
		{"another namespace", plan(t, []string{"create"}, nil, cluster("ledger-staging", 5, 1, retain)), stg, `the plan changes a database in namespace "ledger-staging", not payments-staging`, ""},
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

// without returns a copy of a cluster side with one nested field removed.
func without(side map[string]any, path ...string) map[string]any {
	b, _ := json.Marshal(side)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	m := out
	for _, k := range path[:len(path)-1] {
		m = m[k].(map[string]any)
	}
	delete(m, path[len(path)-1])
	return out
}

// with returns a copy of a cluster side with one nested field set to v.
func with(side map[string]any, v any, path ...string) map[string]any {
	b, _ := json.Marshal(side)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	m := out
	for _, k := range path[:len(path)-1] {
		m = m[k].(map[string]any)
	}
	m[path[len(path)-1]] = v
	return out
}

// raw builds a plan from one hand-written resource change.
func raw(t *testing.T, rc map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"resource_changes": []any{rc}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A rule that reads a missing field does not fire in Rego. These are the
// shapes that once slipped through: each must be refused.
func TestRulesFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const retain = "longhorn-retain"
	ok := cluster("payments-staging", 5, 1, retain)
	stg := Forge{Service: "payments", Environment: "staging"}
	destroy := stg
	destroy.AllowDestroy = true
	change := func(actions any, before, after any) map[string]any {
		return map[string]any{"address": "kubernetes_manifest.database", "type": "kubernetes_manifest",
			"change": map[string]any{"actions": actions, "before": before, "after": after}}
	}
	noAddress := change([]string{"create"}, nil, map[string]any{"id": "1"})
	delete(noAddress, "address")
	noActions := change(nil, nil, ok)
	delete(noActions["change"].(map[string]any), "actions")
	for _, tc := range []struct {
		name string
		plan []byte
		f    Forge
		want string
	}{
		{"an update whose after has no manifest", raw(t, change([]string{"update"}, ok, map[string]any{})), stg, "which is not a CloudNativePG cluster"},
		{"a replace whose after has no manifest", raw(t, change([]string{"delete", "create"}, ok, map[string]any{})), destroy, "which is not a CloudNativePG cluster"},
		{"a create with no namespace", raw(t, change([]string{"create"}, nil, without(ok, "manifest", "metadata", "namespace"))), stg, `in namespace "", not payments-staging`},
		{"a delete with no name", raw(t, change([]string{"delete"}, without(ok, "manifest", "metadata", "name"), nil)), stg, "the plan deletes database kubernetes_manifest.database; plan again with --allow-destroy"},
		{"a replace with no name", raw(t, change([]string{"delete", "create"}, ok, without(ok, "manifest", "metadata", "name"))), stg, "the plan deletes database main; plan again with --allow-destroy"},
		{"a replace that moves a database out of another namespace", raw(t, change([]string{"delete", "create"}, cluster("ledger-staging", 5, 1, retain), ok)), destroy, `in namespace "ledger-staging", not payments-staging`},
		{"an unknown environment", plan(t, []string{"create"}, nil, cluster("payments-dev", 5000, 1, retain)), Forge{Service: "payments", Environment: "dev"}, `the rules have no limits for environment "dev"`},
		{"no environment", plan(t, []string{"create"}, nil, ok), Forge{Service: "payments"}, `the rules have no limits for environment ""`},
		{"something else with no address", raw(t, noAddress), stg, "the plan changes an unnamed resource, which is not a CloudNativePG cluster"},
		{"an update from a size the rules cannot read", raw(t, change([]string{"update"}, map[string]any{"manifest": map[string]any{
			"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster", "metadata": map[string]any{"name": "main", "namespace": "payments-staging"},
			"spec": map[string]any{"instances": 1, "storage": map[string]any{"size": "1Ti", "storageClass": retain}}}}, ok)), stg, "has no readable storage size or instance count"},
		{"actions as a string", raw(t, change("create", nil, ok)), stg, "has no readable actions"},
		{"no actions", raw(t, noActions), stg, "has no readable actions"},
		{"empty actions", raw(t, change([]string{}, nil, ok)), stg, "has no readable actions"},
		{"metadata that is not an object", raw(t, change([]string{"create"}, nil, with(ok, "main", "manifest", "metadata"))), stg, `in namespace "", not payments-staging`},
		{"a namespace that is not a string", raw(t, change([]string{"create"}, nil, with(ok, 7, "manifest", "metadata", "namespace"))), stg, "in namespace 7, not payments-staging"},
		{"a manifest that is not an object", raw(t, change([]string{"create"}, nil, map[string]any{"manifest": "cluster"})), stg, "which is not a CloudNativePG cluster"},
		{"a spec that is not an object", raw(t, change([]string{"create"}, nil, with(ok, "big", "manifest", "spec"))), stg, "has no readable storage size or instance count"},
		{"a storage that is not an object", raw(t, change([]string{"create"}, nil, with(ok, "5Gi", "manifest", "spec", "storage"))), stg, `storage class "" is not longhorn-retain`},
		{"a change that is not an object", raw(t, map[string]any{"address": "x.y", "type": "kubernetes_manifest", "change": "create"}), stg, "the plan's change to x.y has no readable actions"},
		{"a before that is not an object", raw(t, change([]string{"delete"}, "main", nil)), stg, "which is not a CloudNativePG cluster"},
		{"zero instances", plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 0, retain)), stg, "0 instances is below the minimum of 1"},
		{"a database of another manifest type", raw(t, map[string]any{"address": "kubectl_manifest.x", "type": "kubectl_manifest",
			"change": map[string]any{"actions": []string{"create"}, "before": nil, "after": ok}}), stg, "kubectl_manifest.x, which is not a CloudNativePG cluster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := e.Evaluate(ctx, tc.plan, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if !r.Denied() || !strings.Contains(strings.Join(r.Deny, "|"), tc.want) {
				t.Errorf("deny %q, want it to contain %q", r.Deny, tc.want)
			}
		})
	}

	// A pure delete that was asked for goes through even when the old
	// object cannot be read: it only has to go.
	odd := without(ok, "manifest", "spec")
	if r, err := e.Evaluate(ctx, raw(t, change([]string{"delete"}, odd, nil)), destroy); err != nil || r.Denied() {
		t.Errorf("an asked-for delete of an unreadable database: %v %v", r, err)
	}
	// Changes that are not a list of objects are refused too.
	for body, want := range map[string]string{
		`{"resource_changes":"x"}`:      "the plan's resource_changes is not a list",
		`{"resource_changes":{"a":1}}`:  "the plan's resource_changes is not a list",
		`{"resource_changes":[7]}`:      "the plan changes an unnamed resource, which is not a CloudNativePG cluster",
		`{"resource_changes":[null]}`:   "the plan changes an unnamed resource, which is not a CloudNativePG cluster",
		`{"resource_changes":["tofu"]}`: "has no readable actions",
	} {
		r, err := e.Evaluate(ctx, []byte(body), stg)
		if err != nil || !strings.Contains(strings.Join(r.Deny, "|"), want) {
			t.Errorf("%s: deny %q (%v), want it to contain %q", body, r.Deny, err, want)
		}
	}
	// A plan that is not a JSON object could not be checked: an error.
	for _, body := range []string{"null", "[]", `"tofu"`, "42"} {
		if _, err := e.Evaluate(ctx, []byte(body), stg); err == nil {
			t.Errorf("%s: want an error", body)
		}
	}
}
