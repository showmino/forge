package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/showmino/forge/internal/exitcode"
)

func TestCreateChartFlags(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"create", "p", "--chart", "podinfo", "--image", "nginx:1.27", "--team", "web"}, "use one"},
		{[]string{"create", "p", "--chart", "podinfo", "--team", "web"}, "--chart needs --chart-repo, --chart-version and --expose"},
		{[]string{"create", "p", "--chart", "podinfo", "--chart-repo", "oci://ghcr.io/x", "--chart-version", "1.0.0", "--expose", "podinfo", "--team", "web"}, "must be <service>:<port>"},
		{[]string{"create", "p", "--chart", "podinfo", "--port", "80", "--team", "web"}, "do not apply to a chart"},
		{[]string{"create", "p", "--template", "go-grpc", "--expose", "a:1", "--team", "web"}, "are for --chart"},
		{[]string{"create", "p", "--chart", "podinfo", "--chart-repo", "oci://ghcr.io/x", "--chart-version", "1.0.0", "--expose", "podinfo:9898", "--values", "/nope.yaml", "--team", "web"}, "--values"},
	} {
		r := h.fails(int(exitcode.Usage), tc.args...)
		if !strings.Contains(r.err, tc.want) {
			t.Errorf("forge %s: %s, want %q", strings.Join(tc.args, " "), r.err, tc.want)
		}
	}
}

func TestChartSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	values := filepath.Join(dir, "values.yaml")
	prod := filepath.Join(dir, "prod.yaml")
	if err := os.WriteFile(values, []byte("replicaCount: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prod, []byte("replicaCount: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := chartSource("podinfo", "oci://ghcr.io/stefanprodan/charts", "6.7.1", "podinfo:9898", values, "", prod)
	if err != nil {
		t.Fatal(err)
	}
	if src.GetExposeService() != "podinfo" || src.GetExposePort() != 9898 || src.GetValues() != "replicaCount: 2\n" ||
		src.GetProductionValues() != "replicaCount: 3\n" || src.GetStagingValues() != "" {
		t.Errorf("source: %v", src)
	}
}
