# Five ways in, one track: phases 9 to 13

Proposed on 2026-10-29. The owner chose the order and choices 1 to 4 below
the same day, and gave the go-ahead for phase 9. Each later phase still needs
its own go-ahead before code; choice 5 is open until phase 11.

## What

Today a service reaches Forge one way: a GitHub repo in the org, made from a
template (or registered), whose own CI pushes
`<registry>/<org>/<service>:<commit>`. Forge then deploys that image with its
service chart, to staging, then production.

These phases add four more ways in, and keep one track out. Whatever the
source, the result is an ordinary catalog service: staging, then production,
versions, rollback, previews where they make sense, policy on every change,
and every step in the audit log. Nothing gets a second deploy path.

| Way in | What Forge has today | What is missing | Phase |
| --- | --- | --- | --- |
| Simple deploy: run an image | The runtime part: the service chart, address, run options, promote and rollback | A service without a repo, running an image Forge did not build | 9 |
| Source deploy: build a repo | Repos that build in their own GitHub Actions; `forge init` and `service register` for existing repos | A repo with only a Dockerfile, and no CI, built and deployed by Forge | 10 |
| Helm chart | Forge deploys its own chart only | Someone else's chart, with values, per environment | 11 |
| App catalog: one click | The service catalog (your services) and templates (new code) | Ready-made apps (Grafana, Uptime Kuma, n8n, ...) installed with their database | 12 |
| Application builder | The Create wizard, service options, PostgreSQL resources | An app of several parts (web, worker, database), built and promoted together | 13 |

Order: 9 first, because 10, 11 and 12 reuse its "service without a repo";
12 needs 9 and 11; 13 is the largest and comes last.

## Common ground: a service without a repo

Phases 9, 11 and 12 make services that have no repo of their own. Their
`forge.yaml` lives in the deploy repo, at `services/<name>/forge.yaml`, next
to the environments' values that are already there. Forge writes it (from the
CLI, the dashboard or a catalog entry) with a commit, as it writes a deploy.
So:

- one format (`internal/manifest`), validated the same way;
- the history of every change is the deploy repo's git history, and the
  audit log has who asked;
- `forge service options set` and the dashboard's Settings change it the
  same way they change a repo's `forge.yaml`, minus the pull request.

A service has a repo, or it has none; `resolveSource` (forge-api) reads
`forge.yaml` from the one it has.

## Phase 9 · Image deploy

Run an image someone else built: `nginx:1.27`, `ghcr.io/acme/tool:2.3`.

- **CLI:** `forge create web --image nginx:1.27 --port 80 --team platform`
  makes the service (no repo). `forge deploy start web --image nginx:1.28`
  deploys a new tag; without `--image`, the tag in `forge.yaml`.
- **Digest, not tag:** a deploy resolves the tag to a digest and pins
  `image@sha256:...` in staging's values. Promote copies the digest, so
  production runs what staging ran even if the tag moved. The version is
  the tag (`1.28`), plus a Forge release candidate number as today.
- **Registries:** public images, and private ones the install's pull secret
  can read. More credentials per team: later.
- **Policy:** a new rule in `policy/` refuses `:latest` and images outside
  the allowed registries (`policy.allowedRegistries`, a chart value, empty
  means any).
- **Dashboard:** the Create page gets a first choice, "From a template" or
  "From an image"; the image path skips template and inputs.
- **Not in this phase:** deploying a new tag when one appears.

**Exit gate:** a writer runs a public image as a new service from the
dashboard, without a repo, promotes it to production, rolls it back, and the
audit log shows each step; `:latest` is refused with the rule's message.

## Phase 10 · Source deploy

A repo with a Dockerfile and no CI of its own gets built and deployed.

- **Connect a repo:** `forge service connect github.com/acme/tool` (and
  "Connect a repo" in the dashboard) registers the service, checks for a
  Dockerfile, and opens a pull request that adds `forge.yaml` and Forge's
  standard build workflow: build, push, `forge deploy start` with the
  workload identity of phase 4. Merging it is the first deploy.
