// Package connect writes what connects an existing repo to Forge (phase 10):
// its forge.yaml, Forge's build-and-deploy workflow, the workflow that tags
// a release's image, and, when the repo has none, a Dockerfile for the
// language it finds. It reads nothing itself: the caller gives it the repo's
// paths and a way to read a file.
package connect

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/showmino/forge/internal/manifest"
)

// ErrNoDockerfile means the repo has no Dockerfile and Forge cannot write
// one for it.
var ErrNoDockerfile = errors.New("no Dockerfile")

// ErrConnected means the repo has a forge.yaml or Forge's workflow already.
var ErrConnected = errors.New("already connected")

// Files Forge adds to a repo.
const (
	WorkflowPath = ".github/workflows/forge.yml"
	ReleasePath  = ".github/workflows/forge-release.yml"
	DockerPath   = "Dockerfile"
)

// Input is the repo and the service it becomes.
type Input struct {
	Service, Team string
	Owners        []string
	// Owner is the repo's owner (the organization) and RepoURL the repo.
	Owner, RepoURL string
	// Port and Health are run.port and run.health; 0 and "" for Forge's defaults.
	Port   int
	Health string
	// Registry is where CI pushes images (ghcr.io, myregistry.azurecr.io).
	Registry string
	// Runner builds the image; DeployRunner runs the deploy job, and must
	// reach the Forge API.
	Runner, DeployRunner string
	// Paths are every file in the repo's default branch, relative to its root.
	Paths []string
	// Read reads one of them.
	Read func(path string) ([]byte, error)
}

// File is one file of the change.
type File struct {
	Path    string
	Content []byte
}

// Result is the change that connects the repo.
type Result struct {
	Files []File
	// Language is the language of the proposed Dockerfile ("go", "node",
	// "python"), or "" when the repo has its own.
	Language string
	// RegistrySecrets is set when the workflow logs in to the registry with
	// the repo secrets FORGE_REGISTRY_USERNAME and FORGE_REGISTRY_PASSWORD.
	RegistrySecrets bool
	// UserNote says what is wrong with the user the repo's own Dockerfile
	// runs as, or "" when its last stage names a numeric non-root USER.
	UserNote string
}

//go:embed files/*
var templateFiles embed.FS

var tmpl = template.Must(template.New("").Delims("{%", "%}").ParseFS(templateFiles, "files/*"))

// Plan writes the change for in.
func Plan(in Input) (*Result, error) {
	has := func(p string) bool { return slices.Contains(in.Paths, p) }
	for _, p := range []string{manifest.FileName, WorkflowPath, ReleasePath} {
		if has(p) {
			return nil, fmt.Errorf("the repo has %s: %w", p, ErrConnected)
		}
	}
	res := &Result{}
	var run manifest.Run
	if in.Port != 0 {
		run.Port = in.Port
	}
	run.Health = in.Health
	if has(DockerPath) {
		if data, err := in.Read(DockerPath); err == nil {
			res.UserNote = userNote(data)
		}
	} else {
		d, err := dockerfile(in, has)
		if err != nil {
			return nil, err
		}
		res.Language, run.Writable = d.language, d.writable
		data, err := render(d.language+".Dockerfile", d)
		if err != nil {
			return nil, err
		}
		res.Files = append(res.Files, File{DockerPath, data})
	}
	m := manifest.Manifest{
		APIVersion: manifest.APIVersion,
		Service:    manifest.Service{Name: in.Service, Team: in.Team, Owners: in.Owners, Lifecycle: "experimental", Repo: in.RepoURL},
	}
	if run.Port != 0 || run.Health != "" || len(run.Writable) > 0 {
		m.Run = &run
	}
	data, err := m.Marshal()
	if err != nil {
		return nil, err
	}
	if _, err := manifest.Parse(data); err != nil {
		return nil, fmt.Errorf("forge.yaml: %w", err)
	}
	ghcr := in.Registry == "" || in.Registry == "ghcr.io"
	res.RegistrySecrets = !ghcr
	registry := in.Registry
	if registry == "" {
		registry = "ghcr.io"
	}
	v := workflowValues{
		Image:  registry + "/" + strings.ToLower(in.Owner) + "/" + in.Service,
		Host:   strings.SplitN(registry, "/", 2)[0],
		GHCR:   ghcr,
		Runner: or(in.Runner, "ubuntu-latest"), DeployRunner: or(in.DeployRunner, or(in.Runner, "ubuntu-latest")),
	}
	wf, err := render("forge.yml", v)
	if err != nil {
		return nil, err
	}
	rel, err := render("forge-release.yml", v)
	if err != nil {
		return nil, err
	}
	res.Files = append([]File{{manifest.FileName, data}}, res.Files...)
	res.Files = append(res.Files, File{WorkflowPath, wf}, File{ReleasePath, rel})
	return res, nil
}

type workflowValues struct {
	Image, Host          string
	GHCR                 bool
	Runner, DeployRunner string
}

// docker is what a proposed Dockerfile needs.
type docker struct {
	language string
	// Go: the version and the package to build.
	GoVersion, Package string
	// Node: how to install, build and start.
	Install, Build string
	Start          []string
	// Python: how to install, and the file to run.
	PipInstall, Entry string
	writable          []string
}

