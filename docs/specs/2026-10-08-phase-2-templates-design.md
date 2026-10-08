# Phase 2: Templates and `forge create`

Oct 8, 2026 · Showmino · Draft for review

Implements Phase 2 of the [Forge CLI design doc](../index.md): `forge template list / get / lint` and `forge create` from a Git template repo, with a Go gRPC service as the first golden path.

**Exit gate (from the design doc):** a service scaffolded with `forge create` passes its own CI and appears in the catalog.

## Decisions

| Question | Decision |
| --- | --- |
| Template format | A new Forge format: `template.yaml` with typed parameters, plus a `skeleton/` rendered with Go `text/template`. No Backstage compatibility or importer; Forge will replace Backstage. |
| What `forge create` does | The API fetches the template, renders it, creates the GitHub repo, pushes the first commit (which starts CI) and registers the service. The CLI sends parameters and shows progress. |
| GitHub access | A "Forge" GitHub App installed on the `showmino` org. |
| Template repo | One repo, `showmino/forge-templates`, one directory per template, released together by repo-wide semver tags. |
| Long-running create | A server-streaming RPC with step events and an idempotency key. It moves onto Operations in Phase 3. |
| First template | `go-grpc`: ConnectRPC, the same stack as Forge itself. |

**Assumptions** (confirm or correct during review):

- Service repos are created in the `showmino` org and are private.
- Service images are pushed to `ghcr.io/showmino/<name>`.

## 1. Template format

### Repository

```
showmino/forge-templates
  go-grpc/
    template.yaml
    skeleton/                     # rendered into the new repo
  .github/workflows/lint.yml      # runs `forge template lint` on every pull request
```

A release is a semver tag on the whole repo (`v1.4.0`). "Newest" means the highest semver tag; tags that are not semver are ignored.

### `template.yaml`

Decoded strictly: unknown fields are an error, as with `forge.yaml`.

```yaml
apiVersion: forge.dev/v1
kind: Template
name: go-grpc                    # must equal the directory name
title: Go gRPC service
description: ConnectRPC service with CI, Dockerfile and forge.yaml
tags: [go, grpc]
owner: platform
delimiters: ["{%", "%}"]         # optional; default ["{{", "}}"]
parameters:
  - name: goModule
    type: string                 # string | int | bool | enum
    default: "github.com/{% .Repo.Owner %}/{% .Repo.Name %}"
    pattern: '^[a-z0-9./-]+$'
    description: Go module path
  - name: port
    type: int
    default: 8080
    description: Port the server listens on
  - name: visibility
    type: enum
    values: [internal, public]
    default: internal
    description: Example enum parameter
```

| Field | Rules |
| --- | --- |
| `name` | Lowercase DNS label, equal to the directory name |
| `title`, `description` | Required, single line |
| `tags` | Optional, lowercase words, used by `template list --tag` |
| `delimiters` | Optional pair of non-empty, distinct strings |
| `parameters[].name` | Go identifier (`^[a-zA-Z][a-zA-Z0-9]*$`), unique |
| `parameters[].type` | `string`, `int`, `bool` or `enum` (`enum` requires `values`) |
| `parameters[].required` | A parameter with no `default` is required |
| `parameters[].default` | Must satisfy the type, `pattern` and `values`; a string default may use built-in values (rendered before validation) |
| `parameters[].pattern` | Optional RE2 regexp, strings only |

### Rendering

- Every file under `skeleton/`, and every path segment, is executed as a Go `text/template` with the template's delimiters and `Option("missingkey=error")`.
- Values available to templates:
  - `.Service.Name`, `.Service.Team`, `.Service.Owners`
  - `.Repo.Owner`, `.Repo.Name`, `.Repo.URL`
  - `.Template.Name`, `.Template.Version`
  - `.Params.<name>`, typed (`int` as int, `bool` as bool)
