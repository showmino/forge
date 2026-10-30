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

### What a chart may do

Each chart service has an AppProject of its own, `forge-chart-<service>-<env>`,
made by Forge's chart in `forge-services`:

- **One namespace:** its only destination is `<service>-<env>`. A shared
  project would allow every `*-staging` and `*-production` namespace, so a
  chart (or a value like `namespaceOverride`) could write into another team's
  service.
- **An allow list of kinds:** ConfigMap, Secret, Service, ServiceAccount,
  PersistentVolumeClaim, Pod; Deployment, StatefulSet, DaemonSet, ReplicaSet;
  Job, CronJob; HorizontalPodAutoscaler; PodDisruptionBudget; NetworkPolicy.
  Nothing cluster-scoped. Not RBAC: a Role would let the chart's own pods
  make routes, DNS records or secrets items at run time. Not Endpoints or
  EndpointSlices, Ingresses or Gateway API objects, quotas, nor the cluster's
  other kinds (Istio, cert-manager, monitoring, secrets operators). A chart
  that needs one of them fails, and stays something the platform team
  installs by hand.
- **A plain address:** the route leads to the Service `chart.expose` names,
  which the chart writes. The platform chart's ValidatingAdmissionPolicy
  `forge-service-addresses` (Kubernetes 1.30+) refuses, in every namespace
  labelled `forge.dev/deploy-env`, a Service that is not a plain ClusterIP
  (ExternalName, LoadBalancer, NodePort, externalIPs). Without Endpoints in
  the allow list, a Service without a selector leads nowhere.
- **Pod security:** with charts on, `forge-services` labels every service's
  namespace `pod-security.kubernetes.io/enforce: baseline` and `warn:
  restricted` (its managed namespace metadata, so a recreated namespace has
  it too), and forge-api labels it again before writing `chart.json`.
- To make the projects, `forge-services` may create AppProjects in `argocd`
  (only with `deploys.charts.enabled`).

A chart that needs more fails: forge-api reads Argo CD's refusal from the
failed sync's per-resource results ("resource ...:ClusterRole is not
permitted", "namespace ... is not permitted"), only when that sync was of this
deploy's commit, and fails the deploy with `chart_not_allowed`, naming the
object and what to do.

### Chosen not to change

- `forge-services` may write every kind it allows into `argocd` too, not only
  AppProjects: Argo CD ties destinations to a project, not to kinds. What it
  writes there is what Forge's own chart renders, which names no namespace
  but the AppProject's; the deploy repo holds values, not templates. A
  separate project and ApplicationSet for the AppProjects would delete a
  project and its chart application at the same time on a teardown, which
  leaves the application stuck (it happened on 2026-10-25 and 2026-10-26).
- Turning charts off (`deploys.charts.enabled: false`) removes the
  ApplicationSet and with it every chart service's workloads: delete those
  services first (the values' comment says so).
- forge-api checks an HTTPS chart repository's address only when it connects
  itself: behind an egress proxy (`HTTPS_PROXY`), the proxy connects.

## The operations

- **Deploy:** `values.yaml`, wait for the namespace, label it, then
  `chart.json` and `chart-values.yaml`, then wait for both applications (the
  chart's at a revision that holds both files: a deploy of new values has
  the same `chart.json`).
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
its `index.yaml` (at most 32 MiB, redirects to https only, and no private,
loopback or link-local address unless the repository is listed).

## Not checked on a cluster

Built and tested against the fakes (`envtest.Argo` makes the chart
applications, can refuse one as Argo CD does, per resource, and keep a
refusal of an earlier sync). Had an independent review; its findings are
fixed or listed above. Not run against a real Argo CD: the files generator's
parameters, an HTTPS chart with an empty `path`, the AppProject made from
another application, where Argo CD puts a refused object's message, and the
admission policy need the exit gate on the cluster.

## Exit gate

podinfo is installed as a service, reachable at its address, promoted,
upgraded to a newer chart version and rolled back; a chart that needs a
ClusterRole is refused with a message that names it.
