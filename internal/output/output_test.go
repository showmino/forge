package output

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	forgev1 "github.com/showmino/forge/gen/forge/v1"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run 'go test ./internal/output -update' to create it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func fixture() View {
	ts := timestamppb.New(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	resp := &forgev1.ListServicesResponse{Services: []*forgev1.Service{
		{
			Name: "services/payments", DisplayName: "Payments", OwnerTeam: "billing",
			Owners: []string{"ana@example.com"}, Lifecycle: forgev1.Lifecycle_LIFECYCLE_PRODUCTION,
			Labels: map[string]string{"tier": "1", "pci": "true"}, CreateTime: ts, Etag: "W/1",
		},
		{
			Name: "services/ledger", OwnerTeam: "billing", Lifecycle: forgev1.Lifecycle_LIFECYCLE_EXPERIMENTAL,
			Dependencies: []string{"services/payments"}, CreateTime: ts, Etag: "W/2",
		},
	}}
	return View{
		Headers: []string{"name", "team", "lifecycle", "owners"},
		Rows: [][]string{
			{"payments", "billing", "production", "ana@example.com"},
			{"ledger", "billing", "experimental", ""},
		},
		Names: []string{"services/payments", "services/ledger"},
		Data:  resp,
	}
}

func TestGolden(t *testing.T) {
	for _, f := range Formats {
		t.Run(string(f), func(t *testing.T) {
			var buf bytes.Buffer
			if err := Print(&buf, f, fixture()); err != nil {
				t.Fatal(err)
			}
			golden(t, "services_"+string(f), buf.Bytes())
		})
	}
}

func TestPlainStructJSON(t *testing.T) {
	var buf bytes.Buffer
	data := struct {
		Version string `json:"version"`
	}{"1.2.3"}
	if err := Print(&buf, YAML, View{Data: data}); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "version: 1.2.3\n" {
		t.Errorf("got %q", buf.String())
	}
}

func TestParse(t *testing.T) {
	for _, s := range []string{"", "table", "json", "yaml", "name"} {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q): %v", s, err)
		}
	}
	if _, err := Parse("xml"); err == nil {
		t.Error("Parse(xml) should fail")
	}
}

func TestTableSanitizesCells(t *testing.T) {
	var buf bytes.Buffer
	_ = Print(&buf, Table, View{Headers: []string{"a", "b"}, Rows: [][]string{{"x\ty", "line1\nline2"}}})
	want := "A     B\nx y   line1 line2\n"
	if buf.String() != want {
		t.Errorf("got %q want %q", buf.String(), want)
	}
}
