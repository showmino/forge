# A second Forge on AKS, stood up from the docs

Status: proposed, 2026-10-25.

## Why

`docs/forge/17-running-your-own-forge.md` in techdocs and the platform chart
(`deploy/platform/chart`) say that someone outside `showmino` can run their own
Forge. Nobody has done it yet. This work does it on a fresh GitHub
organization and an empty AKS cluster, following the docs the way a newcomer
would. Every place the docs or the chart fall short gets written down and fixed.
**The list of gaps is as much the result as the running Forge.**

## Goals

- A working Forge for the GitHub organization `deogratias-forge` (owner
  `deogratias-saidi`, free plan, created 2026-10-25) on the AKS cluster
  `aks-mino-cluster-sdc-production` in resource group
  `rg-kubernetes-sdc-production` (a test subscription despite the name).
- The whole platform on the cluster managed by Argo CD from a config repo in
  the new org, with secrets in Azure Key Vault.
- The dashboard deployed the Forge way: its own repo, its own CI, deployed by
  the new Forge.
- Exit gate: see below.
- Fixes to the techdocs page and to the platform chart for each gap found.
- Separately: `aks-production` removed from showmino's Argo CD and from
  showmino's Forge.

## Non-goals

- TLS and real DNS names. People reach it over plain HTTP at `<name>.<ip>.nip.io`.
- Resources (`forge resource`, the OpenTofu runner). The platform chart leaves
  them out, and so does this work.
- Running Forge on a personal GitHub account (it needs an organization: teams,
  membership checks, the App installation).
- Making showmino's images or repos public.

## Anti-goals

- Quietly using showmino knowledge the docs don't give. If a step needed it,
  that's a gap: write it down.
- Touching showmino's production cluster beyond removing `aks-production`,
  which needs the owner's OK at the time of the change, as always.
- Docker-in-Docker or privileged build pods on the cluster.

## What is decided

| Decision | Choice | By |
| --- | --- | --- |
| GitHub | the new org `deogratias-forge` | owner |
| Addresses | the cluster's Azure public IPs with `nip.io`, HTTP | owner |
| Images | Azure Container Registry `acrforgesdcproduction` (Basic, Sweden Central, attached to AKS: pulls need no secret). Holds copies of showmino's `forge-api:v0.26.0` and `forge-chart:0.26.0` (done 2026-10-25) | owner |
| Dashboard | the Forge way: `deogratias-forge/forge-web`, a copy of `showmino/forge-web`, built by its CI with the new API's address | owner |
| CI | GitHub's Actions Runner Controller on the AKS cluster, runners for `deogratias-forge` answering to `self-hosted` (what the templates generate) | owner |
| Image builds | a rootless BuildKit service on the cluster; runners send builds to it, and it pushes to the registry. (`az acr build` is refused on this subscription: `TasksOperationsNotAllowed`) | owner |
| Config repo | `deogratias-forge/platform`: an Argo CD app-of-apps, one folder per component with its Helm values | builder's call |
| Other repos | `forge-templates` (a copy of showmino's at its latest tag, CI moved to BuildKit and the registry), `forge-deploys` (empty; forge-api writes it). All private | builder's call |
| Secrets | Key Vault (RBAC mode) in the same resource group; External Secrets Operator reads it with a user-assigned managed identity through workload identity. Holds the GitHub App key and client secret, and Grafana's admin password. No secret goes in Git | owner asked for Key Vault; the operator is the builder's call |
| Registry push from CI | runner pods get a managed identity with `AcrPush` through workload identity, log in to the registry with it, and hand the credential to BuildKit for the push | builder's call |
| Gateway | Envoy Gateway (the one the portability proof used) | builder's call |
| Database | CloudNativePG, managed by the platform chart; nightly backups and WAL to an Azure storage account through workload identity, as showmino does | builder's call |
| Monitoring | kube-prometheus-stack, so `forge cluster` shows disk and volumes and links to Grafana | builder's call |
| Writer and admin team | a team `platform-engineering` in the new org, with `deogratias-saidi` in it | builder's call |
| Stable IPs | static Azure public IPs, so the `nip.io` names survive reinstalls | builder's call |
| What faces the internet | only the Gateway (services), Dex and forge-api. Argo CD and Grafana are reached with `kubectl port-forward` | builder's call |
| Argo CD and the chart | Argo CD pulls the deploy chart from the registry with its own workload identity, not a stored password | builder's call |
| Previews | on, since the docs bring them up together with deploys; the pull secret is a placeholder, because the registry needs none | builder's call |
| CLI | built on this machine from `showmino/forge` at `v0.26.0` (a newcomer can't download it: the releases are in a private repo, which is a gap) | builder's call |

## Steps the owner does in the browser

GitHub only offers these in the browser:

1. Create the GitHub App in `deogratias-forge`, with the permissions the docs
   list plus "Self-hosted runners: read and write" (for the runner
   controller), and the Dex callback URL once the IP is known. Install it on
   all of the org's repositories. Create a private key and a client secret
   (they go into Key Vault, never into chat).
2. Sign in to the new dashboard and the CLI at the exit gate.

## Order

1. Azure: Key Vault, managed identities with federated credentials, storage
   account, static public IPs (`az`).
2. GitHub: the org's repos, the team, the org variable `FORGE_API_URL`; the
   owner's App (above).
3. Cluster: Argo CD by hand, then everything else from `platform` through it.
   Forge comes from the platform chart, pointed at the registry.
4. Runners and BuildKit; a test workflow that builds and pushes an image.
5. `forge-templates` and `forge-web` copied and adapted; forge-web registered
   and deployed.
6. Exit gate.
7. The docs and chart fixes as pull requests (forge, techdocs), each gap with
   what a newcomer hit.

Separately, at any point: a pull request in `showmino/kubernetes` removing
`gitops/clusters/aks-production` and its destination in
`gitops/projects/platform.yaml` (the owner merges), and a forge rollout that
drops `FORGE_REMOTE_CLUSTERS=aks-production` from `deploy/k3s/live.env`. Both
need the owner's OK at the time, like any production change. The rollout
can't happen after the merge, or `forge cluster list` fails to reach the
cluster in between: roll out first, then merge.

## Exit gate

1. `forge auth login` against the new Forge as `deogratias-saidi` works; a
   GitHub user outside the org is refused.
2. `forge create hello-forge --template go-grpc --team platform-engineering`
   makes a repo in `deogratias-forge`. Its first push runs CI on the AKS
   runners, builds with BuildKit, pushes to the registry and deploys to
   staging through Argo CD with no human action. The staging address answers
   `/healthz`.
3. `forge deploy promote` makes production answer too.
4. The dashboard (`forge-web`, deployed by the new Forge) loads and signs in.
5. `forge cluster list` shows the AKS cluster with disk and volumes.
6. The gaps are listed and each has a fix in a pull request.

## Gaps known so far

- The docs give a newcomer no way to get the images, the deploy chart or the
  CLI: all live in private showmino repos and packages.
- The dashboard isn't part of the platform chart, and the docs don't say how
  to get it. Its image has the API address baked in at build time
  (`NEXT_PUBLIC_FORGE_API_URL`).
- The templates' CI asks for `runs-on: self-hosted` and builds with Docker; the
  docs mention neither runners nor how images get built and pushed.
- The platform chart has no settings for cluster capacity (`FORGE_CLUSTERS`,
  Prometheus, Grafana).
- Secrets: the docs cover only `kubectl create secret` and 1Password.

## Left to the builder

The versions of each component, resource requests on two small nodes (the
pool can scale to 5), the Key Vault and identity names, and how the runner
image gets the registry login tool and the BuildKit client.
