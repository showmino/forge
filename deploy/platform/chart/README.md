# forge

Forge itself for your GitHub organization: the API, the dashboard and the
login (Dex), on one host. Not the chart a Forge service is deployed with
(`deploy/argocd/chart`, published as `forge-chart`).

Forge's data lives in a PostgreSQL server **you run**: the chart runs no
database of its own, so that removing Forge's namespace or its Application
never removes the data. Put it in another namespace with an Argo CD
Application of its own (for example a CloudNativePG `Cluster`), or use a
managed server, and back it up. It needs two databases owned by one user:
`forge` for the API and `dex` for the login's sessions.

Install Forge from code: an Argo CD Application in your cluster's Git repo, so
an upgrade or a setting is a commit. Make the Secrets first (they don't go in
Git), then commit and apply the Application:

```sh
kubectl create namespace forge
kubectl -n forge create secret generic forge-github \
  --from-file=app-private-key=my-app.private-key.pem \
  --from-literal=client-id=<the App's client ID> \
  --from-literal=client-secret=<a client secret of the App>
# The database user "forge"'s password, which you set on the server.
password='<the password of the user forge>'
kubectl -n forge create secret generic forge-database \
  --from-literal=password="$password" \
  --from-literal=uri="postgres://forge:$password@db.example.com:5432/forge?sslmode=require"
```

The password goes into `uri` URL-escaped, if it has characters such as
`@ : / ? # %`.

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: forge
  namespace: argocd
spec:
  project: default
  source:
    repoURL: oci://ghcr.io/showmino/charts/forge
    targetRevision: 0.32.0
    path: .
    helm:
      releaseName: forge
      valuesObject:
        github: {org: my-org, appID: "123456"}
        host: forge.example.com
        ingress: {className: nginx, tls: {secretName: forge-tls}}
        database: {existingSecret: forge-database, host: db.example.com}
  destination:
    server: https://kubernetes.default.svc
    namespace: forge
  syncPolicy:
    automated: {prune: true, selfHeal: true}
```

The dashboard is at `/`, the API at `/forge.v1.*` and `/cli/`, the login at
`/dex`. The GitHub App's callback URL is `https://<host>/dex/callback`.

**From a chart before 0.32.0**, which ran the database itself
(`database.mode: bundled`, the default, or `cloudnativepg`): 0.32.0 refuses
those values or asks for `database.host`, so nothing is removed until you
change them. The sync after that change deletes the old database's objects,
so first:

1. Copy the data to your own server: `pg_dump` of `forge` and `dex`,
   `pg_restore --no-owner --role=forge`.
2. Keep the old database: annotate its objects (the StatefulSet `forge-db`
   and the Secret `forge-db`, or the CloudNativePG `Cluster` `forge-db`, whose
   deletion also deletes its volumes) with
   `argocd.argoproj.io/sync-options: Prune=false,Delete=false` under Argo CD,
   or `helm.sh/resource-policy=keep` under `helm upgrade`.
3. Set `database.*` and sync. Delete the old database once Forge works.

With `database.mode: external`, move `database.external.*` to `database.*`.

The walkthrough, with the GitHub App's settings, is
[Install Forge](https://forgegate.io/docs/running-your-own-forge), and
[Chart values](https://forgegate.io/docs/chart-values) lists every setting;
`values.yaml` has the defaults, with comments.
