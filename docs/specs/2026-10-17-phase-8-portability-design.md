# Phase 8 — Portability design

## Intent and why

Forge is being prepared for open-sourcing, with no specific second organization
lined up yet. Today it runs for exactly one GitHub organization (`showmino`),
on exactly one secret backend (1Password), on exactly one cluster topology
(k3s + MetalLB + Istio's Gateway API + a Cloudflare tunnel). None of that
should have to be true for someone else to pick Forge up and run it for their
own org, on their own cluster, with their own secret backend — by changing
configuration, not code.

GitHub itself stays the one supported source-control/CI/identity provider for
v1. `docs/index.md`'s open questions already anticipated this moment: "the
production [identity] provider ... is deferred until Forge is offered as a
product" — this phase is that trigger point, deliberately scoped down to
"stay on GitHub, get everything else off `showmino` and off this one
cluster." A GitLab/Bitbucket/etc. provider is a door left open, not opened
here. ArgoCD stays the one GitOps engine (standing decision, unchanged).
Pluggable DNS providers (who creates the record for a public address) are
explicitly deferred to a later phase.

## Goals

- Any GitHub organization can run its own Forge instance by configuration
  alone — no literal `showmino` (or its App ID, teams, repo names) left as a
  compiled-in default anywhere in this repo (CLI, API, protos, chart,
  `deploy/k3s`), outside showmino's own `live.env` and release pipeline.
  `forge-web`'s own showmino assumptions are a separate repo and a separate
  follow-up (see non-goals).
- A service's `secrets:` entry in `forge.yaml` no longer assumes 1Password's
  vault/item addressing. The contract becomes backend-neutral; 1Password
  stays as Forge's own shipped implementation, and the seam for another
  backend is real and documented.
- Forge's render/chart can expose Dex, forge-api, and a deployed service —
  including a *public* address — on a cluster that has none of: MetalLB,
  Istio's Gateway API specifically, or a Cloudflare tunnel.
- Proven on an actual second cluster: a throwaway minikube instance on the
  owner's own server, standing in for "a cluster we don't control the
  topology of." Never the production k3s cluster.
- The deploy chart is an installable, versioned artifact (a Helm/OCI chart
  release alongside the CLI and image releases), not only a path inside this
  git repo pinned by commit SHA — so another org's ArgoCD can reference it
  the ordinary Helm way instead of pointing at `showmino/forge`'s own git
  history.

## Non-goals (this phase)

- A non-GitHub source-control/CI/identity provider (GitLab, Bitbucket,
  etc.). GitHub is the only one this phase builds or tests against.
- A pluggable DNS-provider layer. Stays Cloudflare-specific; later phase.
- TLS termination for a public address on a tunnel-free cluster. Assumed to
  be the platform team's own responsibility (their cloud LB's managed
  certificate, their own cert-manager) — Forge exposes a listener for them
  to sit behind, and issues nothing itself.
- Replacing ArgoCD, or adding a second GitOps engine. Unchanged.
- A second, production-grade secret-backend integration shipped in this
  phase (for example a maintained External Secrets Operator adapter). The
  *contract* becomes backend-neutral and 1Password keeps working as today;
  the minikube exit gate proves the seam with a stand-in backend, not a
  second fully-supported integration.
- `showmino/forge-web`'s own compiled-in showmino assumptions (its default
  API URL, branding/copy). forge-web is a separate repo and a separate
  concern; de-hardcoding it is a follow-up phase or issue, scoped later with
  the same recon approach as this one — not part of Phase 8.

## Anti-goals

- Breaking the showmino production rollout. Every change here is additive:
  a render from today's `live.env`, and every forge.yaml that exists today,
  must keep behaving exactly as they do now.
- Building a plugin marketplace. The point is removing assumptions that
  happen to be true only for showmino's org/cluster, not maximizing the
  number of backends Forge talks to.

## Constraints

- GitHub stays the identity/CI/SCM provider.
- ArgoCD stays the one GitOps engine.
- No change to the production k3s cluster without the owner's explicit OK,
  per `AGENTS.md`. This phase's own proof runs on a throwaway minikube
  instance on the owner's server; it never touches the k3s cluster. Shipping
  the resulting code to showmino's own production instance later follows the
  normal rollout procedure in `docs/agents/verifying.md`, as any phase's
  code eventually does — that rollout is not part of this phase's exit gate.

## Current state (from codebase recon, 2026-10-17)

Three research passes across the repo found:

- **Org identity is already mostly config-driven.** `internal/server/rbac.go`,
  `team_access.go`, `ci_auth.go`, `ci_scope.go` and `internal/github/*` all
  take org/team/App-ID as parameters; no runtime logic assumes exactly one
  org. What's hardcoded is a set of compile-time *defaults* —
  `cmd/forge-api/main.go` (`--github-org`, `--templates-repo`,
  `--deploy-repo`, `--cli-repo`, `--preview-pull-secret`, all defaulting to
  `showmino/...`), mirrored in `internal/server/server.go:53,87` and in
  `deploy/k3s/render.sh:113-114,133`. One real template-level hardcode:
  `deploy/k3s/deploys.yaml.tmpl` has showmino's GitOps URLs written directly
  into the template, unlike `forge.yaml.tmpl`, which already takes them as
  `${FORGE_...}` placeholders. The release pipeline
  (`.goreleaser.yaml`'s image repo, Homebrew tap, and the compiled-in default
  API URL `https://forge-api.showmino.com`) is showmino's own release
  config — a fork sets up its own, so this is left alone, not templatized.

