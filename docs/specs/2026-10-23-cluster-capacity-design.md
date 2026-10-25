# Cluster capacity: `forge cluster` and the dashboard's Clusters page

Status: approved 2026-10-23; milestone 1 live since 2026-10-24 (forge-api `v0.24.0`); milestone 2 live since 2026-10-24 (forge-api `v0.25.0`).

## What the owner asked for

The Forge Console design (`forge-web/docs/design/forge-console.html`) has a
Clusters page: each cluster with its status and how much CPU, memory and
storage is left, and a page per cluster with its nodes and volumes. Build what
Forge can back with real data.

Decided with the owner on 2026-10-23:

| Question | Decision |
| --- | --- |
| Scope of the first version | Nodes, their conditions, CPU and memory (metrics-server). Disk and volumes from Prometheus are a second milestone |
| Who may see it | Admins only |
| The cluster's name | A forge-api flag, `--cluster-name`, default `default` |
| Link to Grafana | An optional forge-api flag, `--grafana-url`; no link when it is empty |

## What the design shows that Forge leaves out

- **More than one cluster.** Forge runs on one cluster; staging and production
  are environments inside it. The list has one row. The API is shaped as a
  list so a second cluster needs no new RPC.
- **Region.** Nothing on the cluster says it. Left out.
- **Disk and volumes** until milestone 2.

## API

`proto/forge/v1/cluster.proto`, AIP-style (comments shortened here):

```proto
service ClusterService {
  rpc ListClusters(ListClustersRequest) returns (ListClustersResponse);
  rpc GetCluster(GetClusterRequest) returns (GetClusterResponse);  // name: clusters/{id}
}

message Cluster {
  string name = 1;                 // clusters/default
  string kubernetes_version = 2;   // of the API server
  google.protobuf.Timestamp create_time = 3;  // oldest node's creation
  string grafana_url = 4;          // empty when --grafana-url is unset
  Capacity totals = 5;             // sum over schedulable nodes
  repeated Node nodes = 6;         // by name, in List and Get alike
  bool metrics_available = 7;      // false when metrics-server did not answer
}

message Node {
  string name = 1;
  repeated string roles = 2;       // control-plane, etcd; worker when none
  string condition = 3;            // Ready, NotReady, MemoryPressure, DiskPressure, PIDPressure, NetworkUnavailable
  bool schedulable = 4;            // false when cordoned
  string kubelet_version = 5;
  Capacity capacity = 6;
}

message Capacity {
  int64 cpu_millis = 1;            // allocatable
  int64 cpu_used_millis = 2;       // metrics-server; 0 without metrics
  int64 memory_bytes = 3;          // allocatable
  int64 memory_used_bytes = 4;
  int32 pods = 5;                  // on the node and not finished
  int32 pods_max = 6;              // allocatable
}
```

The list carries the nodes too: its status needs their conditions, and there
is one cluster.

