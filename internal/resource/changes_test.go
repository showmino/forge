package resource

import (
	"encoding/json"
	"fmt"
	"testing"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

// cluster is the object a kubernetes_manifest change holds for a database.
func cluster(name string, gi, instances int) map[string]any {
	return map[string]any{"manifest": map[string]any{
		"apiVersion": "postgresql.cnpg.io/v1", "kind": "Cluster",
		"metadata": map[string]any{"name": name, "namespace": "payments-staging"},
		"spec":     map[string]any{"instances": instances, "storage": map[string]any{"size": fmt.Sprintf("%dGi", gi), "storageClass": "longhorn-retain"}},
	}}
}

func change(address string, actions []string, before, after any) map[string]any {
	return map[string]any{"address": address, "type": "kubernetes_manifest",
		"change": map[string]any{"actions": actions, "before": before, "after": after}}
}

func planJSON(t *testing.T, changes ...map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"format_version": "1.2", "resource_changes": changes})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestChanges(t *testing.T) {
	t.Parallel()
	got, err := Changes(planJSON(t,
		change("kubernetes_manifest.database[\"main\"]", []string{"update"}, cluster("main", 5, 1), cluster("main", 10, 2)),
		change("kubernetes_manifest.database[\"old\"]", []string{"delete"}, cluster("old", 5, 1), nil),
		change("kubernetes_manifest.database[\"new\"]", []string{"create"}, nil, cluster("new", 1, 1)),
		change("kubernetes_manifest.database[\"swap\"]", []string{"delete", "create"}, cluster("swap", 5, 1), cluster("swap", 5, 1)),
		change("kubernetes_manifest.database[\"same\"]", []string{"no-op"}, cluster("same", 5, 1), cluster("same", 5, 1)),
	))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"update main 5/1 -> 10/2", "create new -> 1/1", "delete old 5/1 ->", "replace swap 5/1 -> 5/1"}
	if len(got) != len(want) {
		t.Fatalf("%d changes, want %d: %v", len(got), len(want), got)
	}
	for i, c := range got {
		line := c.GetAction() + " " + c.GetResource()
		if b := c.GetBefore(); b != nil {
			line += fmt.Sprintf(" %d/%d", b.GetStorageGi(), b.GetInstances())
		}
		line += " ->"
		if a := c.GetAfter(); a != nil {
			line += fmt.Sprintf(" %d/%d", a.GetStorageGi(), a.GetInstances())
		}
		if line != want[i] || c.GetType() != "postgres" {
			t.Errorf("change %d: %q (type %q), want %q", i, line, c.GetType(), want[i])
		}
	}
	if Count(got) != "create=1 update=1 delete=1 replace=1" || !Destructive(got) {
		t.Errorf("Count %q, Destructive %v", Count(got), Destructive(got))
	}
}

func TestChangesOddPlans(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{"empty object": `{}`, "no changes": `{"resource_changes":[]}`} {
		got, err := Changes([]byte(body))
		if err != nil || len(got) != 0 {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
	if Count(nil) != "create=0 update=0 delete=0" || Destructive(nil) {
		t.Errorf("no changes: %q", Count(nil))
	}
	for name, body := range map[string]string{"empty": ``, "not JSON": `tofu`, "a list": `[]`, "a wrong field type": `{"resource_changes":"x"}`} {
		if _, err := Changes([]byte(body)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}

	// Something that is not a database keeps its address and its plan type.
	other := change("null_resource.x", []string{"create"}, nil, map[string]any{"id": "1"})
	other["type"] = "null_resource"
	got, err := Changes(planJSON(t, other))
	if err != nil || len(got) != 1 || got[0].GetResource() != "null_resource.x" || got[0].GetType() != "null_resource" || got[0].GetAfter() != nil {
		t.Errorf("foreign change: %v %v", got, err)
	}
	if only := []*forgev1.ResourceChange{{Action: "create"}}; Destructive(only) {
		t.Error("a create is not destructive")
	}
}