- **Secrets are backend-neutral at the edges, 1Password-shaped at the
  core.** Forge's own infra secrets (GitHub App key, DB credentials) are
  already backend-agnostic: `cmd/forge-api/main.go` just reads a file path
  or env var, never the 1Password API directly, and `render.sh`'s own
  top-of-file comment already shows a plain `kubectl create secret generic`
  alternative. But a *deployed service's* `run.secrets:` entry hard-requires
  1Password's addressing: `internal/manifest/options.go`'s `Secret` struct,
  its `itemPath`/`vaultName` regexes and `Vault()`/`ItemName()` parsing all
  assume the literal shape `vaults/<vault>/items/<item>`;
  `internal/server/deploys.go`'s `secretsAllowed` enforces 1Password's
  vault-allow-list and item-title-vs-ID distinction; the chart's
  `templates/onepassword.yaml` emits one `OnePasswordItem` per secret; the
  AppProject's `namespaceResourceWhitelist`
  (`deploy/k3s/live/forge.yaml:487`) explicitly allow-lists that CRD. By
  contrast, `_helpers.tpl`'s `forge.env` helper only ever does a plain
  `secretKeyRef` by Secret name and key — already backend-agnostic, since it
  doesn't care how that Secret got populated.

- **Cluster topology is the deepest coupling.** `render.sh` has exactly two
  exposure modes today, both environment-specific: `--lb <ip> <ip>`
  (MetalLB's `metallb.universe.tf/loadBalancerIPs` annotation plus
  IP-derived `nip.io` hostnames) or a bare domain (k3s's bundled Traefik
  Ingress, `ingressClassName: traefik` literal in `ingress.yaml.tmpl`).
  Neither a generic cloud `LoadBalancer` (no MetalLB annotation) nor an
  `ingress-nginx`-style controller has a matching mode. The previews Gateway
  (`previews.yaml.tmpl:81`) hardcodes `gatewayClassName: istio`. CloudNativePG's
  storage class (`database.yaml.tmpl:15`, `longhorn-retain`) is a bare
  literal. Public addresses structurally require a Cloudflare tunnel as the
  actual packet path (`render.sh`, `deploy/argocd/chart/templates/dns.yaml`)
  — separate from, and in addition to, the question of who creates the DNS
  record. Previews/internal addresses need no tunnel at all and already work
  on any cluster with a Gateway/LB. `*.nip.io` and `NetworkPolicy` are
  already portable (the former is just render.sh's convenience string for
  IP-only exposure; the latter is a standard `NetworkPolicy`, not
  k3s-specific).

## Scope / milestones

### M1 — Org identity is configuration, not code

Mostly deletion, not design, per the recon above:

