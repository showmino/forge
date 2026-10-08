package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  team: billing\n"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Service.Name != "payments" || m.Service.Team != "billing" {
		t.Errorf("got %+v", m.Service)
	}
	if _, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: payments\n  tem: billing\n")); err == nil || !strings.Contains(err.Error(), "tem") {
		t.Errorf("unknown field: %v", err)
	}
	if _, err := Parse([]byte("apiVersion: forge.dev/v1\nservice:\n  name: Bad_Name\n  team: billing\n")); err == nil {
		t.Error("invalid name accepted")
	}
}

func TestRoundTripAndFind(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	m := &Manifest{APIVersion: APIVersion, Service: Service{Name: "payments", Team: "billing", Lifecycle: "production"}}
	data, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, FileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := Find(sub)
	if err != nil || p != filepath.Join(root, FileName) {
		t.Fatalf("Find = %q, %v", p, err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Service.Name != "payments" || got.Service.Team != "billing" {
		t.Errorf("got %+v", got)
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	bad := []Manifest{
		{APIVersion: "v0", Service: Service{Name: "a", Team: "t"}},
		{APIVersion: APIVersion, Service: Service{Name: "Bad_Name", Team: "t"}},
		{APIVersion: APIVersion, Service: Service{Name: "ok"}},
		{APIVersion: APIVersion, Service: Service{Name: "ok", Team: "t", Lifecycle: "alive"}},
	}
	for i, m := range bad {
		if err := m.Validate(); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	for _, id := range []string{"a", "payments", "pr-142", "a1"} {
		if !ValidID(id) {
			t.Errorf("%q should be valid", id)
		}
	}
	for _, id := range []string{"", "1a", "-a", "a-", "A", "a_b"} {
		if ValidID(id) {
			t.Errorf("%q should be invalid", id)
		}
	}
}
