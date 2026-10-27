# showmino's Forge on the chart

## Goal

showmino's own install moves from `deploy/k3s/render.sh` (rendered in
showmino/forge-ops, applied by hand) to the public `forge` chart, installed
like every other application in showmino/kubernetes: an
Argo CD Application of the chart with its values inline, synced
automatically, as on the AKS test install. Like there, Forge is on one host:
`forge.showmino.com` serves the dashboard (`/`), the API and the login
(`/dex`). The chart then serves both the open installs and showmino's, and
`render.sh` can be retired.

## Decided with the owner (2026-10-27)

| Question | Answer |
| --- | --- |
| Approach | The chart learns what showmino's install uses first; then the install switches to it in one step |
| Approval of a rollout | Merging the pull request in showmino/kubernetes (the owner merges there). Argo CD then syncs by itself; the manual Sync goes |
| Data | The existing CloudNativePG cluster `forge-db` is kept: the chart takes it over in place, nothing is copied |
| Addresses | One host, `forge.showmino.com`, with the chart's dashboard (the `forge-web` service that serves it today is retired). `forge-api.showmino.com` keeps answering for the API, so CLI contexts and services' CI (`FORGE_API_URL`) keep working. `forge-login.showmino.com` is retired: the GitHub OAuth app's callback becomes `https://forge.showmino.com/dex/callback`, changed by the owner at the switch |

## What the chart lacks today

Found by comparing the live render with `helm template` of the chart. The
names of the main objects already agree (`forge-api`, `dex`, `forge-db`,
`forge-db-dex`, `forge-previews`, `forge-services`), so Argo CD can adopt
them.

| Feature | render.sh | Chart change |
| --- | --- | --- |
| Resources (databases for services) and the runner | `FORGE_RESOURCES`, `FORGE_RUNNER_IMAGE`; the `forge-runner` namespace, its RBAC, admission policy, runner Service, NetworkPolicy | `resources.enabled`, `resources.runnerImage`, the same objects |
| Public addresses | `FORGE_PUBLIC_DOMAINS`, `FORGE_PUBLIC_DNS_TUNNEL`; one public listener per domain on the previews Gateway; the DNS record kind in the services' AppProject | `apps.publicDomains`, `apps.dnsTunnel`, the listeners and the kind |
| Other clusters to deploy to | `FORGE_REMOTE_CLUSTERS`; a Role to read Argo CD's cluster Secrets | `clusters.remote`, the Role |
| Who may write | `FORGE_WRITER_GROUPS` | `github.writerTeams` beside `adminTeams` |
| More kinds a service may deploy | CronJob, ScaledObject in the AppProject | `deploys.extraKinds` |
| The API also on a load balancer IP (MetalLB), for the old API name | the API's Service of type LoadBalancer with a fixed IP, which the Cloudflare tunnel routes `forge-api.showmino.com` to | `api.service` (`type`, `annotations`), beside the one host's routes |
| The login's GitHub client in a Secret of its own | a second 1Password item | `login.existingSecret` (default: `github.existingSecret`) |
| Pinned versions | `FORGE_API_IMAGE`, `FORGE_DEPLOY_CHART_REVISION` | `api.image.tag`, `deploys.chartRevision` (exist) |

Each change is off or empty by default, renders exactly as before when
unset, and gets a case in `make platform-chart-test`. The user docs
(forgegate's chart values page) get each new value.

Not in the chart: the database's backups to an object store and their
alerts (ObjectStore, ScheduledBackup, PodMonitor, PrometheusRule) and the
1Password items. They depend on the cluster's operators and stores, so they
stay plain manifests next to the app in showmino/kubernetes, as other apps
there do. The chart's `database.cloudnativepg.plugins` already connects the
backups.

## Routing the one host

`forge.showmino.com` is served today by the `forge-web` service through the
previews Gateway's public listener for `showmino.com` (the tunnel's wildcard
route), which only accepts routes from namespaces labelled as Forge
deployments. The chart's HTTPRoutes attach to that Gateway with
`gateway.parentRefs`; the listener (or the namespace `forge`) is made to
accept them. A route for one exact host wins over the wildcard, so the switch
moves the address from `forge-web` to the chart without a DNS change.

## Order

1. Chart pull requests in this repo, released.
2. A comparison of `helm template` (with showmino's values) and the live
   render, object by object. Every difference is either intended (labels,
   renamed cluster roles, the one host's routes) or fixed in the chart. Then
   a server dry run of the chart's render against the cluster.
3. One pull request in showmino/kubernetes that rewrites
   `gitops/apps/forge.yaml`, as the AKS install's `apps/forge.yaml`: the
   source becomes `oci://ghcr.io/showmino/charts/forge` at a fixed version
   with showmino's values in `helm.valuesObject`, plus a second source,
   `apps/forge/production/` in the same repo, for the extra manifests
   (1Password items, backups, alerts); sync becomes automated (prune,
   self-heal). The Application keeps its name, so it keeps its objects;
   anything the old render had and the chart does not is pruned, which is why
   step 2 must be clean. The switch keeps the live forge-api image and deploy
   chart revision; upgrades are later pull requests, one change at a time.
   The values live in that file, not in forge-ops.
4. The owner changes the GitHub OAuth app's callback URL and merges: that is
   the rollout. Then the checks below.
5. Afterwards: the `forge-web` service (staging and production) is deleted
   with `forge service delete` (the owner's OK), `live.env` and `render-live.sh` leave forge-ops, `verifying.md`
   and AGENTS.md describe the new rollout, and `deploy/k3s` is removed here
   (deleting needs the owner's OK).

## Checks after the switch

Application `forge` Synced and Healthy with no manual step; `kubectl diff`
against the chart's render is empty; the API, login and dashboard answer at
`forge.showmino.com` and `forge-api.showmino.com`; a sign-in works in the
browser and with `forge auth login`; a service deploys to staging and promotes;
a resource plan runs; a public address resolves; the remote cluster is
listed; the next nightly backup completes.

## Risks

- Prune: objects missing from the chart would be deleted on merge. Step 2's
  comparison and the dry run are there for this.
- The database: if the chart's Cluster spec differs from the live one,
  CloudNativePG rolls its pods (a short failover). The comparison shows it
  before the merge.
- The login's address changes: until the GitHub callback matches, every new
  sign-in fails (AGENTS.md, "Mistakes"). Existing Forge sessions (`fgs_…`)
  are kept by forge-api, not by Dex, so signed-in CLIs should keep working;
  checked during the switch.
- `forge-staging.showmino.com` (the dashboard's staging) goes with the
  `forge-web` service; the chart has one dashboard.
- Two Applications on the same objects: the old one is removed in the same
  pull request that adds the new one.