- Functions: `lower`, `upper`, `replace`, `trimPrefix`, `trimSuffix`. Nothing that reads files, runs commands or reaches the network.
- File modes are kept (executable scripts stay executable). Symlinks are rejected.
- A path segment that renders to an empty string drops that file or directory, so templates can make files optional.
- Limits: at most 2,000 files and 10 MiB of rendered output.
- Files that are not valid UTF-8 are copied unchanged instead of rendered.
- The render must produce a `forge.yaml` at the root that passes `manifest.Load` and whose `service.name` and `service.team` equal the requested service and team. The catalog entry is registered from it.

### Lint

`forge template lint` and the server use the same Go package. Lint reports every finding rather than stopping at the first, each with a file, an optional line and a message.

1. `template.yaml` parses strictly and matches the field rules above.
2. `name` equals the directory name.
3. `skeleton/` exists and is not empty.
4. Every file and path segment parses as a template.
5. A test render succeeds, using each parameter's default (or a type-appropriate sample for required parameters) and the sample service `sample-service` with team `sample-team` (a name with a dash, so templates are checked against one).
6. Rendered paths are relative and clean (no `..`), do not collide, and stay within the size limits.
7. The rendered `forge.yaml` is valid and names the sample service and team.

## 2. API and server

All proto changes are additive, so `buf breaking` passes.

### Protos

```proto
// proto/forge/v1/template.proto
service TemplateService {
  rpc ListTemplates(ListTemplatesRequest) returns (ListTemplatesResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
  rpc GetTemplate(GetTemplateRequest) returns (GetTemplateResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;
  }
}

message Template {
  string name = 1;              // "templates/go-grpc"
  string version = 2;           // "v1.4.0"
  string title = 3;
  string description = 4;
  repeated string tags = 5;
  string owner = 6;
  repeated TemplateParameter parameters = 7;
}

message TemplateParameter {
  enum Type { TYPE_UNSPECIFIED = 0; TYPE_STRING = 1; TYPE_INT = 2; TYPE_BOOL = 3; TYPE_ENUM = 4; }
  string name = 1;
  Type type = 2;
  bool required = 3;
  string default_value = 4;     // as written in template.yaml
  string pattern = 5;
  repeated string values = 6;
  string description = 7;
}

message ListTemplatesRequest {
  string version = 1;           // empty = newest tag
  string tag = 2;               // only templates with this tag
}
message ListTemplatesResponse { repeated Template templates = 1; }

message GetTemplateRequest {
  string name = 1;              // "templates/go-grpc"
  string version = 2;           // empty = newest tag
}
message GetTemplateResponse { Template template = 1; }
```

```proto
// added to ServiceCatalogService in catalog.proto
rpc CreateService(CreateServiceRequest) returns (stream CreateServiceEvent) {}

message CreateServiceRequest {
  string service_id = 1;              // becomes services/{id} and showmino/{id}
  string template = 2;                // "templates/go-grpc"
  string template_version = 3;        // empty = newest tag
  map<string, string> parameters = 4; // values as strings; the server converts by type
  string owner_team = 5;
  repeated string owners = 6;
  string request_id = 7;              // idempotency key
  bool validate_only = 8;             // render and check; create nothing
}

message CreateServiceEvent {
  oneof event {
    CreateStep step = 1;
    Service service = 2;              // success: the registered service (last event)
    CreateFailure failure = 3;        // failure after side effects began (last event)
    CreatePlan plan = 4;              // validate_only: what would be created (last event)
  }
}

message CreateStep {
  string name = 1;                    // fetch, render, create_repo, push, register
  enum State { STATE_UNSPECIFIED = 0; STATE_STARTED = 1; STATE_DONE = 2; }
  State state = 2;
  string detail = 3;                  // e.g. "go-grpc@v1.4.0", "23 files", commit SHA
}

message CreateFailure {
  string step = 1;
  string message = 2;
  bool rolled_back = 3;               // the repo was deleted again
}

message CreatePlan {
  string template_version = 1;
  string repo_url = 2;
  repeated string files = 3;
}
```

