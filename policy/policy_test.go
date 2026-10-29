package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
			r, err := e.Evaluate(ctx, tc.plan, declaring(tc.f, tc.plan))
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
			r, err := e.Evaluate(ctx, tc.plan, declaring(tc.f, tc.plan))
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

// declaring returns f declaring what planJSON's first change ends with, so a
// case that is not about the declaration is not refused for it.
func declaring(f Forge, planJSON []byte) Forge {
	var doc struct {
		ResourceChanges []struct {
			Change struct {
				After struct {
					Manifest struct {
						Metadata struct {
							Name string `json:"name"`
						} `json:"metadata"`
						Spec struct {
							Instances json.Number `json:"instances"`
							Storage   struct {
								Size string `json:"size"`
							} `json:"storage"`
						} `json:"spec"`
					} `json:"manifest"`
				} `json:"after"`
			} `json:"change"`
		} `json:"resource_changes"`
	}
	if json.Unmarshal(planJSON, &doc) != nil || len(doc.ResourceChanges) == 0 {
		return f
	}
	m := doc.ResourceChanges[0].Change.After.Manifest
	gi, err1 := strconv.Atoi(strings.TrimSuffix(m.Spec.Storage.Size, "Gi"))
	n, err2 := strconv.Atoi(m.Spec.Instances.String())
	if err1 != nil || err2 != nil {
		return f
	}
	f.Resources = []Resource{{Name: m.Metadata.Name, Type: "postgres", StorageGi: int32(gi), Instances: int32(n)}} //nolint:gosec // test sizes
	return f
}

// A plan may only make what forge.yaml declares.
func TestRulesDeclared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const retain = "longhorn-retain"
	create := plan(t, []string{"create"}, nil, cluster("payments-staging", 5, 1, retain))
	main := func(gi, instances int32) []Resource {
		return []Resource{{Name: "main", Type: "postgres", StorageGi: gi, Instances: instances}}
	}
	for name, tc := range map[string]struct {
		resources []Resource
		want      string
	}{
		"as declared":           {main(5, 1), ""},
		"nothing declared":      {nil, `the plan makes database "main" with 5Gi and 1 instances, which is not what forge.yaml declares`},
		"another size":          {main(6, 1), "which is not what forge.yaml declares"},
		"another count":         {main(5, 2), "which is not what forge.yaml declares"},
		"another name":          {[]Resource{{Name: "other", Type: "postgres", StorageGi: 5, Instances: 1}}, "which is not what forge.yaml declares"},
		"another type":          {[]Resource{{Name: "main", Type: "redis", StorageGi: 5, Instances: 1}}, "which is not what forge.yaml declares"},
		"declared among others": {append(main(9, 3), main(5, 1)...), ""},
	} {
		r, err := e.Evaluate(ctx, create, Forge{Service: "payments", Environment: "staging", Resources: tc.resources})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(r.Deny, "|"); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: deny %q, want %q", name, got, tc.want)
		}
	}
	// Deleting needs no declaration: removing it from forge.yaml is the point.
	gone := plan(t, []string{"delete"}, cluster("payments-staging", 5, 1, retain), nil)
	if r, err := e.Evaluate(ctx, gone, Forge{Service: "payments", Environment: "staging", AllowDestroy: true}); err != nil || r.Denied() {
		t.Errorf("an asked-for delete with nothing declared: %v %v", r, err)
	}
}

// Actions the rules do not know, and changes that leave nothing behind
// without saying they delete, are refused.
func TestRulesKnownActions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ok := cluster("payments-staging", 5, 1, "longhorn-retain")
	destroy := Forge{Service: "payments", Environment: "staging", AllowDestroy: true}
	for name, tc := range map[string]struct {
		actions       []string
		before, after any
		want          string
	}{
		"forget":                      {[]string{"forget"}, ok, nil, `has an action the rules do not know: "forget"`},
		"forget beside a delete":      {[]string{"delete", "forget"}, ok, nil, `has an action the rules do not know: "forget"`},
		"an update to nothing":        {[]string{"update"}, ok, nil, "leaves nothing behind but is not a delete"},
		"a create of nothing":         {[]string{"create"}, nil, nil, "leaves nothing behind but is not a delete"},
		"an unknown verb with a body": {[]string{"destroy"}, ok, ok, `has an action the rules do not know: "destroy"`},
	} {
		r, err := e.Evaluate(ctx, plan(t, tc.actions, tc.before, tc.after), declaring(destroy, plan(t, tc.actions, tc.before, tc.after)))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(r.Deny, "|"), tc.want) {
			t.Errorf("%s: deny %q, want it to contain %q", name, r.Deny, tc.want)
		}
	}
}

