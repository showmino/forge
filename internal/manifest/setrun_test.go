package manifest

import (
	"strings"
	"testing"
)

func TestSetRunOptions(t *testing.T) {
	t.Parallel()
	in := `# Forge service manifest. See 'forge init --help'.
apiVersion: forge.dev/v1
service:
  name: payments
  team: billing
run:
  port: 3000 # the app's port
  health: /api/healthz
  replicas: 5
docs:
  - path: index.md
`
	out, err := SetRunOptions([]byte(in), &Run{
		Replicas: 2,
		Env:      map[string]string{"LOG_LEVEL": "info"},
		Jobs:     []Job{{Name: "cleanup", Schedule: "0 3 * * *", Command: []string{"node", "cleanup.js"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"# Forge service manifest.", "port: 3000 # the app's port", "health: /api/healthz", "replicas: 2", "LOG_LEVEL: info", "schedule: 0 3 * * *", "- path: index.md"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	m, err := Parse(out)
	if err != nil {
		t.Fatalf("%v in:\n%s", err, s)
	}
	if m.Run.Port != 3000 || m.Run.Replicas != 2 || m.Run.Jobs[0].Command[1] != "cleanup.js" || m.Run.Secrets != nil {
		t.Errorf("parsed back: %+v", m.Run)
	}

	// Options left empty are removed; the rest of run stays.
	out, err = SetRunOptions(out, &Run{})
	if err != nil {
		t.Fatal(err)
	}
	if s := string(out); strings.Contains(s, "replicas") || strings.Contains(s, "jobs") || strings.Contains(s, "env") || !strings.Contains(s, "port: 3000") {
		t.Errorf("cleared:\n%s", s)
	}

	// A manifest without run gets one.
	out, err = SetRunOptions([]byte("apiVersion: forge.dev/v1\nservice:\n  name: a\n  team: b\n"), &Run{Replicas: 3})
	if err != nil || !strings.Contains(string(out), "run:\n  replicas: 3\n") {
		t.Errorf("no run: %s %v", out, err)
	}
}

func TestRunOptionsOf(t *testing.T) {
	t.Parallel()
	r := &Run{Replicas: 2, Env: map[string]string{"A": "b"}, Autoscaling: &Autoscaling{Max: 4}}
	if got := r.OptionsSet(); strings.Join(got, ",") != "replicas,env,autoscaling" {
		t.Errorf("set: %v", got)
	}
	if !ValidOption("secrets") || ValidOption("port") {
		t.Error("ValidOption")
	}
}

func TestSetStagingPublic(t *testing.T) {
	t.Parallel()
	in := "# top\napiVersion: forge.dev/v1\nservice:\n  name: a\n  team: b\nrun:\n  port: 3000 # app\n  staging:\n    public: true\n"
	out, err := SetStagingPublic([]byte(in), false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(out)
	if err != nil || !m.StagingInternal() || m.RunPort() != 3000 || !strings.Contains(string(out), "port: 3000 # app") || strings.Count(string(out), "public:") != 1 {
		t.Errorf("%v:\n%s", err, out)
	}
	out, err = SetStagingPublic([]byte("apiVersion: forge.dev/v1\nservice:\n  name: a\n  team: b\n"), false)
	if m, perr := Parse(out); err != nil || perr != nil || !m.StagingInternal() {
		t.Errorf("without run: %v %v\n%s", err, perr, out)
	}
}

func TestSetRunDomain(t *testing.T) {
	t.Parallel()
	in := "# top\napiVersion: forge.dev/v1\nservice:\n  name: a\n  team: b\nrun:\n  port: 3000 # app\n"
	out, err := SetRunDomain([]byte(in), "none")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Parse(out)
	if err != nil || m.RunDomain() != "none" || m.RunPort() != 3000 || !strings.Contains(string(out), "port: 3000 # app") {
		t.Errorf("%v:\n%s", err, out)
	}
	out, err = SetRunDomain([]byte("apiVersion: forge.dev/v1\nservice:\n  name: a\n  team: b\n"), "none")
	if m, perr := Parse(out); err != nil || perr != nil || m.RunDomain() != "none" {
		t.Errorf("without run: %v %v\n%s", err, perr, out)
	}
}