Status ("Healthy", "Memory pressure on 1 node", "Memory at 84%") is worked out
by the clients from these numbers: a node condition first (NotReady worst),
then CPU or memory from 80% used (the design's amber; red from 90%). It is a
pure function with its own tests (`clusterStatus` in `internal/cli/cluster.go`;
`lib/cluster.ts` in forge-web).

### Authorization and audit

- A new action `cluster.read` with role `admin` in `actionRoles`; both RPCs
  call `authorizeTeam(ctx, ActClusterRead, "")`. A reader gets
  `PermissionDenied: ... only an admin may`.
- Both RPCs go in `auditReads`.
- Not in `workloadProcedures`: CI cannot call them.

### Server

- `internal/server/clusters.go`: the handler, on the Kubernetes clientset
  (nodes, pods, server version) and the dynamic client (metrics-server's
  `metrics.k8s.io/v1beta1` nodes, so no new dependency). Tests use fakes
  (`envtest.Nodes`).
- Off unless forge-api runs with `--clusters` (`FORGE_CLUSTERS=1`); off, both
  RPCs answer `FailedPrecondition`, like the other optional services. It
  reuses the previews' Kubernetes connection when there is one.
- Pods per node: one `pods` list with a field selector
  `status.phase!=Succeeded,status.phase!=Failed` (pending pods bound to a node
  count against its limit too), counted by `spec.nodeName`.
- Totals are over the nodes that take new pods: not cordoned and not
  NotReady.
- One read is bounded by 10 seconds: callers wait on the cache's lock behind it.
- metrics-server down: the RPC still answers, with `metrics_available=false`
  and used values 0; clients show "No usage figures: metrics-server did not
  answer".
- No permission to read nodes or pods: `FailedPrecondition` naming the
  ClusterRole `forge-api-clusters`.
- Cached for 30 seconds in memory, so a dashboard left open does not load the
  API server.
- `--grafana-url` must be an http or https address: the dashboard renders it
  as a link.

### Kubernetes access (a cluster change: rollout needs the owner's OK)

`deploy/k3s/clusters.yaml.tmpl`, which `render.sh` adds with
`FORGE_CLUSTERS=1` (it needs previews, whose ServiceAccount it binds): a
ClusterRole `forge-api-clusters` bound to the `forge-api` ServiceAccount,
read-only, exactly the calls the handler makes:

```yaml
rules:
  - apiGroups: [""]
    resources: [nodes]
    verbs: [list]
  - apiGroups: [""]
    resources: [pods]
    verbs: [list]
  - apiGroups: [metrics.k8s.io]
    resources: [nodes]
    verbs: [list]
```

The previews role already lists pods; this role keeps cluster reads separate
so either can be removed without the other. `FORGE_CLUSTER_NAME` and
`FORGE_GRAFANA_URL` become forge-api's settings. `render_test.sh` pins the
role (`testdata/clusters.golden`); without `FORGE_CLUSTERS` the render is
unchanged.

The rollout sets in `live.env` (after a forge-api release with this code):

```
FORGE_CLUSTERS=1
FORGE_GRAFANA_URL=https://grafana.showmino.com
```

## CLI

`internal/cli/cluster.go`:

```
$ forge cluster list
NAME      STATUS                      CPU   MEMORY   NODES   KUBERNETES
default   Memory pressure on 1 node   29%   42%      3       v1.37.0+k3s1

$ forge cluster get default
FIELD        VALUE
name         clusters/default
status       Memory pressure on 1 node
cpu          29% of 12 cores
...

NODE       ROLES                CONDITION        SCHEDULABLE   CPU              MEMORY            PODS
srv-cron   worker               Ready            yes           31% of 8 cores   25% of 16.0 GiB   2/110
...
```

`-o json|yaml|name` as every command; JSON is the `Cluster` itself. Golden
tests, harness tests with `t-writer` (admin) passing and `t-reader` denied.
`forge auth can cluster.read` answers whether you may.

## Dashboard (forge-web)

After the forge PR is merged: `scripts/sync-proto.sh`, then

- `/clusters`: the design's list, with CPU and memory meters (no storage
  column in milestone 1), the "Open Grafana" button when `grafana_url` is set.
- `/clusters/[name]`: the stat row (CPU, memory, status), the Nodes table
  (no disk column), no Volumes section.
- The rail shows "Clusters" only to admins (`GetStatusResponse.admin`); the
  API still enforces it.
- Every page shows its CLI command.

## Milestone 2: disk and volumes (go-ahead 2026-10-24)

forge-api queries Prometheus (`--prometheus-url`, for example
`http://prometheus-prometheus.monitoring.svc:9090`; forge-api's
NetworkPolicy is ingress only, so egress is open) with four fixed instant
queries (`internal/server/prometheus.go`, answers capped at 8 MiB):

- Node disk: `node_filesystem_size_bytes` / `node_filesystem_avail_bytes` on
  `/` per node (node-exporter); used is size minus available. The node is
  `kubernetes_node` (or `node`) when the series has it, else the node whose
  InternalIP is the `instance` host. Proto: `Capacity.disk_bytes`,
  `disk_used_bytes`, summed into the totals like CPU.