// The JSON form of a CMD, ["npm", "start"].
func (d docker) Cmd() string {
	q := make([]string, len(d.Start))
	for i, s := range d.Start {
		b, _ := json.Marshal(s)
		q[i] = string(b)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

var goDirective = regexp.MustCompile(`(?m)^go\s+(\d+)\.(\d+)`)

func dockerfile(in Input, has func(string) bool) (docker, error) {
	switch {
	case has("go.mod"):
		d := docker{language: "go", GoVersion: "1.24"}
		if mod, err := in.Read("go.mod"); err == nil {
			if m := goDirective.FindSubmatch(mod); m != nil {
				d.GoVersion = string(m[1]) + "." + string(m[2])
			}
		}
		switch cmds := commands(in.Paths); {
		case has("main.go"):
			d.Package = "."
		case slices.Contains(cmds, in.Service):
			d.Package = "./cmd/" + in.Service
		case len(cmds) == 1:
			d.Package = "./cmd/" + cmds[0]
		default:
			return d, fmt.Errorf("a Go module with no main.go at its root and no single cmd/<name>/main.go (or cmd/%s): add a Dockerfile: %w", in.Service, ErrNoDockerfile)
		}
		return d, nil
	case has("package.json"):
		data, err := in.Read("package.json")
		if err != nil {
			return docker{}, fmt.Errorf("read package.json: %w", err)
		}
		var pkg struct {
			Main            string            `json:"main"`
			Scripts         map[string]string `json:"scripts"`
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		if err := json.Unmarshal(data, &pkg); err != nil {
			return docker{}, fmt.Errorf("package.json is not valid JSON: add a Dockerfile: %w", ErrNoDockerfile)
		}
		d := docker{language: "node", Install: "npm install"}
		if has("package-lock.json") {
			d.Install = "npm ci"
		}
		if pkg.Scripts["build"] != "" {
			d.Build = "npm run build"
		}
		switch {
		case pkg.Scripts["start"] != "":
			d.Start = []string{"npm", "start"}
		case pkg.Main != "" && has(path.Clean(pkg.Main)):
			d.Start = []string{"node", path.Clean(pkg.Main)}
		case has("index.js"):
			d.Start = []string{"node", "index.js"}
		case has("server.js"):
			d.Start = []string{"node", "server.js"}
		default:
			if pkg.Main != "" {
				d.Start = []string{"node", path.Clean(pkg.Main)}
				break
			}
			return d, fmt.Errorf("package.json has no start script and no main: add one, or a Dockerfile: %w", ErrNoDockerfile)
		}
		if _, ok := pkg.Dependencies["next"]; ok {
			d.writable = []string{"/app/.next/cache"}
		}
		return d, nil
	case has("requirements.txt") || has("pyproject.toml"):
		d := docker{language: "python", PipInstall: "pip install --no-cache-dir ."}
		if has("requirements.txt") {
			d.PipInstall = "pip install --no-cache-dir -r requirements.txt"
		}
		for _, e := range []string{"main.py", "app.py"} {
			if has(e) {
				d.Entry = e
				break
			}
		}
		if d.Entry == "" {
			return d, fmt.Errorf("a Python project with no main.py or app.py at its root: add a Dockerfile: %w", ErrNoDockerfile)
		}
		return d, nil
	}
	return docker{}, fmt.Errorf("no Dockerfile, and no go.mod, package.json, requirements.txt or pyproject.toml at its root: add a Dockerfile: %w", ErrNoDockerfile)
}

// plainName is a folder name that is safe to write into a Dockerfile's RUN.
var plainName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// commands are the <name> of each cmd/<name>/main.go, sorted. A name with
// anything but letters, digits, '.', '_' and '-' is left out: it would go
// into the Dockerfile's shell.
func commands(paths []string) []string {
	var out []string
	for _, p := range paths {
		if rest, ok := strings.CutPrefix(p, "cmd/"); ok {
			if name, file, ok := strings.Cut(rest, "/"); ok && file == "main.go" && plainName.MatchString(name) && name != "." && name != ".." {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}

func render(name string, data any) ([]byte, error) {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", name, err)
	}
	return buf.Bytes(), nil
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// userNote reads the user a Dockerfile's last stage runs as and says what
// stops Forge running it, or "" for a numeric non-root USER. Forge runs every
// service with runAsNonRoot, which needs a numeric user other than 0.
func userNote(dockerfile []byte) string {
	const fix = "add USER 65532:65532 (a number) to its last stage, or its pods will not start"
	user, seen := "", false
	for _, line := range strings.Split(string(dockerfile), "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			user, seen = "", false
		case "USER":
			if len(fields) > 1 {
				user, seen = fields[1], true
			}
		}
	}
	uid, _, _ := strings.Cut(user, ":")
	switch {
	case !seen:
		return "its Dockerfile's last stage sets no USER, so it runs as root unless its base image sets a non-root one (such as distroless :nonroot): " + fix
	case uid == "0" || uid == "root":
		return "its Dockerfile's last stage runs as root: " + fix
	case !plainUID.MatchString(uid):
		return fmt.Sprintf("its Dockerfile's last stage runs as the user %q, a name, and Kubernetes can only check a user given by number: %s", user, fix)
	}
	return ""
}

var plainUID = regexp.MustCompile(`^[1-9][0-9]*$`)
