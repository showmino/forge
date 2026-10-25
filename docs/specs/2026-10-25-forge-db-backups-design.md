# forge-db backups: CloudNativePG to Azure Blob, with point-in-time recovery

Status: live on k3s since 2026-10-25 (the plugin is showmino/kubernetes#130, the render forge#141); the restore drill is not done yet. Issue #48.

## What there is today

`forge-db` (CloudNativePG, two instances, `longhorn-retain` volumes) holds the
catalog, the audit log, sessions, resource state and Dex's logins. Its two
volumes are in Longhorn's `showmino` job group: a snapshot every hour (24
kept) and a backup every night at 02:30 to the NAS
(`nfs://10.150.0.12:/srv/nfs/longhorn-backups`, 14 kept). Those are copies of
a running disk, on the LAN, without point-in-time recovery, and no restore
has been tried. They stay as a second line.

## Decided with the owner on 2026-10-25

| Question | Decision |
| --- | --- |
| Method | CloudNativePG backups through the Barman Cloud plugin: a base backup every night and WAL archived continuously, so a restore can go to any point in time |
| Where | Azure Blob: storage account `stminoforgesdcproduction`, container `forge-db` (offsite) |
| Credential | A SAS token scoped to the container, in 1Password (vault Kube) |
| Retention | 14 days, as Longhorn |
| Restore drill | Into a scratch namespace on the cluster, then deleted; it needs the owner's OK when it runs |

## The pieces

### 1. The plugin (showmino/kubernetes, a pull request the owner merges)

CloudNativePG 1.30 takes backups through plugins; the in-tree
`barmanObjectStore` is deprecated. Add the Argo CD app
`apps/helm/plugin-barman-cloud/production/app.yaml` like
`cluster-native-pg`: chart `plugin-barman-cloud` `0.8.1` (plugin `v0.15.1`)
from `https://cloudnative-pg.github.io/charts`, namespace `cnpg-system`. It
needs cert-manager, which runs.

### 2. The credential (the owner, in Azure and 1Password)

- Container `forge-db` in `stminoforgesdcproduction` (made 2026-10-25).
- A SAS token on that container only, through the stored access policy
  `forge` (read, add, create, write, delete, list; HTTPS only), expiring
  2027-10-25: deleting the policy revokes it at once. Rotate it before then.
- 1Password item `vaults/production/items/forge-backup-storage` with exactly
  the fields `storage-account-name` and `sas-token` (the operator copies every
  field into the Secret: no account key in it). The vault `production` holds
  only the platform's own credentials and is not in `FORGE_SECRET_VAULTS`, so
  no service's `run.secrets` can name it. The 1Password Connect server must be
  given access to that vault.
- Recommended on the account: blob soft delete (7 days) and a lifecycle rule
  that deletes blobs older than 30 days, as a backstop if Barman's own
  retention ever stops.

### 3. forge's render (this repo, `deploy/k3s`)

A new template `backups.yaml.tmpl`, rendered when `FORGE_DB_BACKUP_DEST` is
set (lines marked `#backups`):

```yaml
apiVersion: onepassword.com/v1
kind: OnePasswordItem
metadata: {name: forge-db-backup, namespace: forge}
spec: {itemPath: "${FORGE_DB_BACKUP_ITEM}"}   # vaults/production/items/forge-backup-storage
---
apiVersion: barmancloud.cnpg.io/v1
kind: ObjectStore
metadata: {name: forge-db-azure, namespace: forge}
spec:
  retentionPolicy: "14d"
  configuration:
    destinationPath: "${FORGE_DB_BACKUP_DEST}"  # https://stminoforgesdcproduction.blob.core.windows.net/forge-db
    azureCredentials:
      storageAccount: {name: forge-db-backup, key: storage-account-name}
      storageSasToken: {name: forge-db-backup, key: sas-token}
    wal: {compression: gzip}
    data: {compression: gzip}
---
apiVersion: postgresql.cnpg.io/v1
kind: ScheduledBackup
metadata: {name: forge-db-nightly, namespace: forge}
spec:
  schedule: "0 0 3 * * *"        # 03:00, after Longhorn's 02:30
  backupOwnerReference: self
  cluster: {name: forge-db}
  method: plugin
  pluginConfiguration: {name: barman-cloud.cloudnative-pg.io}
  immediate: true                # a first base backup right away
```

and in `database.yaml.tmpl`, on the `Cluster`:

```yaml
  plugins: #backups
    - name: barman-cloud.cloudnative-pg.io #backups
      isWALArchiver: true #backups
      parameters: {barmanObjectName: forge-db-azure} #backups
```

