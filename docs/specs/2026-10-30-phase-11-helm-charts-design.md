# Phase 11 · Helm charts, as built

Designed in `2026-10-29-deploy-sources-design.md` (phase 11). The owner gave
the go-ahead on 2026-10-30 and answered choice 5: charts make namespaced
resources only. This is what was built.

## What

A service without a repo can run a published Helm chart instead of an image:

```sh
forge create podinfo --chart podinfo --chart-repo oci://ghcr.io/stefanprodan/charts \
  --chart-version 6.7.1 --expose podinfo:9898 --values podinfo.yaml --team web
```

```yaml
# services/podinfo/forge.yaml in the deploy repo
chart:
  repo: oci://ghcr.io/stefanprodan/charts   # or https://<host>/<path>
  name: podinfo
  version: 6.7.1                            # one exact version
  values: {replicaCount: 2}
  expose: {service: podinfo, port: 9898}    # what gets the address
  environments:
    production:
      values: {replicaCount: 3}
run:
  domain: example.com                       # only the address applies
```

With `chart`, only the address of `run` applies (`domain`, `host`,
`staging`); every other run option, and `resources`, is refused: the chart's
values decide how it runs, and a Forge database cannot reach its pods.

## How it reaches the cluster

Each environment's directory in the deploy repo holds three files:

| File | Written | Read by |
| --- | --- | --- |
| `values.yaml` | first, by every deploy, promote and rollback | `forge-services` (as today): Forge's own chart. With `chart` set it renders no Deployment or Service, only the route to `chart.expose`, the quota, the DNS record, and the chart's AppProject `forge-chart-<service>-<env>` in `argocd`. It is also the deploy's record, which a rollback restores. |
| `chart.json` | once the namespace exists | the new `forge-charts` ApplicationSet: repoURL, chart, path, version |
| `chart-values.yaml` | with `chart.json`, in one commit | the chart's application, as its values file: the environment's values over the shared ones |

`forge-charts` makes `<service>-<env>-chart` in the project
`forge-chart-<service>-<env>`, release name `<service>`, into the namespace
`<service>-<env>`. Before writing `chart.json`, forge-api labels the namespace
`pod-security.kubernetes.io/enforce: baseline` (and `warn: restricted`), so
Kubernetes refuses privileged pods, host networking and host paths.

### Why a project per service

A shared project would allow every `*-staging` and `*-production` namespace,
so a chart (or a value like `namespaceOverride`) could write into another
team's service. The project made for each service allows its namespace only,
no cluster-scoped resources, and none of: Gateway API objects, Ingresses,
ResourceQuota, LimitRange, kro objects (DNS records), 1Password and External
Secrets objects, Argo CD objects. To make these projects, `forge-services`
may now create AppProjects in `argocd` (only with `deploys.charts.enabled`);
their content is Forge's chart's, not the deploy repo's.

A chart that needs more fails: forge-api reads Argo CD's refusal ("resource
...:ClusterRole is not permitted", "namespace ... is not permitted") and
fails the deploy with `chart_not_allowed`, naming the object, and what to do
(turn it off in the values, or have the platform team install the chart).

## The operations

- **Deploy:** `values.yaml`, wait for the namespace, label it, then
  `chart.json` and `chart-values.yaml`, then wait for both applications.
  Steps: resolve, release, commit, namespace, chart, address, sync. The
  version is the chart's version; there is no repo to tag. `--ref` deploys
  another version and `--values` (StartDeployRequest `chart_values`) new
  shared values; both go into `forge.yaml` once the deploy has succeeded.
- **Promote:** staging's version and shared values; production's own values
  from `forge.yaml` as it is now. The chart rules are checked again.
- **Rollback:** the old `values.yaml` holds the whole chart record; the two
  chart files are written from it again.
- **Status:** a deployment's sync and health are the worse of its two
  applications, in `forge deploy status`, the dashboard and the promotion rule.
- **Delete:** the chart files go first and forge-api waits for the chart's
  application to be gone; then the rest, as for any service.
- **Previews:** refused, as for an image service (no branch).

## Settings

forge-api `--charts` (`FORGE_CHARTS`) and `--allowed-chart-repos`
(`FORGE_ALLOWED_CHART_REPOS`); the chart's `deploys.charts.enabled` and
`deploys.charts.allowedRepos`, which also become the projects' sourceRepos.
`policy/charts.rego` refuses repositories outside the list and repositories on
an address or a one-word host unless listed. An OCI chart is looked up with the
image registry client (`<repo>/<name>:<version>`, `+` as `_`); an HTTPS one in
its `index.yaml`.

## Not checked on a cluster

Built and tested against the fakes (`envtest.Argo` makes the chart
applications and can refuse one). Not run against a real Argo CD: the files
generator's parameters, an HTTPS chart with an empty `path`, the AppProject
made from another application, and Argo CD keeping the pod-security labels
next to its managed namespace metadata need the exit gate on the cluster.

## Exit gate

podinfo is installed as a service, reachable at its address, promoted,
upgraded to a newer chart version and rolled back; a chart that needs a
ClusterRole is refused with a message that names it.
