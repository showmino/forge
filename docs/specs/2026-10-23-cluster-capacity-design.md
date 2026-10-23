# Cluster capacity: `forge cluster` and the dashboard's Clusters page

Status: proposed 2026-10-23, waiting for the owner's go-ahead.

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

`proto/forge/v1/cluster.proto`, AIP-style:

```proto
service ClusterService {
  rpc ListClusters(ListClustersRequest) returns (ListClustersResponse);
  rpc GetCluster(GetClusterRequest) returns (Cluster);   // name: clusters/{id}
}

message Cluster {
  string name = 1;                 // clusters/default
  string kubernetes_version = 2;   // of the API server
  google.protobuf.Timestamp create_time = 3;  // oldest node's creation
  string grafana_url = 4;          // empty when --grafana-url is unset
  Capacity totals = 5;             // sum over schedulable nodes
  repeated Node nodes = 6;         // GetCluster only; empty in List
  bool metrics_available = 7;      // false when metrics-server did not answer
}

message Node {
  string name = 1;
  repeated string roles = 2;       // control-plane, etcd, worker
  string condition = 3;            // Ready, NotReady, MemoryPressure, DiskPressure, PIDPressure
  bool schedulable = 4;            // false when cordoned
  string kubelet_version = 5;
  Capacity capacity = 6;
}

message Capacity {
  int64 cpu_millis = 1;            // allocatable
  int64 cpu_used_millis = 2;       // metrics-server; 0 without metrics
  int64 memory_bytes = 3;          // allocatable
  int64 memory_used_bytes = 4;
  int32 pods = 5;                  // running on the node(s)
  int32 pods_max = 6;              // allocatable
}
```

Status ("Healthy", "Memory pressure on 1 node", "Memory at 84%") is worked out
by the clients from these numbers, with the design's thresholds (amber at 80%,
red at 90%), as a pure function with its own tests (`lib/cluster.ts` in
forge-web; `internal/cli` for the table's STATUS column).

### Authorization and audit

- A new action `cluster.read` with role `admin` in `actionRoles`; both RPCs
  call `authorizeTeam(ctx, ActClusterRead, "")`. A reader gets
  `PermissionDenied: ... only an admin may`.
- Both RPCs go in `auditReads`.
- Not in `workloadProcedures`: CI cannot call them.

### Server

- `internal/server/clusters.go`: the handler, over a small interface
  (`ListNodes`, `NodeMetrics`, `ServerVersion`) so tests use a fake and the
  e2e test the real client.
- Served only when forge-api has Kubernetes access (the same in-cluster config
  `--previews` uses); otherwise `Unimplemented`, like the other optional
  services.
- Pods per node: one `pods` list with a field selector
  `status.phase=Running`, counted by `spec.nodeName`.
- metrics-server down: the RPC still answers, with `metrics_available=false`
  and used values 0; clients show "No usage figures: metrics-server did not
  answer".
- Cached for 30 seconds in memory, so a dashboard left open does not load the
  API server.

### Kubernetes access (a cluster change: rollout needs the owner's OK)

A new ClusterRole in `deploy/k3s/forge.yaml.tmpl`, bound to the `forge-api`
ServiceAccount, read-only:

```yaml
rules:
  - apiGroups: [""]
    resources: [nodes]
    verbs: [get, list]
  - apiGroups: [""]
    resources: [pods]
    verbs: [list]
  - apiGroups: [metrics.k8s.io]
    resources: [nodes]
    verbs: [get, list]
```

The previews role already lists pods; this role keeps cluster reads separate
so either can be removed without the other. The render goldens and
`render_test.sh` change with it.

## CLI

`internal/cli/cluster.go`:

```
$ forge cluster list
NAME      STATUS   CPU   MEMORY   NODES   KUBERNETES
default   Healthy  23%   61%      5       v1.37.0+k3s1

$ forge cluster get default
NAME     ROLES               CONDITION  SCHEDULABLE  CPU   MEMORY  PODS
srv-kube control-plane,etcd  Ready      yes          31%   72%     41/110
...
```

`-o json|yaml|name` as every command. Golden tests, harness tests with
`t-writer` (admin) passing and `t-reader` denied.

## Dashboard (forge-web)

After the forge PR is merged: `scripts/sync-proto.sh`, then

- `/clusters`: the design's list, with CPU and memory meters (no storage
  column in milestone 1), the "Open Grafana" button when `grafana_url` is set.
- `/clusters/[name]`: the stat row (CPU, memory, status), the Nodes table
  (no disk column), no Volumes section.
- The rail shows "Clusters" only to admins (`GetStatusResponse.admin`); the
  API still enforces it.
- Every page shows its CLI command.

## Milestone 2: disk and volumes (separate go-ahead)

forge-api queries Prometheus (`--prometheus-url`, for example
`http://prometheus-prometheus.monitoring.svc:9090`; forge-api's
NetworkPolicy is ingress only, so egress is open):

- Node disk: `node_filesystem_size_bytes` / `node_filesystem_avail_bytes` on
  `/` per node (node-exporter).
- Volumes: `kubelet_volume_stats_used_bytes` / `_capacity_bytes` per PVC, with
  the namespace mapped to the Forge service and environment when it is one.

Prometheus unreachable: the same as metrics-server down, for those fields.

## Documentation

- techdocs: a page `docs/forge/` for "Cluster capacity" (admins), and
  `16-command-reference.md` for `forge cluster list|get`.
- `docs/agents/status.md` and, after the rollout, `verifying.md` (new flags in
  `live.env`, the ClusterRole, how to check).

## Not checked

- Whether metrics-server's figures match Grafana's dashboards closely enough
  that the two do not seem to disagree (metrics-server is a short window,
  Grafana usually a 5-minute rate).