Errors found before any side effect (bad input, unknown template, conflicts, missing configuration) end the stream with a Connect error code. A failure after the repo exists ends with a `CreateFailure` event, because a code alone cannot say which step failed or whether the rollback worked.

### Components

| Package | Responsibility |
| --- | --- |
| `internal/template` | Parse, lint and render a template from an `fs.FS`. Pure Go, no I/O of its own. Used by the CLI and the server. |
| `internal/server/templates.go` | `TemplateSource` interface: `Versions(ctx)` and `Fetch(ctx, version) (fs.FS, error)`. `GitHubSource` downloads a tag's tarball and caches the extracted tree in memory per tag. `DirSource` serves a local checkout as version `dev`, for development and tests. Also the `TemplateService` handlers. |
| `internal/github` | GitHub App client: app JWT, installation token (cached until shortly before expiry), tarball download, create repo, push the first commit with the Git Data API (blobs, tree, commit, ref) so the image needs no `git`, delete repo. |
| `internal/server/create.go` | The `CreateService` pipeline. |

### The create pipeline

Requires `services.write`. Step 1 runs before any event is sent; steps 2, 3, 5, 6 and 7 are each sent as a `CreateStep` started and done:

1. **Check:** validate `service_id` (DNS label), parameters (types, patterns, enums, unknown keys, at most 1 KiB each), team and owners. Fail with `ALREADY_EXISTS` if `services/{id}` is registered or `showmino/{id}` exists on GitHub.
2. **fetch:** resolve the version (newest tag if empty) and fetch the template.
3. **render:** render with the request values, and check the rendered `forge.yaml` names the requested service and team.
4. With `validate_only`, stop here and send a `CreatePlan`.
5. **create_repo:** create the private repo `showmino/{id}` with no auto-init.
6. **push:** push one commit with every rendered file to `main`. The `.github/workflows` it contains start CI.
7. **register:** register `services/{id}` in the catalog with `repo_url` and the fields from the rendered `forge.yaml`.
8. Send the `Service`.

If step 6 or 7 fails, delete the repo (best effort) and send a `CreateFailure` with `rolled_back` set to whether the delete worked.

**Idempotency:** the outcome of each `request_id` (success, failure or in progress) is kept in memory, like the Phase 1 catalog. A retry with the same key gets the stored outcome replayed as a short stream instead of a second repo. A retry while the first attempt is still running gets `ABORTED` ("in progress").

**Logging:** each create logs the caller's subject, the template and version, the repo and the outcome. The audit log proper is Phase 5.

### Configuration

New forge-api flags, each with a `FORGE_` environment variable like the existing ones:

| Flag | Purpose |
| --- | --- |
| `--github-app-id` | GitHub App ID |
| `--github-app-key-file` | Path to the App's private key (mounted from a k8s Secret) |
| `--github-org` | Org for new repos (default `showmino`) |
| `--templates-repo` | Template repo (default `showmino/forge-templates`) |
| `--templates-dir` | Serve templates from a local directory instead (development) |

Without a template source, `TemplateService` returns `FAILED_PRECONDITION` ("templates are not configured"). Without GitHub App settings, `CreateService` does the same, except with `validate_only`, which works against `--templates-dir`.

`TemplateService` requires an authenticated caller but no permission: templates are not sensitive.

GitHub App authentication ships in milestone 2 (the App, `mino-forge`, exists): the API finds the App's installation from `--github-org` (`GET /orgs/{org}/installation`), so no installation ID is configured.

### GitHub App

Created once by an org owner from a manifest that will be included in the implementation:

- Repository permissions: Administration write (create and delete repos), Contents write, Workflows write (push `.github/workflows`), Metadata read.
- No webhooks or events.
- Installed on the `showmino` org with access to **all repositories**, so it can reach the repos it creates and read `forge-templates`.

Image pushes from service CI use each repo's own `GITHUB_TOKEN`, not the App.

## 3. CLI

