# Open Forge: one Helm chart, public artifacts

Status: proposed, 2026-10-25. Decisions below are the owner's (2026-10-25).

## Goal

Anyone with a Kubernetes cluster and a GitHub organization installs Forge
with one `helm install`: the API, the dashboard, the login and a database.
Besides the chart they need two repos in their org: one for templates (or
showmino's public `forge-templates` itself) and an empty `forge-deploys`.
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
| License | Apache-2.0, for `forge`, `forge-web`, `forge-templates` |
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

`forge-templates` (public) builds on `ubuntu-latest` and pushes to
`ghcr.io/<owner>/<service>` with `GITHUB_TOKEN`; the deploy step is
`forge deploy` with the Actions OIDC token, as today. A template parameter
keeps self-hosted runners and another registry possible (what showmino's
k3s and the AKS install use). GitHub-hosted runners must reach the API, so
the API must be on the internet for CI deploys; documented.

## Opening the repos

Before anything becomes public:

1. A secret scan of the full history of `forge`, `forge-web`,
   `forge-templates` (gitleaks), with the findings shown to the owner. A
   secret found in history is rotated, not only removed.
2. `LICENSE` (Apache-2.0) and a short `CONTRIBUTING.md`/`SECURITY.md`.
3. What stays visible and is not secret: `deploy/k3s/live.env` (LAN
   addresses, 1Password item names), `docs/agents/`. The owner decides
   whether they stay.
4. The owner switches the repos and packages to public (GitHub settings).

## Order

1. Scan and licenses (no change to anything running).
2. forge-web: runtime API address and same-origin by default; image tagged
   with the Forge version.
3. The chart: dashboard, one-host routing, ingress/gateway, bundled Postgres,
   `existingSecret`; published in the release workflow; chart tests in CI.
4. Templates: GitHub-hosted runners and ghcr.io by default.
5. Owner: repos and packages public.
6. techdocs: "Install Forge" rewritten around `helm install`.
7. Exit gate.

showmino's own k3s install keeps `deploy/k3s` for now; moving it onto the
chart is a later step.

## Exit gate

On a cluster that has only an ingress controller (the AKS cluster, after the
`deogratias-forge` install is removed, with the owner's OK):

1. `helm install forge oci://ghcr.io/showmino/charts/forge` with the three
   values above and one Secret. The dashboard loads at the host and signs in;
   `forge auth login` works; someone outside the org is refused.
2. `forge create hello` from `showmino/forge-templates` makes a repo whose CI
   runs on GitHub-hosted runners and pushes to ghcr.io.
3. With Argo CD installed and `deploys.enabled`, that push deploys to staging
   with no human action, and promote reaches production.
4. Nothing in the install came from showmino beyond the public chart,
   images, CLI and templates.

## Not in this

TLS issuance (the user's cert-manager or ingress does it), Homebrew, other
Git hosts than GitHub, multi-cluster deploys.
