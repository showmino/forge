# Phase 10 · Source deploy, as built

Designed in `2026-10-29-deploy-sources-design.md` (phase 10); the owner gave
the go-ahead on 2026-10-30. This is what was built.

## What

A repo in the organization that has a Dockerfile, or Go, Node or Python code,
and no CI for Forge, is connected with one command:

```sh
forge service connect github.com/my-org/tool --team web [--name tool] [--port 8080] [--health /]
```

Forge registers the service and opens a pull request in the repo that adds:

| File | What |
| --- | --- |
| `forge.yaml` | The service (name, team, owners, repo) and `run.port`, `run.health` |
| `.github/workflows/forge.yml` | Build the image `<registry>/<org>/<service>:<sha7>` on `main` and on pull requests from the repo (previews need it), then on `main` deploy it to staging with `forge deploy start --env staging`, logged in with the job's GitHub OIDC token (phase 4) |
| `.github/workflows/forge-release.yml` | When Forge pushes a version tag, add it to the image CI built for that commit, as the templates' `release.yml` does |
| `Dockerfile` | Only when the repo has none: one for the language Forge finds (below) |

Merging the pull request is the first deploy. Nothing runs in the cluster to
build: the build is the repo's own Actions run, visible in the repo, and the
deploy goes through the CI path phase 4 built and its limits (a CI token
deploys only its own service, to staging, at its commit, from the default
branch).

## In the API

`CreateService` takes a third source, `repo_url`, next to `template` and
`image`; the steps stream as for the others:

1. `check_repo`: the repo is in the organization and the GitHub App can read
   it; it has no `forge.yaml` and no `.github/workflows/forge.yml` yet, and
   no service in the catalog has it as its repo. It has a `Dockerfile` at its
   root, or Forge finds a language.
2. `render`: the files above.
3. `register`: the service, with the repo as its repo.
4. `pull_request`: branch `forge/connect`. If it cannot be opened, the
   service is removed from the catalog again.

The last message is the proposed change, with the service and which
Dockerfile was proposed (empty when the repo had its own).
`validate_only` stops after `render` and lists the files.

## Finding a language (no Dockerfile)

| Found at the root | Dockerfile |
| --- | --- |
| `go.mod` | Go's version from `go.mod`; builds `.` when `main.go` is at the root, else `./cmd/<name>` (the service's name, or the only folder under `cmd/`) into a distroless static image |
| `package.json` | Node 24: `npm ci` with a `package-lock.json` (else `npm install`), `npm run build` when there is a build script; runs `npm start` when there is a start script, else `node <main>` |
| `requirements.txt` or `pyproject.toml` | Python 3.13 slim: installs it; runs `main.py` or `app.py` |

Every proposed image runs as a numeric non-root user with a read-only root
filesystem (writes go to `/tmp`; `HOME` is `/tmp`), as phase 9 requires. A
repo with none of these, or a Go repo with no `main` package Forge can
find, or a Python repo with no `main.py` or `app.py`, is refused with
"add a Dockerfile". The pull request is a proposal: the team reviews the
Dockerfile before it builds anything.

## The workflow's settings

The workflow is Forge's, not a template's, so it cannot name one install's
runners. forge-api's `--ci-runner` (`FORGE_CI_RUNNER`, chart `ci.runner`,
default `ubuntu-latest`) is where the image is built, and `--ci-deploy-runner`
(`FORGE_CI_DEPLOY_RUNNER`, chart `ci.deployRunner`, default the same) is
where the deploy job runs: it must reach the Forge API, at the organization
variable `FORGE_API_URL`, as the templates' does. The registry is forge-api's
`--image-registry`; for `ghcr.io` the job logs in with its own token, for any
other registry with the repo secrets `FORGE_REGISTRY_USERNAME` and
`FORGE_REGISTRY_PASSWORD` (the pull request says so).

## Not in this phase

- Cloud Native Buildpacks, and languages other than Go, Node and Python.
- Repos outside the organization.
- The dashboard's "Connect a repo": it comes with forge-web after this API
  is merged, as a third choice on the Create page.