New files `internal/cli/template.go` and `internal/cli/create.go`, built with `Deps` and `Factory` like the Phase 1 commands. `pkg/client` gains the `TemplateService` client and a helper that consumes the `CreateService` stream.

### `forge template list [--tag T] [--version V]`

```
NAME      VERSION   TAGS       DESCRIPTION
go-grpc   v1.4.0    go, grpc   ConnectRPC service with CI, Dockerfile and forge.yaml
```

`-o json` prints `{"templates": [...]}` with the `Template` messages.

### `forge template get <name>[@version]`

Prints the template's fields, then its parameters:

```
NAME         TYPE     REQUIRED   DEFAULT                                     DESCRIPTION
goModule     string   no         github.com/{% .Repo.Owner %}/{% .Repo.Name %}   Go module path
port         int      no         8080                                        Port the server listens on
```

`-o json` prints the `Template` message.

### `forge template lint [dir]`

- Runs locally; needs no API or login.
- `dir` may be one template (it has a `template.yaml`) or a repo of templates (every child directory with a `template.yaml`). Default `.`.
- Prints `go-grpc/template.yaml:12: parameter "port": default "x" is not an int`, one line per finding, to stdout.
- Exit 0 when clean, 1 when there are findings (as `doctor` does for failed checks), 2 when `dir` has no template.
- `-o json` prints `{"findings": [{"template", "file", "line", "message"}]}`.
- A hidden `--render-dir DIR` flag also writes each clean template's sample render to `DIR/<template>` (used by the template repo's CI).

### `forge create <name> --template <t>[@version] --team <team>`

| Flag | Meaning |
| --- | --- |
| `--template` | Required. Template name, optionally `@version` |
| `--team` | Required (prompted on a terminal) |
| `--owner` | Repeatable |
| `--set key=value` | Repeatable template parameter |
| `--dry-run` | Sends `validate_only`; prints the files that would be created |

- **Before the call:** the CLI fetches the template with `GetTemplate`, checks `--set` keys, types, patterns and enums, and on a terminal prompts for missing required parameters. With `--no-input` or a non-terminal stdin, a missing parameter fails with exit 2 and names the `--set` flag. The server validates everything again.
- **Progress:** step lines go to stderr (`✓ created github.com/showmino/payments`); with `-q` they are hidden.
- **Result:** the registered service goes to stdout in the chosen format, like `service get`.
- **Retries:** one `request_id` per invocation. If the stream drops before a final event, the CLI retries once with the same key.

## 4. Errors, security and testing

### Errors

| Situation | Server | CLI exit |
| --- | --- | --- |
| Bad parameter, service id, team or owner | `INVALID_ARGUMENT`, naming the field | 2 |
| Template or version not found | `NOT_FOUND` | 3 |
| `services/{id}` or `showmino/{id}` exists | `ALREADY_EXISTS` | 6 |
| Templates or GitHub not configured | `FAILED_PRECONDITION` | 6 |
| Same `request_id` still running | `ABORTED` | 6 |
| GitHub unavailable or rate-limited before any side effect | `UNAVAILABLE` | 1 |
| A step fails after the repo exists | `CreateFailure` event | 7 |
| `--timeout` reached | client deadline | 8 |

### Security

- The App private key exists only in a Secret mounted read-only. Installation tokens are kept in memory and never logged.
- New repos are private. Only callers with `services.write` may create.
- Rendering cannot read files, run commands or reach the network. Paths are checked for `..`, absolute paths and collisions, and sizes are capped.
- Tarball extraction rejects symlinks, hard links, path traversal and entries over the size limit.
- `service_id` is a DNS label, so it is a safe repo name. Parameters are validated and capped before anything reaches GitHub.

### Testing

All new code is written test first.

