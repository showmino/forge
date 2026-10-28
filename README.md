# Forge

[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/forge)](https://artifacthub.io/packages/search?repo=forge)

Forge is an internal developer platform. This repo holds the `forge` CLI, the
Forge API (`forge-api`) and the Protobuf contract they share.

**Status (October 2026):** the catalog, templates and `forge create`, preview
environments, deploys with promotion and rollback, versions and releases, the
audit log, public addresses and the web dashboard
([`showmino/forge-web`](https://github.com/showmino/forge-web)) are built and
live. Resources (databases) are built and switched off on the cluster;
TechDocs is designed. [`docs/index.md`](docs/index.md) has the design.

**Working on Forge**, as a person or as a coding agent: read
[`AGENTS.md`](AGENTS.md) first. It has the rules (the cluster is production),
the commands and the conventions, and points to the runbook.

## Install

Download the archive for your OS from the
[latest release](https://github.com/showmino/forge/releases/latest), put
`forge` on your `PATH`, point it at your Forge's API and log in:

```sh
forge context set mine --api https://forge.example.com --use
forge auth login
```

`FORGE_API_URL` or `--api-url` name an API for one command instead.

After that, `forge upgrade` brings the CLI to the API's version on Linux,
macOS and Windows: the API serves the release's archives at
`/cli/releases/v<version>/<file>`, so no GitHub access is needed, and the
download is checked against the release's checksums (`--check` only looks).
When the API is on plain HTTP (not `localhost`), the checksums come from the
release on GitHub over HTTPS instead, so that one needs GitHub.

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

The API keeps its data in PostgreSQL at `--database-url` (or
`FORGE_DATABASE_URL`) and does not start without it. `make api` and
`make api-dev` pass `--in-memory` (`FORGE_IN_MEMORY=1`) instead: for
development only, everything is lost when forge-api stops. `make db` starts a
local PostgreSQL on port 55432 for a real database and for the store tests:
`FORGE_TEST_DATABASE_URL='postgres://postgres:forge@localhost:55432/postgres?sslmode=disable' make test`.

The CLI asks the API which issuer and client ID to use (`AuthService.GetServerInfo`)
and runs the OIDC device flow. The API verifies ID tokens with the CLI's
client ID as audience.

### Sessions

A login does not last on its own: the identity provider's tokens are short,
and with GitHub behind Dex the refresh dies as soon as GitHub revokes the
token of that login (it keeps ten per person, and every sign-in anywhere makes
one). So right after the login the CLI trades the ID token for a session of
Forge's own (`AuthService.CreateSession`) and keeps only that, in the OS
keychain (a 0600 file on headless Linux):

- **How long:** a CLI session ends when it has not been used for 90 days; a
  dashboard session ends 7 days after the sign-in. `forge auth logout` ends
  one at once, `forge auth session list` shows yours on every machine, and
  `forge auth session revoke <id>` ends another one.
- **Who:** forge-api asks GitHub about hourly, with its own App (Members:
  read), whether the person is still in the organization and in the writer
  teams. Leaving a team takes the permission away within the hour; leaving
  the organization ends the session. If GitHub cannot be asked, a session
  goes on with what was last known for at most a day.
- **What is stored:** the SHA-256 of the token, never the token
  (`fgs_...`), in the `sessions` table. A session is made only from a login
  less than ten minutes old, once per login, and never from another session
  or a CI token. With sessions the CLI asks for no refresh token, and a new
  login on a machine ends the session that machine had.

forge-api keeps sessions only when it has the GitHub App, because that is how
it knows a person has left (`GetServerInfo` says `sessions: true`). Without
it, and against an older API, `CreateSession` answers `Unimplemented` and the
CLI keeps the ID and refresh tokens, as before.

## On k3s

> This section is for a throwaway single-node k3s of your own. **The
> `showmino` cluster is production and is not set up this way:** its images
> come from `ghcr.io`, and every change to it follows the rollout procedure in
> showmino's private ops repo with the owner's OK.

On a single-node k3s with the bundled Traefik ingress, import a local image:

```sh
make image
docker save forge-api:dev | sudo k3s ctr images import -
FORGE_GITHUB_ORG=<org> deploy/k3s/render.sh <node-ip>.nip.io | kubectl apply -f -

forge context set k3s --api http://forge.<node-ip>.nip.io --use
forge auth login
```

`FORGE_INGRESS_CLASS` (default `traefik`) picks a different `IngressClass`
for a cluster whose ingress controller isn't Traefik.

On a cluster without Traefik, or when you are not on the node, push the image
to a registry and give Dex and forge-api their own LoadBalancer IPs (MetalLB):

```sh
docker build -t ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD) .
docker push ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD)
FORGE_API_IMAGE=ghcr.io/showmino/forge-api:$(git rev-parse --short HEAD) FORGE_GITHUB_ORG=<org> \
  deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
# private image: copy a dockerconfigjson pull secret into 'forge', then
kubectl -n forge patch serviceaccount default -p '{"imagePullSecrets":[{"name":"<secret>"}]}'
kubectl -n forge rollout restart deploy/forge-api

forge context set k3s --api http://forge.<api-ip>.nip.io --use
forge auth login
```

Without MetalLB (a cloud provider's own LoadBalancer implementation, or
`minikube tunnel`), use `--cloud-lb <dex-host> <api-host>` instead of `--lb`:
hostnames you already control, not an IP MetalLB would annotate. Point them
at whatever address the cluster assigns afterward — and make sure they
resolve from inside the cluster too (the same requirement the nip.io modes
already have, below): forge-api's pod discovers Dex's OIDC issuer at
startup and fails to start if it can't resolve the Dex hostname.

```sh
FORGE_GITHUB_ORG=<org> deploy/k3s/render.sh --cloud-lb dex.example.com forge.example.com | kubectl apply -f -
```

For templates and `forge create`, run forge-api as a GitHub App. Put the App's key
in the Secret `forge-github-app` (key `github_app_private_key`), or let the
1Password operator create it, and render with the App ID:

```sh
kubectl -n forge create secret generic forge-github-app --from-file=github_app_private_key=<key.pem>
FORGE_GITHUB_ORG=<org> FORGE_GITHUB_APP_ID=<app-id> FORGE_API_IMAGE=<image> deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
# or, with the 1Password operator instead of the manual Secret:
#   FORGE_ONEPASSWORD_ITEM=vaults/<vault>/items/<item> FORGE_GITHUB_ORG=<org> FORGE_GITHUB_APP_ID=<app-id> ...
```

`FORGE_GITHUB_ORG` is required by every render (Dex's GitHub connector needs
it regardless of the App). `FORGE_TEMPLATES_REPO`, `FORGE_REPO` and
`FORGE_DEPLOY_REPO` default to `<org>/forge-templates`, `<org>/forge` and
`<org>/forge-deploys`; set them to point at different repos.

Preview environments (`forge env`) need the App too, plus a free LoadBalancer IP
for their Gateway (Gateway API with Istio's `istio` class, `FORGE_GATEWAY_CLASS`
to use a different implementation) and a dockerconfigjson
pull secret in namespace `forge` that can read the services' images:

```sh
FORGE_GITHUB_ORG=<org> FORGE_PREVIEW_IP=<gateway-ip> FORGE_PREVIEW_PULL_SECRET=<secret> FORGE_GITHUB_APP_ID=<app-id> ... \
  deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
```

Previews are then reached at `http://<env>.preview.<gateway-ip>.nip.io`.

Without MetalLB, `FORGE_GATEWAY_DOMAIN` replaces `FORGE_PREVIEW_IP` (set only
one of the two): the cluster's own LoadBalancer implementation assigns the
Gateway's address, with no requested-IP annotation, and previews are reached
at `http://<env>.preview.<domain>` instead — point that hostname (and
`staging.`/`production.` once deploys are on) at the assigned address
yourself, the same way `--cloud-lb`'s hostnames are.

Deploys (`forge deploy`) need previews, the GitOps repo (`FORGE_DEPLOY_REPO`,
default `<org>/forge-deploys`; private, with an initial commit), and the
published deploy chart's version: a `vX.Y.Z` release tag publishes
`deploy/argocd/chart` to `oci://ghcr.io/<org>/forge-chart` (`FORGE_CHART_REPO`
picks a different path), and `FORGE_DEPLOY_CHART_REVISION` is that version
without the leading `v` (e.g. `0.20.0`, not a commit SHA):

```sh
FORGE_GITHUB_ORG=<org> FORGE_DEPLOYS=1 FORGE_DEPLOY_CHART_REVISION=0.20.0 FORGE_PREVIEW_IP=<gateway-ip> ... \
  deploy/k3s/render.sh --lb <dex-ip> <api-ip> | kubectl apply -f -
```

This adds the Argo CD AppProject and ApplicationSet `forge-services`, a Role
that lets forge-api read Applications, and the Gateway listeners for
`http://<service>.staging.<gateway-ip>.nip.io` and `.production.`. Argo CD's
repo credentials must be able to read both the values repo and the (possibly
private) OCI registry the chart is published to.

Once deploys are on, every re-render must include `FORGE_DEPLOYS=1` and the current `FORGE_DEPLOY_CHART_REVISION` (read it with `kubectl -n argocd get applicationset forge-services -o jsonpath='{.spec.template.spec.sources[0].targetRevision}'`); a render without them removes the staging and production routes. Changing the revision re-renders every deployed service.

Both the CLI and the forge-api pod must reach Dex at the same issuer URL
(`http://dex.<node-ip or dex-ip>.nip.io/dex`), which is why Dex is exposed
outside the cluster rather than called by its Service name. On k3s you log in
through GitHub: only members of `FORGE_DEX_GITHUB_ORG` (default the GitHub org)
can log in. The teams in `FORGE_WRITER_GROUPS` (default
`<org>:platform-engineering`) are admins, who may do everything; forge-api
reads them as `FORGE_ADMIN_GROUPS`, its newer name, when that is unset. Create a
GitHub OAuth App owned by the org with the callback
`http://dex.<dex-ip>.nip.io/dex/callback`, and put its `client_id` and
`client_secret` in the Secret `dex-github`, or in a 1Password item named by
`FORGE_DEX_ONEPASSWORD_ITEM`. `FORGE_DEX_THEME=1` gives Dex's pages (the sign-in page, the CLI's device code, errors) the dashboard's look: the files in `deploy/platform/chart/files/dex-theme` (styles, logo, header, fonts) become the ConfigMap `dex-theme`, mounted beside Dex's own files. The render also creates the CloudNativePG cluster
`forge-db` (2 instances), which holds the service catalog
and Dex's logins; it needs the CloudNativePG operator. `FORGE_DB_STORAGE_CLASS`
names its StorageClass explicitly (showmino uses `longhorn-retain`, so its
volumes outlive an accidental delete); left unset, the cluster's own default
StorageClass is used. There are no backups yet.

`forge service delete <name>` (and Delete in the dashboard) removes a service
and everything it runs: its previews, its staging and production deployments
with their namespaces, and last its catalog entry. The GitHub repo is kept. If
it fails half way the service stays in the catalog and the delete can be run
again.

A service belongs to a team of the GitHub organization. `forge team list` (and
the dashboard's Create form) read the teams from GitHub as the App, which needs
the organization permission "Members: read" for it.

The owner team also gets the role `maintain` on the service's repo, when
forge-api has a GitHub App: `forge create` gives it right after making the
repo (step `grant_team`; if that fails the repo is deleted again),
`forge service register` before it registers, and moving a service to
another team or repo gives the new team the repo and takes it from the old
one (unless another service of that repo still belongs to the old team).
If GitHub refuses, nothing is registered or moved. A team that already has a
higher role (admin) keeps it, and a move only takes away the role
`maintain`. Only repos in the
organization are touched; deleting a service leaves the repo and its access
as they are.

Who may do what follows the team. Every member of the organization can read
everything. The members of a service's owner team may update it, deploy,
promote and roll it back, manage its previews and plan and apply its
resources outside production, and create services for their own teams. Only
an admin may delete a service, change its address, register an existing
repo or point a service at another repo, create a service for another team,
move a service to another team or apply resources in production. A refusal names the action and the team it
needs. `forge auth can deploy.promote payments` prints yes or no (exit 0 or
5), `forge auth status` shows your role and `forge service get` what you may
do on that service.

A service's docs are Markdown under `docs/` in its repo, with `docs/index.md`
as the start page (Phase 7). forge-api reads them from the repo's default
branch as the App (the "Contents: read" it has already), at most once a minute
per service, and nothing is built: `forge docs list`, `forge docs get payments
runbook.md`, `forge docs search restart queue` (pages holding every word,
across all services or one with `--service`), and `forge docs lint` in a repo
to check them before a push. An optional `docs:` list in forge.yaml sets the
order and titles of the pages.

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

When Cloudflare answers a request with a challenge for a browser
(`cf-mitigated: challenge`, often from a company network or VPN), the CLI
says so with the Ray ID and exits 1, not 5: Forge never saw the request. Find
the Ray ID under Security > Events in Cloudflare to see which rule fired.

## Templates

Templates live in [`showmino/forge-templates`](https://github.com/showmino/forge-templates):
one directory per template with a `template.yaml` (typed parameters) and a
`skeleton/` rendered with Go `text/template`, released by repo-wide semver
tags.

`forge template list` and `forge template get <name>[@version]` show the
templates of a release (the newest by default). forge-api reads them from
GitHub as the Forge GitHub App (`--github-app-id`, `--github-app-key-file`,
`--github-org`, `--templates-repo`), or from a local checkout with
`--templates-dir` (served as version `dev`).
`forge create <name> --template <template> --team <team>` asks forge-api to
render the template, create the private repo `github.com/<org>/<name>` as the
Forge GitHub App, give the owner team `maintain` on it, push the first commit (which starts its CI) and register the
service. `--set name=value` sets parameters, `--dry-run` lists the files
without creating anything, and a failure after the repo exists deletes it again.
A template's `options:` list (in `template.yaml`, e.g. `options: [replicas,
env, secrets, jobs]`) names the run options its create form offers;
`forge create` sets them with `--replicas`, `--resources`, `--env`,
`--secret`, `--job` and `--autoscaling-max`, and forge-api writes them into
the new repo's `forge.yaml` (see "Run options").
A file template (`type: file`, `target: <owner>/<repo>` in `template.yaml`)
makes no repo: `forge create <name> --template <template> --team <team>`
renders it and opens a pull request that adds its files to the target repo,
and prints the pull request's URL. It refuses when a file is there already.
Parameters can carry a `label`, `placeholder` and `group` for forms, an int
`min` and `max`, and the type `list` (several of `values`, comma-separated);
see `docs/specs/2026-10-14-file-templates-design.md`.
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

A promotion goes ahead only when staging is ready: Argo CD reports it Synced
and Healthy, at its latest deploy. Otherwise `forge deploy promote` (and its
`--dry-run`) is refused for everyone, admins too, with the reasons (exit 5):
`staging is Degraded; it must be Healthy to be promoted`. Repair staging, or
wait for the deploy to sync, and promote again. A rollback is never checked,
so production can always be rolled back. The rules are `policy/deploys.rego`
(package `forge.deploys`), built into forge-api like the resource rules; the
audit log records `policy: passed` or `denied` on each promotion.

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

### Run options

`forge.yaml`'s `run:` section also says how a service runs in staging and
production. Every field is optional; without them a service runs one pod with
the namespace's default resources:

```yaml
run:
  replicas: 3                 # production (1 to 10); staging always runs one
  resources:
    requests: {cpu: 100m, memory: 128Mi}
    limits: {cpu: 500m, memory: 512Mi}
  env: {LOG_LEVEL: info}      # plain environment variables
  secrets:                    # secret items, as environment variables
    - name: stripe
      item: vaults/Apps/items/stripe
      env: {STRIPE_KEY: credential}   # variable: field of the item
  jobs:                       # cron jobs: the service's image, env and secrets
    - name: cleanup
      schedule: "0 3 * * *"
      command: [node, scripts/cleanup.js]
  autoscaling: {max: 6}       # production grows from replicas to max on CPU
```

`forge service options check` checks them and shows what each environment
gets, without the API; `forge service options set replicas=3 env.LOG_LEVEL=debug`
changes them in `forge.yaml`, keeping its comments. forge-api checks them again when it deploys. Everything
that may run at once in an environment must fit the namespace's quota
(requests 2 CPU / 2Gi, limits 4 CPU / 4Gi, 15 pods): the replicas or the
autoscaling maximum, one more pod while a rollout replaces them, one per cron
job, and the database's instances. `item` is `vaults/<vault>/items/<item>` by
default (1Password, `--secret-backend`'s default); a Forge instance
configured with a different `--secret-backend` also accepts the generic
`<group>/<item>` form, and something other than the 1Password Connect
operator is then expected to populate the Secret. Either way a secret must
come from a group forge-api allows (`--secret-vaults`, none by default), may
not be one of the items forge-api denies (`--secret-denied-items`, the
platform's own credentials), and must be named by its title, not an opaque
ID; it becomes the Secret `<name>-secret`. A change applies at
the next deploy (staging) or promote (production). A forge-api older than
these fields refuses a `forge.yaml` that has them.

### Addresses

Where a service is reached depends on whether the platform has public
domains (`--public-domains`, e.g. `showmino.com=showmino-com,deogratias.cloud=deogratias-cloud`:
each domain with the name of its DNS zone, the default first).

- **Without them** a service has an internal address,
  `http://<service>.<env>.<deploy domain>`.
- **With them** a service has a public one, over HTTPS:
  `https://payments.showmino.com` in production and
  `https://payments-staging.showmino.com` in staging. One label under the
  domain, because the edge's certificate for `*.showmino.com` covers nothing
  deeper.
- **A service chooses** in `forge.yaml`:

  ```yaml
  run:
    domain: deogratias.cloud   # another of the platform's domains
    # domain: none             # no public address: the internal one
    host: shop                 # the name before the domain, when it is
                               # not the service's own: shop.<domain>
                               # and shop-staging.<domain>
  ```

  `run.host` cannot be the name of another service in the catalog, and like
  every new address it must not resolve yet.

  To keep staging on the internal address while production is public:

  ```yaml
  run:
    staging:
      public: false            # staging: http://<service>.staging.<deploy domain>
  ```

  Production keeps its public address, and an address set in Forge (below)
  still names it; staging gets no DNS record. It is kept with each deploy, so
  a promotion or rollback carries it (a rollback never makes an internal
  staging public), and it applies at the next staging deploy, which says the
  public staging address stops working. A forge-api
  older than this field rejects a `forge.yaml` that has it.

  A new service can start either way, with every service template:
  `forge create <name> ... --internal` (in the dashboard's Visibility step:
  "Private") writes `run.domain: none` into its forge.yaml, and
  `--internal-staging` (in the dashboard: "Public production") writes
  `run.staging.public: false`.

  It can also pick its domain: `forge create <name> ... --domain forgegate.io`
  (in the dashboard's Visibility step: the Domain list) writes
  `run.domain: forgegate.io`. `forge domain list` shows the domains this
  Forge API offers, the default first; a domain it does not offer is refused
  before anything is created, and `--domain` with `--internal` is refused.

A writer can also set a service's address in Forge, where it wins over
`run.host` and `run.domain` (in the dashboard: Settings, Danger zone, Change
address). It is meant for the rare move, for example a service that takes
over a domain of its own:

```sh
forge service address deogratias --host @ --domain deogratias.cloud --dry-run
forge service address deogratias --host @ --domain deogratias.cloud --claim deogratias.cloud
forge service address deogratias --clear      # back to forge.yaml
```

`--host @` is the domain itself: production at `https://deogratias.cloud`,
staging at `https://staging.deogratias.cloud`. Forge makes no DNS record for
the root of a domain; the domain's owner points it at the cluster. An address
that already resolves is taken over only when named with `--claim`, and the
claim is kept in the service and the audit log. Like `run.host`, the setting
applies at the next deploy of each environment.

The deploy chart makes the DNS record with the deployment: a
`CloudflareRecordsApplication` (the platform's own kind; a Crossplane `Record`
underneath, a proxied CNAME to the cluster's Cloudflare tunnel). It is removed
with the service, and replaced when the service moves to another domain. The
tunnel sends the domains' hosts to Forge's Gateway, which has one listener
per domain (`public-<zone>`): it takes only hosts under that domain, and only
routes from the namespaces of Forge deployments. Previews keep their internal
addresses.

What to know before switching it on:

- **A service moves when it is next deployed or promoted**, not before, and
  the operation says so in an `address` step: the new address, that it is
  reachable from the internet, and the old address that stops working (a
  route has one host). A service that must stay internal needs
  `run.domain: none` first, and forge-api must already know the field: an
  older one rejects a `forge.yaml` that has it.
- **A name in use is refused** before anything is written: a new address must
  not resolve in DNS yet. If DNS cannot be asked, the deploy is refused too
  and can be run again. A service called `<something>-staging` gets no public
  address, because that is another service's staging address.
- **A rollback restores the commit, not the address.** The address is worked
  out again from the domain that commit asked for, so rolling back to a deploy
  from before public addresses does not take the service off the internet.
- **The chart revision must make DNS records** (`render.sh` checks it), or
  deployments would report success and be reachable nowhere.
- **Not checked:** whether the DNS record itself became ready. Argo CD has no
  health rule for the kind, so a deploy can finish a few seconds before the
  name resolves, and a record the platform could not create shows up only in
  the `CloudflareRecordsApplication`'s status.
- **Switching it off again** strands what is public: those services are
  refused on their next deploy until `run.domain` is removed or set to `none`.

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
(contents: write). The tag is the version: a deploy fails with
`release_failed` before it changes anything if the tag cannot be made, so run
it again. A GitHub release that cannot be made does not stop the deploy; the
operation says so and the next deploy of the commit makes it. `--bump` is for
production: a staging deploy refuses it.

The image a deploy or preview waits for and runs is
`<registry>/<owner>/<service>:<sha7>`. The registry is `ghcr.io` unless
forge-api runs with `--image-registry` (`FORGE_IMAGE_REGISTRY`, a host and an
optional path such as `myregistry.azurecr.io`; the platform chart's
`imageRegistry`). The pull secret (`--preview-pull-secret`) needs a login for
that registry: forge-api uses it to check that the image exists, and asks the
registry where to log in (its `WWW-Authenticate` challenge), so ghcr.io and an
Azure Container Registry both work. The login only goes to a token service in
the registry's own domain. Docker Hub does not work (`docker.io` redirects
image requests to its website).

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

Events live in Forge's PostgreSQL (`--database-url`), or in memory with
`--in-memory`.

### Cluster capacity

Admins can see how much room the cluster has left: its nodes, their
conditions, how much of their CPU and memory is in use (from metrics-server)
and, with Prometheus, their disks and every persistent volume, tied to its
Forge service when it has one. The status is the worst problem: a node
condition first, then CPU, memory, disk or a volume from 80% used.

```sh
forge cluster list
forge cluster get default
forge cluster get default -o json | jq '.nodes[] | select(.condition != "Ready")'
```

It is off unless forge-api runs with `--clusters` (`FORGE_CLUSTERS=1`) and may
list nodes, pods and `metrics.k8s.io` nodes; on k3s, `render.sh` adds that
role with `FORGE_CLUSTERS=1`. `--cluster-name` names the cluster (default
`default`) and `--grafana-url` adds a link to Grafana. Without metrics-server
the nodes are still shown, without usage figures. `--prometheus-url`
(`FORGE_PROMETHEUS_URL`) adds disks and volumes; forge-api then also lists
namespaces, to tie volumes to services.

Other clusters Argo CD knows can be shown too: `--remote-clusters`
(`FORGE_REMOTE_CLUSTERS`) names Argo CD cluster Secrets in namespace `argocd`,
comma-separated, and forge-api reads each cluster with the credential in its
Secret (a bearer token or client certificate over verified TLS; exec plugins,
cloud logins and `insecure` are refused). `render.sh` then adds a Role that
may `get` exactly those Secrets. A cluster that cannot be read is listed as
`Unavailable`, with the reason.

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
signatures and SBOMs, plus a `ghcr.io/showmino/forge-api` image and, from a
separate job, `deploy/argocd/chart` packaged and pushed as a Helm/OCI
artifact to `ghcr.io/showmino/forge-chart` at the same version (no leading
`v`).

Releases need a `HOMEBREW_TAP_GITHUB_TOKEN` secret with write access to
`showmino/homebrew-tap`.

## License

Apache License 2.0: see [LICENSE](LICENSE). To report a vulnerability, see [SECURITY.md](SECURITY.md).