- **Why a pull request and not builds in the cluster:** it reuses phase 4's
  CI path and its security model (a CI token deploys only its own service to
  staging), needs no build machines, and the build is visible in the repo.
- **No Dockerfile:** the pull request proposes one for the languages Forge
  detects (Go, Node, Python), from the templates' Dockerfiles. Cloud Native
  Buildpacks: later.

**Exit gate:** a repo with only a Dockerfile is connected from the
dashboard; after its pull request merges, it runs in staging without anyone
touching CI.

## Phase 11 · Helm charts

Install a published chart, with values, through the same track.

- **forge.yaml:** a service runs either an image (`run`) or a chart:
  ```yaml
  chart:
    repo: oci://ghcr.io/stefanprodan/charts   # or https://...
    name: podinfo
    version: 6.7.1
    values: {replicaCount: 2}
    expose: {service: podinfo, port: 9898}    # what gets the address
    environments:
      production:
        values: {replicaCount: 3}
  ```
- **Argo CD:** a second ApplicationSet, `forge-charts`, over the deploy
  repo's `services/*/<env>/chart.json`, with the chart as its source and the
  values from the deploy repo. Same AppProject, same namespaces.
- **Fenced in:** the AppProject allows namespaced resources only. A chart
  that brings a ClusterRole, a CRD or a webhook fails its sync, and Forge
  says which object and why. Forge adds the route to `expose`, the quota and
  the network policy around the chart, as for its own chart.
- **Promote and rollback:** promote copies the chart version and the shared
  values; each environment keeps its own `environments.<env>.values`.
- **Policy:** allowed chart repositories, like registries in phase 9.

**Exit gate:** podinfo is installed as a service, reachable at its address,
promoted, upgraded to a newer chart version and rolled back; a chart that
needs a ClusterRole is refused with a message that names it.

## Phase 12 · App catalog

Ready-made apps, installed with one command or a few clicks.

- **Entries** live in a Git repo versioned by tag, like templates: one
  folder per app with `app.yaml` (title, description, an image of phase 9 or
  a chart of phase 11, typed parameters as in `template.yaml`, the resources
  it needs, such as a PostgreSQL database) and its default values.
- **Install:** makes a service from the entry (no repo, phase 9's
  `forge.yaml` in the deploy repo), with its resources, and deploys it to
  staging. An installed app is a normal service from then on.
- **Upgrade:** a new catalog version of an installed app is offered on its
  page; upgrading is a deploy to staging, then a promote.
- **First entries:** three that show the kinds: an image without data
  (Uptime Kuma), an image with a database (n8n), a chart (Grafana).

**Exit gate:** a writer installs an app with its database from the
dashboard, opens it at its address, and later upgrades it to the next
catalog version through staging.

## Phase 13 · Application builder

An app made of several parts, put together in the dashboard.

- **Model:** an application groups services under one name and team: each
  part is a service of any kind above (a template, an image, a repo, a
  chart), and the database and other resources are the application's.
  Parts find each other by environment variables Forge sets
  (`<PART>_URL`).
- **Together:** the application deploys, promotes and rolls back as one;
  each part still has its own page, logs and history.
- **Dashboard:** a builder page that shows the parts and what connects them,
  adds a part from any source, and shows the result as the `forge.yaml`
  files it will write, as the Create page shows its command.

**Exit gate:** a writer builds a web part, a worker and a PostgreSQL
database in the dashboard, deploys them, and promotes the application to
production in one step.

## Owner's choices

1. **Order:** 9, 10, 11, 12, 13. Phase 9 may start (2026-10-29).
2. **Name of the app catalog:** "catalog" already means the services Forge
   knows, so phase 12 is "Apps": the dashboard page "App catalog" and
   `forge app list | get | install`.
3. **Where a repo-less service's `forge.yaml` lives:** in the deploy repo,
   at `services/<name>/forge.yaml`, as above (not a small repo per service).
4. **Source deploy builds:** a pull request that adds Forge's build
   workflow, as above (not builds in the cluster).
5. **Charts (open, decided before phase 11):** namespaced resources only, as
   proposed? Charts that need cluster-wide objects would stay something the
   platform team installs by hand.