// Plans captured from real OpenTofu 1.12.7 runs of the module (provider
// hashicorp/kubernetes 3.3.0) against CloudNativePG 1.30: the rules must read
// the real shape, not only the hand-built one.
func TestRulesOnCapturedPlans(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	main := func(instances int32) []Resource {
		return []Resource{{Name: "main", Type: "postgres", StorageGi: 1, Instances: instances}}
	}
	for _, tc := range []struct {
		file       string
		f          Forge
		deny, warn string
	}{
		{"create.json", Forge{Service: "payments", Environment: "staging", Resources: main(1)}, "", ""},
		{"create.json", Forge{Service: "payments", Environment: "staging", Resources: main(2)}, "which is not what forge.yaml declares", ""},
		{"create.json", Forge{Service: "ledger", Environment: "staging", Resources: main(1)}, `in namespace "payments-staging", not ledger-staging`, ""},
		{"update.json", Forge{Service: "payments", Environment: "staging", Resources: main(2)}, "", "staging has 2 instances; 1 is usually enough"},
		{"delete.json", Forge{Service: "payments", Environment: "staging"}, "the plan deletes database main; plan again with --allow-destroy to remove it", ""},
		{"delete.json", Forge{Service: "payments", Environment: "staging", AllowDestroy: true}, "", ""},
	} {
		planJSON, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		r, err := e.Evaluate(ctx, planJSON, tc.f)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if got := strings.Join(r.Deny, "|"); (tc.deny == "") != (got == "") || !strings.Contains(got, tc.deny) {
			t.Errorf("%s as %s: deny %q, want %q", tc.file, tc.f.Service, got, tc.deny)
		}
		if got := strings.Join(r.Warn, "|"); got != tc.warn {
			t.Errorf("%s as %s: warn %q, want %q", tc.file, tc.f.Service, got, tc.warn)
		}
	}
}

func TestPromotionRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ok := Promotion{Service: "payments", From: "staging", To: "production",
		Source: Source{Sync: "Synced", Health: "Healthy", AtRevision: true, Commit: "1a2b3c4", DeployCommit: "9f8e7d6"}}
	mod := func(f func(*Promotion)) Promotion { p := ok; f(&p); return p }
	for _, tc := range []struct {
		name string
		p    Promotion
		deny string
	}{
		{"a synced and healthy staging passes", ok, ""},
		{"out of sync", mod(func(p *Promotion) { p.Source.Sync = "OutOfSync" }), "staging is OutOfSync; it must be Synced to be promoted"},
		{"no sync status", mod(func(p *Promotion) { p.Source.Sync = "" }), "staging is unknown; it must be Synced to be promoted"},
		{"degraded", mod(func(p *Promotion) { p.Source.Health = "Degraded" }), "staging is Degraded; it must be Healthy to be promoted"},
		{"progressing", mod(func(p *Promotion) { p.Source.Health = "Progressing" }), "staging is Progressing; it must be Healthy to be promoted"},
		{"missing", mod(func(p *Promotion) { p.Source.Health = "Missing" }), "staging is Missing; it must be Healthy to be promoted"},
		{"not at its latest deploy", mod(func(p *Promotion) { p.Source.AtRevision = false }), "staging does not run its latest deploy (9f8e7d6) yet; wait for Argo CD to sync it"},
		{"both", mod(func(p *Promotion) { p.Source.Sync = "OutOfSync"; p.Source.Health = "Degraded" }),
			"staging is Degraded; it must be Healthy to be promoted|staging is OutOfSync; it must be Synced to be promoted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := e.EvaluatePromotion(ctx, tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(r.Deny, "|"); got != tc.deny {
				t.Errorf("deny %q, want %q", got, tc.deny)
			}
			if r.Denied() != (tc.deny != "") {
				t.Errorf("denied %v", r.Denied())
			}
		})
	}
}

// Rules with no forge.deploys package cannot judge a promotion: that is an
// error, never a pass.
func TestPromotionWithoutRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rule := "package forge.resources\nimport rego.v1\ndeny contains \"no\" if false\nwarn contains \"hm\" if false\n"
	e, err := NewFromFS(ctx, fstest.MapFS{"x.rego": {Data: []byte(rule)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.EvaluatePromotion(ctx, Promotion{}); err == nil {
		t.Error("no promotion rules must be an error")
	}
}

func TestImageRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ok := Image{Service: "web", Image: "docker.io/library/nginx:1.27", Host: "docker.io", Repo: "library/nginx", Tag: "1.27"}
	mod := func(f func(*Image)) Image { i := ok; f(&i); return i }
	for _, tc := range []struct {
		name string
		i    Image
		deny string
	}{
		{"a tagged image from any registry passes when none are named", ok, ""},
		{"an allowed registry passes", mod(func(i *Image) { i.AllowedRegistries = []string{"ghcr.io", "docker.io"} }), ""},
		{"a digest without a tag passes", mod(func(i *Image) { i.Tag = ""; i.Digest = "sha256:0123" }), ""},
		{"latest", mod(func(i *Image) { i.Tag = "latest" }),
			"docker.io/library/nginx:1.27: the tag latest moves; name a version, such as nginx:1.27"},
		{"no tag and no digest", mod(func(i *Image) { i.Tag = "" }),
			"docker.io/library/nginx:1.27 names no tag; name a version, such as nginx:1.27"},
		{"a registry not allowed", mod(func(i *Image) { i.AllowedRegistries = []string{"ghcr.io"} }),
			"docker.io/library/nginx:1.27: images may only come from ghcr.io"},
		{"no host", mod(func(i *Image) { i.Host = ""; i.AllowedRegistries = []string{"ghcr.io"} }),
			"docker.io/library/nginx:1.27: images may only come from ghcr.io"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := e.EvaluateImage(ctx, tc.i)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(r.Deny, "|"); got != tc.deny {
				t.Errorf("deny %q, want %q", got, tc.deny)
			}
		})
	}
	// A rule set without image rules cannot judge an image.
	bare, err := NewFromFS(ctx, fstest.MapFS{"x.rego": {Data: []byte("package forge.resources\nimport rego.v1\ndeny contains \"no\" if false\n")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.EvaluateImage(ctx, ok); err == nil {
		t.Error("an image was judged without image rules")
	}
}
