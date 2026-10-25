# forge-db backups: CloudNativePG to Azure Blob, with point-in-time recovery

Status: proposed 2026-10-25, waiting for the owner's go-ahead. Issue #48.

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

- Container `forge-db` in `stminoforgesdcproduction`.
- A SAS token on that container only: read, add, create, write, delete, list;
  HTTPS only; an expiry the owner chooses (a year), noted in the runbook.
- 1Password item, vault Kube, for example `forge-db-backup`, with the fields
  `AZURE_STORAGE_ACCOUNT` and `AZURE_STORAGE_SAS_TOKEN`. It goes into
  `FORGE_SECRET_DENIED_ITEMS`: no service may read it.
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
spec: {itemPath: "${FORGE_DB_BACKUP_ITEM}"}   # vaults/Kube/items/forge-db-backup
---
apiVersion: barmancloud.cnpg.io/v1
kind: ObjectStore
metadata: {name: forge-db-azure, namespace: forge}
spec:
  retentionPolicy: "14d"
  configuration:
    destinationPath: "${FORGE_DB_BACKUP_DEST}"  # https://stminoforgesdcproduction.blob.core.windows.net/forge-db
    azureCredentials:
      storageAccount: {name: forge-db-backup, key: AZURE_STORAGE_ACCOUNT}
      storageSasToken: {name: forge-db-backup, key: AZURE_STORAGE_SAS_TOKEN}
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
needs both. `render_test.sh` pins the objects in a golden and checks that
without them the render is unchanged. forge-api is not touched; its
NetworkPolicy is ingress only.

### 4. The rollout (needs the owner's OK)

`live.env` gets `FORGE_DB_BACKUP_DEST` and `FORGE_DB_BACKUP_ITEM`, and the
item joins `FORGE_SECRET_DENIED_ITEMS`. Adding the plugin to the `Cluster`
puts a sidecar into the Postgres pods: CloudNativePG restarts the replica,
then switches over and restarts the old primary. forge-api and Dex see a few
seconds of failed connections. Then:

- `kubectl -n forge get cluster forge-db` shows `ContinuousArchiving` true;
- `kubectl -n forge get backups` shows the first backup `completed`;
- the container has `forge-db/base/...` and `forge-db/wals/...`.

### 5. The restore drill (needs the owner's OK when it runs)

In a namespace `forge-db-drill`: the 1Password item and the `ObjectStore`
(read side) again, and a `Cluster` `forge-db-drill` with one instance and

```yaml
  bootstrap:
    recovery: {source: origin}
  externalClusters:
    - name: origin
      plugin:
        name: barman-cloud.cloudnative-pg.io
        parameters: {barmanObjectName: forge-db-azure, serverName: forge-db}
```

once to the latest WAL and once to a `recoveryTarget.targetTime` an hour
back. Check: the counts of services, audit events, sessions and resource
records match the live database at those moments (read with `psql` on
both, read-only on the live one); then delete the namespace. The steps go
into `verifying.md`, with the time it took. The drill is run by hand, never
in CI.

### 6. Knowing it works

A `PrometheusRule` in the same template (kube-prometheus-stack picks it up):
an alert when the last successful backup is older than 26 hours, and when
WAL archiving fails (the plugin's and CloudNativePG's metrics). Where it
alerts goes wherever the cluster's Alertmanager sends today.

## Not in this design

- Backups of the services' databases (forgegate's and later): the same
  plugin serves them, but Forge's resource module decides that; a later
  change, in `deploy/runner/module`.
- The platform chart (`deploy/platform/chart`) for other Forges.
- Moving Longhorn's NAS backups offsite.

## Not checked

- That the SAS token's permissions are enough for Barman's retention to
  delete old backups (it needs delete and list); the drill will show it.
- The plugin's metric names on v0.15.1; the alert is written against what it
  exposes once installed.