`render.sh` checks `FORGE_DB_BACKUP_DEST` (an `https://<account>.blob.core.windows.net/<container>`
address) and `FORGE_DB_BACKUP_ITEM` (`vaults/<vault>/items/<item>`), and
needs both. When `FORGE_SECRET_VAULTS` holds the item's vault, it refuses
the render unless the item is in `FORGE_SECRET_DENIED_ITEMS`: the token may
write and delete the backups. `render_test.sh` pins the objects in a golden
and checks that without them the render is unchanged. forge-api is not
touched; its NetworkPolicy is ingress only.

The item is in its own vault, `production`, which `FORGE_SECRET_VAULTS`
(`Kube`) does not list; had it been in a listed vault, render.sh would have
required it on the deny list.

### 4. The rollout (needs the owner's OK)

First the plugin's Argo CD app must be Synced and Healthy (otherwise the
`ObjectStore` has no kind). Then `live.env` gets `FORGE_DB_BACKUP_DEST` and
`FORGE_DB_BACKUP_ITEM`. Adding the plugin to the `Cluster` puts a sidecar
into the Postgres pods: CloudNativePG restarts the replica, then restarts the
primary in place (`primaryUpdateMethod` is the default, `restart`). forge-api
and Dex lose the database for as long as Postgres takes to start, some tens
of seconds. Then:

- `kubectl -n forge get cluster forge-db` shows `ContinuousArchiving` true;
- `kubectl -n forge get backups`: the first backup (`immediate`) may have
  failed during the restarts and is not retried; if so, create a one-off
  `Backup` (`method: plugin`, plugin `barman-cloud.cloudnative-pg.io`) and see
  it `completed`;
- the container has `forge-db/base/...` and `forge-db/wals/...`;
- the 1Password Secret `forge-db-backup` has exactly the keys
  `storage-account-name` and `sas-token` (names only);
- Argo CD shows `forge` Synced (the plugin entry says `enabled: true`, as
  CloudNativePG defaults it).

### 5. The restore drill (needs the owner's OK when it runs)

In a namespace `forge-db-drill`, at least an hour after the first base
backup completed:

- its own 1Password item with a second SAS token, from a second stored
  access policy with read and list only, and an `ObjectStore` on it with no
  `retentionPolicy`: the drill can never write to or prune the production
  backups;
- a `Cluster` `forge-db-drill` with one instance, no `plugins:`, on a storage
  class whose reclaim policy is `Delete` (not `longhorn-retain`: a Released
  volume would keep a copy of production data), and

```yaml
  bootstrap:
    recovery: {source: origin}
  externalClusters:
    - name: origin
      plugin:
        name: barman-cloud.cloudnative-pg.io
        parameters: {barmanObjectName: forge-db-drill-azure, serverName: forge-db}
```

Before it: note a time T (RFC 3339 with offset) and the counts of services,
audit events, sessions and resource records at T (read-only `psql` on the
live database). Restore once to the latest WAL and once to
`recoveryTarget.targetTime: T`; the second must show T's counts exactly.
Then delete the namespace and check that no PersistentVolume of it is left.
The steps go into `verifying.md`, with the time it took. The drill is run by
hand, never in CI.

### 6. Knowing it works

With the backups, the template adds a `PodMonitor` for forge-db (Prometheus
reads CloudNativePG's own exporter) and a `PrometheusRule`:

- `ForgeDBWALArchivingFailing` (critical): archive failures in the last 15
  minutes. While WAL cannot be archived, Postgres keeps it on the 5Gi volume
  until the primary stops.
- `ForgeDBWALArchivingStale`: the primary archived nothing for 30 minutes.
- `ForgeDBVolumeFilling`: a forge-db volume above 80%.

They go wherever the cluster's Alertmanager sends today. An alert on the age
of the last base backup follows once the plugin's metrics are known; until
then `kubectl -n forge get backups` says it. The SAS token expires
2027-10-25: the owner keeps a reminder.

## Not in this design

- Backups of the services' databases (forgegate's and later): the same
  plugin serves them, but Forge's resource module decides that; a later
  change, in `deploy/runner/module`.
- The platform chart (`deploy/platform/chart`) for other Forges.
- Moving Longhorn's NAS backups offsite.

## Not checked

- That the SAS token's permissions are enough for Barman's retention to
  delete old backups (it needs delete and list); the drill will show it.
- The plugin's metric names on v0.15.1, for the backup-age alert.
- That `barman-cloud` in plugin v0.15.1 supports PostgreSQL 18; the first
  backup will show it.
- The time zones: the ScheduledBackup runs on the operator's clock, Longhorn's
  jobs on Longhorn's.