- Remove the `showmino`-flavored literal defaults from
  `cmd/forge-api/main.go` (`--github-org`, `--templates-repo`,
  `--deploy-repo`, `--cli-repo`, `--preview-pull-secret`) and the mirrored
  fallback in `internal/server/server.go:53,87`. These become required
  flags/env vars with no silent default (so a misconfigured instance fails
  loudly at start, not by quietly acting as showmino's).
- Fix `deploy/k3s/deploys.yaml.tmpl` to take the GitOps repo URLs as a render
  parameter, the way `forge.yaml.tmpl` already does.
- Leave the release pipeline (`.goreleaser.yaml`, the Homebrew tap, the
  compiled-in default API URL) as showmino's own — document it as something
  a fork replaces with its own, not something this phase templatizes.
- showmino's own `deploy/k3s/live.env` already supplies these explicitly, so
  it needs no change; it's the proof that the new required-flag shape still
  renders what's live today.
- **Exit bar:** non-regression only — showmino's existing `live.env` renders
  and behaves identically once the defaults become required flags. This
  milestone does not stand up a second GitHub org/App/team to prove a real
  second identity end-to-end; that's a materially bigger task than removing
  defaults, and isn't needed to show the config/code is no longer
  org-specific.

### M2 — Secrets become a backend-neutral contract

- Redesign the `secrets:` field's addressing in
  `internal/manifest/options.go` so it no longer hard-requires 1Password's
  `vaults/<vault>/items/<item>` shape: name a Secret and its keys, and leave
  *populating* that Secret to whatever operator the platform team runs.
- Update server-side validation
  (`internal/server/deploys.go`'s `secretsAllowed`, `SecretVaults` /
  `SecretDeniedItems`, and the CLI flags in `cmd/forge-api/deploys.go`) to
  the new, backend-neutral vocabulary.
- Chart: keep `templates/onepassword.yaml` as the shipped 1Password path.
  The new contract must make it possible to swap in a different template
  (for example an External Secrets Operator `ExternalSecret`) without
  touching `_helpers.tpl`'s env-wiring, which already only needs a Secret
  name and key.
- Update the AppProject's resource-whitelist pattern so a different
  backend's CRD can be allow-listed the way `onepassword.com/OnePasswordItem`
  is today.
- The backend is chosen once per running Forge instance (a chart/render
  value, like today's implicit "1Password is the only option"), not
  per-secret or per-service. This matches the existing model — `--secret-vaults`
  is already a per-instance allow-list, not something a single `secrets:`
  entry picks for itself — and avoids a multi-backend-per-install feature
  nobody asked for.
- Exit proof: on the minikube cluster, populate a deployed service's secret
  through something other than the 1Password operator (a plain Kubernetes
  Secret created by hand is enough to prove the seam), and confirm the
  service reads it exactly the way a 1Password-backed one is read today.

### M3 — Cluster portability

- Add exposure mode(s) to `render.sh`/the chart for Dex and forge-api that
  don't assume MetalLB's annotation: a plain `LoadBalancer` Service
  (cloud-assigned IP) and/or an Ingress mode for a non-Traefik controller.
- Parameterize the previews Gateway's `gatewayClassName`
  (`previews.yaml.tmpl:81`) instead of the literal `istio`.
- Parameterize CloudNativePG's `storageClass`
  (`database.yaml.tmpl:15`) instead of the literal `longhorn-retain`.
- Add a public-address path that doesn't require a Cloudflare tunnel: the
  Gateway/Service takes a real public address directly from whatever the
  platform team's own cluster provides. DNS is pointed at it by hand (DNS
  provider abstraction is out of scope, per non-goals); TLS is terminated in
  front of Forge by the platform team (their cloud LB's cert, their own
  cert-manager), never by Forge.
- Exit proof: stand up minikube on the owner's server (the owner has
  already OK'd this, as a throwaway instance separate from the production
  k3s cluster), render Forge for it with none of MetalLB, Istio, or the
  tunnel configured, and run the golden path end to end — create a service,
  deploy it, reach it at an address exposed the new way. This proves the
  *mechanism* (a Gateway/Service gets a real address without the tunnel, and
  the service answers over it) — it does not require real internet-routable
  DNS or a certificate, since actual public reachability is explicitly the
  platform team's own concern (see non-goals).
- **Rollout:** each milestone ships independently, same as any other phase
  — merged, tested, then rolled out to showmino's own production instance
  with the owner's OK when it's ready, following the normal procedure in
  `docs/agents/verifying.md`. M3 in particular needs no change to what's
  live on k3s at all: its new exposure modes are additive and only engaged
  when rendering for a cluster that asks for them: showmino's own render
  keeps using MetalLB/Istio/the tunnel exactly as today.

### M4 — Helm/OCI chart release

- Package `deploy/argocd/chart` and push it as a versioned OCI artifact to
  `ghcr.io` on every `vX.Y.Z` tag, alongside the existing CLI archives and
  the `forge-api` image, as one more step of `.github/workflows/release.yml`.
  The chart's own `Chart.yaml` `version` is set at package time to the
  release's version (Helm chart versions are plain SemVer, no leading `v`;
  the OCI tag ends up `0.20.0` where the image tag is `v0.20.0` — a Helm/OCI
  convention, not a Forge inconsistency to paper over).
- showmino's own ArgoCD `ApplicationSet` (`deploy/k3s/deploys.yaml.tmpl`)
  switches its chart source from the git-commit pin it uses today
  (`repoURL`/`path`/`targetRevision: <40-char SHA>`) to the same OCI
  reference any other adopter would use (`repoURL: oci://ghcr.io/...`,
  `targetRevision: <chart semver>`) — one supported path, proven by
  showmino's own production use, per the owner's decision (2026-10-17).
  `FORGE_DEPLOY_CHART_REVISION` becomes a chart version instead of a commit
  SHA; `render.sh`'s existing guard that the chart at that revision actually
  has `templates/dns.yaml` before enabling `FORGE_PUBLIC_DOMAINS`
  (today a local `git cat-file`) is re-pointed at the published chart
  instead of this repo's git history.
- Exit proof: a real `vX.Y.Z` tag push publishes the chart to `ghcr.io`
  (checked with `helm pull`/`helm show chart` against the OCI reference,
  no cluster involved), and a rollout of showmino's own instance onto that
  chart version — through the normal procedure in
  `docs/agents/verifying.md`, with the owner's OK — shows no drift.

## What's left to the builder

- Exact flag/env names for the new exposure and secret-contract shapes.
- Which Gateway API implementation (or plain Ingress) the minikube exit-gate
  test uses to stand in for "a cluster we don't control the topology of."
- Whether the new secrets syntax needs an automatic migration from today's
  `vaults/...` strings, or existing services update their forge.yaml by
  hand (either way, already-deployed services must keep working against the
  old syntax without edits, or get a clear, one-time migration path — not a
  silent breakage).
- Exact wording of the new required-flag error messages that replace
  today's silent showmino defaults.
