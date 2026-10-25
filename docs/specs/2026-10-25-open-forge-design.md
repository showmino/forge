# Open Forge: one Helm chart, public artifacts

Status: proposed, 2026-10-25. Decisions below are the owner's (2026-10-25).

## Goal

Anyone with a Kubernetes cluster and a GitHub organization installs Forge
with one `helm install`: the API, the dashboard, the login and a database.
Besides the chart they need two repos in their org: their own templates repo
and an empty `forge-deploys`.
No runners, no BuildKit, no Key Vault, no 1Password, no registry of their
own, no mirroring.

The AKS install (`docs/specs/2026-10-25-own-forge-on-aks-design.md`) showed
what is in the way today: private images, chart and CLI; the dashboard
outside the chart with its API address baked in at build time; CloudNativePG
and 1Password assumed; templates that need self-hosted runners; a chart
credential for Argo CD.

## Decided with the owner

| Question | Choice |
| --- | --- |
| License | Apache-2.0, for `forge` and `forge-web` |
| Where | The showmino repos become public; `ghcr.io/showmino` packages public. Chart at `oci://ghcr.io/showmino/charts/forge`, CLI in GitHub Releases |
| Database | Bundled Postgres (one StatefulSet, no operator) by default, or an external URL |
| Prerequisites | Only an ingress controller or a Gateway. Argo CD only when deploys are on; previews off by default |
| Services' CI | GitHub-hosted runners, images pushed to `ghcr.io/<org>` with `GITHUB_TOKEN` |
| Secrets | Plain Kubernetes Secrets the user creates (`existingSecret`); 1Password and External Secrets documented as ways to fill them |

## The chart: `forge`

`deploy/platform/chart` becomes the chart `forge`, published with every
release (`vX.Y.Z` tag) next to the images. What `values.yaml` must have, and
nothing else is required:

```yaml
github:
  org: my-org
  appID: "123456"
  existingSecret: forge-github   # keys: app-private-key, client-id, client-secret
host: forge.example.com          # one host for everything
ingress: {className: nginx, tls: {secretName: forge-tls}}   # or gateway: {parentRef: ...}
```

- **One host, by path.** `/` the dashboard, `/forge.v1.*` and `/cli/` the
  API, `/dex` the login. One name, one certificate; the dashboard calls its
  own origin, so no CORS and no build-time address. Separate hosts stay
  possible (`api.host`, `login.host`).
- **Exposure:** `ingress` (any controller; TLS from a Secret or cert-manager
  annotations) or `gateway` (HTTPRoutes on a Gateway the user names). The
  `lb`/`cloudLB` modes and the bundled Gateway go; the previews Gateway stays
  with previews.
- **Dashboard:** `forge-web` in the chart, image
  `ghcr.io/showmino/forge-web:<version>`, the same version as the API. It
  reads its API address at runtime (same origin by default), so one image
  serves every Forge. The chart's version pins API, dashboard and deploy
  chart together.
- **Database:** `database.mode: bundled` (a Postgres StatefulSet with a
  volume, password generated once into a Secret), `external` (a Secret with
  `uri`), or `cloudnativepg` (today's, for backups).
- **Secrets:** run.secrets (`forge.yaml` `run.secrets`) is off unless a
  backend is configured; `onepassword` stays an option.
- **Deploys** (`deploys.enabled`): needs Argo CD in the cluster; the chart
  adds the AppProject and ApplicationSet. The deploy chart is public, so Argo
  CD needs no registry credential, only read access to `forge-deploys` (the
  GitHub App).
- **Previews, cluster capacity, resources:** off by default, as today's
  settings.
- `helm test` checks the API answers and the login's discovery document.

## Templates

Each organization writes its own templates (the owner, 2026-10-25): Forge
ships no template repo, and `showmino/forge-templates` stays showmino's own,
private. What Forge gives instead:

- the template format (`template.yaml`, the skeleton, parameters) documented
  in techdocs, with `forge template lint` to check one;
- one small example template in the docs, whose CI builds on GitHub-hosted
  runners, pushes to `ghcr.io/<owner>/<service>` with `GITHUB_TOKEN` and
  deploys with `forge deploy` and the Actions OIDC token;
- a note that GitHub-hosted runners must reach the API, so CI deploys need
  the API on the internet.

## Opening the repos

Done 2026-10-25: `forge` and `forge-web` are public, Apache-2.0.

1. Secret scan of the full history (gitleaks): `forge` 13 hits, all test
   fixtures, 1Password item paths and a file path; `forge-web` none.
2. `LICENSE` and `SECURITY.md`.
3. CI and releases on GitHub-hosted runners (forge#151, forge-web#70): a
   public repo's pull requests from forks must never run on the self-hosted
   runners in the owner's cluster.
4. `deploy/k3s/live.env`, `deploy/k3s/live/` and `docs/agents/` move to a
   private repo, `showmino/forge-ops` (the owner: they stay local). They are
   in the public history already; they hold no secrets, and the history is not
   rewritten. Argo CD's `forge` app is pointed at the new repo first
   (showmino/kubernetes, owner merges), then they leave `forge`.
5. Still to do by the owner: the packages (`forge-api`, `forge-web`,
   `forge-chart`) public, and private vulnerability reporting switched on.

## Order

1. Scan, licenses, CI on hosted runners, `forge-ops` (above).
2. forge-web: runtime API address and same-origin by default; image tagged
   with the Forge version.
3. The chart: dashboard, one-host routing, ingress/gateway, bundled Postgres,
   `existingSecret`; published in the release workflow; chart tests in CI.
4. techdocs: "Install Forge" rewritten around `helm install`, and "Write
   your templates" with the example.
5. Exit gate.

showmino's own k3s install keeps `deploy/k3s` for now; moving it onto the
chart is a later step.

### The chart as built

- `deploy/platform/chart`, name `forge`; `make platform-chart-test` checks
  three cases against goldens in CI. A release pushes it next to the deploy
  chart.
- The API's paths on the shared host are one per ConnectRPC service
  (`forge.apiPaths`): an Ingress or HTTPRoute prefix matches whole path
  segments. `TestChartRoutesEveryService` fails when a service is missing.
- The dashboard gets `FORGE_API_URL` = the public API address: the browser
  calls it and the page's CSP names it.
- The bundled database's password is made once (Helm `lookup`); a tool that
  renders without the cluster (Argo CD) names a Secret of its own
  (`database.bundled.existingSecret`).
- Deploys still need previews (the Gateway the services are reached
  through) and the namespace `forge` (the deploy chart's routes name
  `forge/forge-previews`). Lifting both is the open part of the exit gate's
  step 3.
- The dashboard image is pinned by the chart (`web.image.tag`): forge-web
  keeps its own versions.

## Exit gate

On a cluster that has only an ingress controller (the AKS cluster, after the
`deogratias-forge` install is removed, with the owner's OK):

1. `helm install forge oci://ghcr.io/showmino/charts/forge` with the three
   values above and one Secret. The dashboard loads at the host and signs in;
   `forge auth login` works; someone outside the org is refused.
2. A templates repo of the tester's own, with the docs' example template;
   `forge create hello` makes a repo whose CI runs on GitHub-hosted runners
   and pushes to ghcr.io.
3. With Argo CD installed and `deploys.enabled`, that push deploys to staging
   with no human action, and promote reaches production.
4. Nothing in the install came from showmino beyond the public chart,
   images, CLI and docs.

## Not in this

TLS issuance (the user's cert-manager or ingress does it), Homebrew, other
Git hosts than GitHub, multi-cluster deploys.
