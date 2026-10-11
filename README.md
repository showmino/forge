# Forge

Forge is an internal developer platform. This repo holds the `forge` CLI, the
Forge API (`forge-api`) and the Protobuf contract they share.

**Status: Phase 1.** The service catalog, contexts, OIDC login and the API
skeleton work. Templates, environments, deploys and resources come in later
phases (see the design doc).

```
cmd/forge/          CLI entry point; maps errors to exit codes
cmd/forge-api/      API server entry point
internal/cli/       command tree, one file per noun
internal/config/    layered config and kubeconfig-style contexts
internal/auth/      OIDC device flow, keyring storage, token refresh
internal/output/    table, json, yaml and name printers (golden-tested)
internal/manifest/  forge.yaml schema and validation
internal/server/    API handlers: catalog, auth, OIDC verification
internal/store/     catalog storage: memory or PostgreSQL (pgx, embedded migrations)
pkg/client/         Connect client wrapper: auth, retries, request IDs, version headers
proto/forge/v1/     API contract
gen/                buf-generated code (committed; CI checks it is current)
deploy/dex/         Dex for local development
deploy/k3s/         Dex + forge-api manifests for k3s
```

## Quick start (no identity provider)

```sh
make build
make api-dev                       # forge-api on :8080, accepts the token "dev"

# in another shell
export FORGE_TOKEN=dev
bin/forge context set local --api http://localhost:8080 --use
bin/forge service register payments --team billing --owner you@example.com --lifecycle production
bin/forge service list
bin/forge service get payments -o yaml
bin/forge doctor
```

## Local login with Dex

```sh
make dex                           # Dex on :5556 (docker compose)
make api                           # forge-api verifying Dex ID tokens

unset FORGE_TOKEN
bin/forge context set local --api http://localhost:8080 --use
bin/forge auth login               # open the URL, log in as admin@example.com / password
bin/forge auth status
```

The API keeps the catalog in memory unless it gets `--database-url` (or
`FORGE_DATABASE_URL`). `make db` starts a local PostgreSQL on port 55432 for
that and for the store tests:
`FORGE_TEST_DATABASE_URL='postgres://postgres:forge@localhost:55432/postgres?sslmode=disable' make test`.

The CLI asks the API which issuer and client ID to use (`AuthService.GetServerInfo`),
runs the OIDC device flow, and stores the ID and refresh tokens in the OS
keychain (a 0600 file on headless Linux). The API verifies ID tokens with the
CLI's client ID as audience.

## On k3s

On a single-node k3s with the bundled Traefik ingress, import a local image:

```sh
make image
docker save forge-api:dev | sudo k3s ctr images import -
deploy/k3s/render.sh <node-ip>.nip.io | kubectl apply -f -

forge context set k3s --api http://forge.<node-ip>.nip.io --use
forge auth login
```

On a cluster without Traefik, or when you are not on the node, push the image
to a registry and give Dex and forge-api their own LoadBalancer IPs (MetalLB):

```sh
docker build -t ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD) .
docker push ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD)
FORGE_API_IMAGE=ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD) \
  deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
# private image: copy a dockerconfigjson pull secret into 'forge', then
kubectl -n forge patch serviceaccount default -p '{"imagePullSecrets":[{"name":"<secret>"}]}'
kubectl -n forge rollout restart deploy/forge-api

forge context set k3s --api http://forge.<api-ip>.nip.io --use
forge auth login
```

For templates and `forge create`, run forge-api as a GitHub App. Put the App's key
in the Secret `forge-github-app` (key `github_app_private_key`), or let the
1Password operator create it, and render with the App ID:

```sh
kubectl -n forge create secret generic forge-github-app --from-file=github_app_private_key=<key.pem>
FORGE_GITHUB_APP_ID=<app-id> FORGE_API_IMAGE=<image> deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
# or, with the 1Password operator instead of the manual Secret:
#   FORGE_ONEPASSWORD_ITEM=vaults/<vault>/items/<item> FORGE_GITHUB_APP_ID=<app-id> ...
```

`FORGE_GITHUB_ORG` (default `showmino`) and `FORGE_TEMPLATES_REPO` (default
`showmino/forge-templates`) pick the org for new repos and the templates repo.