- `internal/template`: a table test per lint rule with fixture templates; a golden test of a fixture's render; delimiters, `missingkey=error`, optional paths, file modes and limits.
- `internal/github`: against an `httptest` fake GitHub that records repos, blobs, trees, commits and refs, including JWT exchange and token refresh.
- Create pipeline: success, `validate_only`, both conflicts, failed push with rollback, failed rollback, idempotent replay, in-progress retry.
- CLI harness (`cli_test.go`): `template list/get/lint` and `create` against the in-process API with `DirSource` and the fake GitHub, covering prompts, `--no-input`, every exit code above and `-o json`.
- Golden files for the `template list`, `template get` and `template lint` table and JSON output (`make update-golden`).
- `cli-smoke.sh` gains template and create cases (create runs with `--dry-run` unless GitHub is configured).
- End to end on k3s, which is the exit gate: `forge create forge-e2e-<yyyymmdd> --template go-grpc --team platform` with the real App, `gh run watch` until its CI passes, `forge service get` shows it, then delete the repo and the catalog entry.

## 5. The `go-grpc` template (v1.0.0)

```
go-grpc/skeleton/
  forge.yaml
  README.md
  go.mod                          # module {% .Params.goModule %}, go 1.24
  buf.yaml  buf.gen.yaml
  proto/{% replace `-` `` .Service.Name %}/v1/greeter.proto  # no "-" in Go and proto names; no " or | (Windows)
  .gitignore                      # gen/
  cmd/{% .Service.Name %}/main.go # slog, graceful shutdown, /healthz, Connect handler
  internal/server/greeter.go
  internal/server/greeter_test.go
  Dockerfile                      # distroless, USER 65532:65532
  Makefile                        # build, test, lint, generate
  .golangci.yml
  .github/workflows/ci.yml
```

CI (`ci.yml`) runs `buf lint`, `make generate`, `go test -race ./...` and golangci-lint on every push and pull request. On `main` it also builds the image and pushes `ghcr.io/showmino/{% .Service.Name %}:<short-sha>` with the repo's `GITHUB_TOKEN`.

Generated code is not part of the skeleton: `.pb.go` files embed the proto file path and Go package as length-prefixed descriptor bytes, so rendering a new name into them would corrupt them. Scaffolded services run `buf generate` (`make generate`, plugins pinned with `go install`) in CI, in the Dockerfile and locally, and keep `gen/` out of Git. This differs from Forge itself, which commits `gen/`.

The template's own CI renders `go-grpc` with sample values and runs the rendered project's `make generate test lint`, so a template change that breaks scaffolded services fails before it is tagged.

## 6. Rollout

Four milestones, each ending in something that works on its own:

1. **Template format and lint.** `internal/template`, `forge template lint`, the `showmino/forge-templates` repo with `go-grpc` v1.0.0 and its lint CI. Works offline; no API changes.
2. **Template service.** GitHub App authentication, `TemplateService` with `DirSource` and `GitHubSource`, `forge template list / get`.
3. **Create.** repo creation in `internal/github`, the `CreateService` pipeline, `forge create`.
4. **k3s and the exit gate.** App key Secret, forge-api flags in the k3s manifests, the end-to-end run.

### Needs from the owner

| When | What | Status |
| --- | --- | --- |
| Milestone 1 | Create `showmino/forge-templates` (private) | Approved |
| Milestone 1 | How the template repo's CI gets the `forge` binary, since both repos are private. Proposal: a read-only fine-grained token for `showmino/forge` (Contents: read) as the org secret `FORGE_READ_TOKEN`; the template repo's CI checks out and builds `forge` with it. | Open |
| Milestone 3 | Create the Forge GitHub App from the manifest; share the App ID and the path of the saved private key (never the key itself) | Open |
| Milestone 4 | Create the k8s Secret from the key; run the end-to-end create, which creates and deletes a real repo | Open |

## Out of scope for Phase 2

- Operations, `forge op watch` and detaching from a create (Phase 3).
- Updating existing services when a template changes.
- Templates from repos other than `forge-templates`, or per-template versions.
- Persistent storage for the catalog and idempotency records (in memory, as in Phase 1).
- Public repos and orgs other than `showmino`.