- Volumes: `kubelet_volume_stats_used_bytes` / `_capacity_bytes` per PVC.
  On k3s the kubelet is scraped through the API server: the claim's
  namespace is `exported_namespace`, and every API server reports it once, so
  claims are de-duplicated. Proto: `Cluster.volumes` (namespace, claim, used,
  capacity, and `service` and `environment` when the namespace carries
  Forge's labels), fullest first.
- `Cluster.storage_available` is false without Prometheus or when it fails;
  the rest still answers, as with metrics-server.
- Status: after node conditions, the fullest of CPU, memory, disk and any
  volume from 80% ("Volume payments-production/data at 90%").
- Kubernetes: `list` on namespaces, added to `forge-api-clusters` only when
  `FORGE_PROMETHEUS_URL` is set, so the live render does not change before the
  rollout. Without it volumes are listed but not tied to services.

Checked against the owner's cluster on 2026-10-24 with a local forge-api
(dev token, read-only kubeconfig, the LAN Prometheus): five disks, nine
claims (15 series before de-duplication), `forgegate-staging/main-1` tied to
forgegate's staging.

## Milestone 3: other clusters (decided with the owner 2026-10-25)

The owner added an AKS cluster, which Argo CD on k3s already knows as
`aks-production`. Forge should show it; deploying to it is not part of this.

| Decision | Choice |
| --- | --- |
| Scope | See it only: nodes, CPU, memory, pods |
| Credential | Argo CD's, from its cluster Secret (the owner chose this over a read-only ServiceAccount of Forge's own on AKS) |
| Names | `clusters/production` for k3s (`FORGE_CLUSTER_NAME=production`, was `default`) and `clusters/aks-production` |
| Disk and volumes on AKS | None: there is no Prometheus forge-api can reach there |

- forge-api: `--remote-clusters` (`FORGE_REMOTE_CLUSTERS`), names of Argo
  CD cluster Secrets in `argocd`, comma-separated; each is a cluster of that
  name, listed after the one forge-api runs on. The Secret's name is the
  cluster's name, so it must be a DNS label of at most 40 characters
  (`argocd cluster add` makes longer ones; register such a cluster under a
  short Secret name). On every read (at most every
  30 seconds per cluster) forge-api gets the Secret and builds new clients
  only when it changed, so a rotated credential is picked up
  (`cmd/forge-api/clusters.go`). It takes a bearer token or a client
  certificate over verified TLS and refuses the rest: exec plugins (they
  would run a program in forge-api), AWS and password logins, `insecure`.
- Server: one cache per cluster; a list reads them in parallel. A cluster
  that cannot be read is a row with `Cluster.unavailable_reason` (new field
  10) and nothing else; only when every cluster fails is the list an error.
  A remote's failed read is remembered for 30 seconds, so a cluster that is
  down does not make every list wait (the cluster forge-api runs on is
  asked again each time). `GetCluster` on it is the error. Remote clusters
  get no Grafana link and no Prometheus figures.
- render.sh: `FORGE_REMOTE_CLUSTERS` (needs `FORGE_CLUSTERS`; DNS labels,
  none twice, none equal to `FORGE_CLUSTER_NAME`) sets the env var and adds
  the Role `forge-api-remote-clusters` in `argocd`: `get` on exactly those
  Secrets (`testdata/clusters.golden`).
- CLI: such a cluster says `Unavailable` in `forge cluster list`, with the
  reason under the table.
- Security: forge-api can then do on AKS whatever Argo CD can, which is
  normally cluster-admin; it only lists nodes and pods and reads node
  metrics. A Forge API that is taken over is a way into AKS. The owner's
  choice; a read-only ServiceAccount on AKS is the way to narrow it later
  (register it as its own Argo CD-style Secret and name that one).
- Dashboard: `unavailable_reason` shown on the Clusters page (forge-web,
  after `sync-proto.sh`).

## Documentation

- techdocs: a page `docs/forge/` for "Cluster capacity" (admins), and
  `16-command-reference.md` for `forge cluster list|get`.
- `docs/agents/status.md` and, after the rollout, `verifying.md` (new flags in
  `live.env`, the ClusterRole, how to check).

## Not checked

- Whether metrics-server's figures match Grafana's dashboards closely enough
  that the two do not seem to disagree (metrics-server is a short window,
  Grafana usually a 5-minute rate).