Preview environments (`forge env`) need the App too, plus a free LoadBalancer IP
for their Gateway (Gateway API with Istio's `istio` class) and a dockerconfigjson
pull secret in namespace `forge` that can read the services' images:

```sh
FORGE_PREVIEW_IP=<gateway-ip> FORGE_PREVIEW_PULL_SECRET=<secret> FORGE_GITHUB_APP_ID=<app-id> ... \
  deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
```

Previews are then reached at `http://<env>.preview.<gateway-ip>.nip.io`.

Deploys (`forge deploy`) need previews, the GitOps repo `showmino/forge-deploys`
(private, with an initial commit), and a commit on origin/main that Argo CD
takes the chart from (an unpushed commit makes every Application fail):

```sh
FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=$(git rev-parse origin/main) FORGE_PREVIEW_IP=<gateway-ip> ... \
  deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
```

This adds the Argo CD AppProject and ApplicationSet `forge-services`, a Role
that lets forge-api read Applications, and the Gateway listeners for
`http://<service>.staging.<gateway-ip>.nip.io` and `.production.`. Argo CD's
repo credentials must be able to read both repos.

Once deploys are on, every re-render must include `FORGE_DEPLOYS=1` and the current `FORGE_DEPLOY_CHART_REVISION` (read it with `kubectl -n argocd get applicationset forge-services -o jsonpath='{.spec.template.spec.sources[0].targetRevision}'`); a render without them removes the staging and production routes. Changing the revision re-renders every deployed service.

Both the CLI and the forge-api pod must reach Dex at the same issuer URL
(`http://dex.<node-ip or dex-ip>.nip.io/dex`), which is why Dex is exposed
outside the cluster rather than called by its Service name. On k3s you log in
through GitHub: only members of `FORGE_DEX_GITHUB_ORG` (default the GitHub org)
can log in, and only the teams in `FORGE_WRITER_GROUPS` (default
`<org>:platform-engineering`) may register, create, preview or deploy. Create a
GitHub OAuth App owned by the org with the callback
`http://dex.<dex-ip>.nip.io/dex/callback`, and put its `client_id` and
`client_secret` in the Secret `dex-github`, or in a 1Password item named by
`FORGE_DEX_ONEPASSWORD_ITEM`. The render also creates the CloudNativePG cluster
`forge-db` (2 instances, `longhorn-retain`), which holds the service catalog
and Dex's logins; it needs the CloudNativePG operator. There are no backups yet.

`forge service delete <name>` (and Delete in the dashboard) removes a service
and everything it runs: its previews, its staging and production deployments
with their namespaces, and last its catalog entry. The GitHub repo is kept. If
it fails half way the service stays in the catalog and the delete can be run
again.

A service belongs to a team of the GitHub organization. `forge team list` (and
the dashboard's Create form) read the teams from GitHub as the App, which needs
the organization permission "Members: read" for it.

The web dashboard (Phase 6) calls forge-api from the browser. Render with
`FORGE_WEB_ORIGINS`, its addresses separated by commas
(`http://forge-web.staging.<domain>,http://forge-web.production.<domain>`), to
add Dex's public client `forge-web` and to let those pages, and no others, call
the API (`FORGE_CORS_ORIGINS`, `FORGE_OIDC_WEB_CLIENT_ID` on forge-api). Restart
Dex after changing it. Without it, nothing of the dashboard is rendered.
Phase 1 serves plain
HTTP; add TLS (cert-manager) before real use.

## Using the CLI

Commands follow `forge <noun> <verb>`:

| Group | Verbs |
| --- | --- |
| `service` | `list`, `get`, `register`, `update`, `owners`, `deps`, `delete` |
| `template` | `list`, `get`, `lint` |
| `create` | scaffold a service from a template: repo, first commit, catalog entry |
| `context` | `list`, `use`, `current`, `set` |
| `auth` | `login`, `logout`, `status`, `token` |
| `audit` | `list`, `get` |
| utilities | `init`, `doctor`, `version`, `completion` |

Every command takes `-o table|json|yaml|name`. JSON is the API resource
itself. Settings resolve flags > `FORGE_*` env > `forge.yaml` > user config >
defaults. Inside a repo with a `forge.yaml` (`forge init --team <team>`),
service commands default to that service.

Exit codes: 0 ok, 1 unexpected, 2 usage, 3 not found, 4 not authenticated,
5 permission denied, 6 conflict, 7 operation failed, 8 timed out.

## Templates

Templates live in [`showmino/forge-templates`](https://github.com/showmino/forge-templates):
one directory per template with a `template.yaml` (typed parameters) and a
`skeleton/` rendered with Go `text/template`, released by repo-wide semver
tags.

`forge template list` and `forge template get <name>[@version]` show the
templates of a release (the newest by default). forge-api reads them from
GitHub as the Forge GitHub App (`--github-app-id`, `--github-app-key-file`,
`--github-org`), or from a local checkout with `--templates-dir` (served as
version `dev`).
`forge create <name> --template <template> --team <team>` asks forge-api to
render the template, create the private repo `github.com/<org>/<name>` as the
Forge GitHub App, push the first commit (which starts its CI) and register the
service. `--set name=value` sets parameters, `--dry-run` lists the files
without creating anything, and a failure after the repo exists deletes it again.
`forge op list`, `forge op watch <id>` and `forge op cancel <id>` show and stop long-running work on the API (operations). Phase 3's environments are the first to use them.
`forge template lint [dir]` checks templates locally, with no API or login;
see `docs/specs/2026-10-08-phase-2-templates-design.md` for the format.

`forge env create <name> --service <service>` runs the service's CI image for
`--ref` (default `main`) in its own namespace with a URL, until `--ttl` (default
24h) runs out; `forge env list / get / extend / destroy` manage them. forge-api
serves them with `--previews` (see the k3s section).

`forge deploy start <service> --env staging|production` commits the service's
version to the deploy repo, which Argo CD then rolls out; `forge deploy status`,
`history`, `rollback` and `promote` follow and change it. forge-api serves them
with `--deploys`.

From GitHub Actions, `forge deploy start --env staging` needs no secret: with
`permissions: id-token: write`, the CLI requests the job's OIDC token
(audience `forge`) and forge-api accepts it when `--deploys` is on. The token
only lets the job deploy its own repo's service to staging, at the commit it
runs at (`GITHUB_SHA`), from the default branch, and follow that deploy.
Production stays with humans (`forge deploy promote`). The runner must reach
the API (`FORGE_API_URL`). forge-api checks the token's audience against
`FORGE_GITHUB_OIDC_AUDIENCE` (default `forge`).
forge-api serves the matching CLI for Linux at `$FORGE_API_URL/cli/forge-linux-amd64`
(and `-arm64`), so a job needs no token for the forge repo.

### Versions

Every commit Forge deploys gets a version, as a tag and a GitHub release in
the service's repo:

- **Staging:** a release candidate, `v0.3.1-rc.2`, published as a GitHub
  pre-release. Deploying the same commit again keeps its number.
- **Production:** the final version, `v0.3.1`, on the same commit, marked
  Latest, with notes since the last final version. A promotion is a patch
  unless you pick more: `forge deploy promote payments --bump minor` (or
  `major`). A commit that was released before keeps its version, so a
  rollback shows the version it restores. A service's first release is
  `v0.1.0`.

`forge deploy status` and `forge deploy history` show the version next to the
commit, and `--dry-run` says which version a deploy or promotion would cut.

Pods keep running the image CI built for the commit (`:<sha7>`). The
`release.yml` workflow that comes with every template adds the version to
that same image when Forge pushes the tag, so `ghcr.io/<org>/<service>:v0.3.1`
is the image production runs. Forge cuts versions with its GitHub App
(contents: write), and a deploy fails with `release_failed` before it changes
anything if the tag or release cannot be made; run it again.

### Audit log

Every write call to the Forge API is recorded, refused calls included, with
who made it (a user's email, a CI repository, `system` for forge-api itself,
or `anonymous`), the service, the target and the result. Long-running work
adds a second row with its outcome. Every member can read the log; nothing
can change it.

```sh
forge audit list --service payments --since 7d
forge audit list --actor ana@example.com --failed
forge audit get 1234
```

Events live in PostgreSQL (`forge-db`) when forge-api has `--database-url`
(or `FORGE_DATABASE_URL`); otherwise in memory.

## The API

One Protobuf contract served with [ConnectRPC](https://connectrpc.com): gRPC,
gRPC-Web and plain JSON over HTTP/1.1 from the same handlers.

```sh
curl -s localhost:8080/forge.v1.AuthService/GetServerInfo -H 'Content-Type: application/json' -d '{}'
curl -s localhost:8080/forge.v1.ServiceCatalogService/ListServices \
  -H 'Content-Type: application/json' -H "Authorization: Bearer $(forge auth token)" -d '{}'
```

## Development

```sh
make tools        # buf, protoc-gen-go, protoc-gen-connect-go
make generate     # after editing proto/
make test lint
make update-golden
```

CI runs tests with `-race`, golangci-lint, govulncheck, `buf lint`,
`buf breaking` against the base branch, a check that `gen/` is current, and
`goreleaser check`. Tagging `v*` releases binaries for Linux, macOS and Windows
(amd64, arm64) to GitHub Releases and a Homebrew tap, with checksums, cosign
signatures and SBOMs, plus a `ghcr.io/showmino/forge-api` image.

Releases need a `HOMEBREW_TAP_GITHUB_TOKEN` secret with write access to
`showmino/homebrew-tap`.
