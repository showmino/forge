# forge

Forge itself for your GitHub organization: the API, the dashboard, the login
(Dex) and a database, on one host. Not the chart a Forge service is deployed
with (`deploy/argocd/chart`, published as `forge-chart`).

Install it from code: an Argo CD Application in your cluster's Git repo, so
an upgrade or a setting is a commit. Make the Secrets first (they don't go in
Git), then commit and apply the Application:

```sh
kubectl create namespace forge
kubectl -n forge create secret generic forge-github \
  --from-file=app-private-key=my-app.private-key.pem \
  --from-literal=client-id=<the App's client ID> \
  --from-literal=client-secret=<a client secret of the App>
password=$(openssl rand -hex 24)
kubectl -n forge create secret generic forge-db \
  --from-literal=password="$password" \
  --from-literal=uri="postgres://forge:$password@forge-db.forge.svc:5432/forge?sslmode=disable"
```

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
    targetRevision: 0.29.0
    path: .
    helm:
      releaseName: forge
      valuesObject:
        github: {org: my-org, appID: "123456"}
        host: forge.example.com
        ingress: {className: nginx, tls: {secretName: forge-tls}}
        database: {bundled: {existingSecret: forge-db}}
  destination:
    server: https://kubernetes.default.svc
    namespace: forge
  syncPolicy:
    automated: {prune: true, selfHeal: true}
```

`forge-db` holds the database password: Argo CD renders the chart without
reading the cluster, so the chart cannot keep one it made. To try Forge
without Argo CD, `helm install forge oci://ghcr.io/showmino/charts/forge -n
forge -f forge-values.yaml` works too, and makes `forge-db` itself.

The dashboard is at `/`, the API at `/forge.v1.*` and `/cli/`, the login at
`/dex`. The GitHub App's callback URL is `https://<host>/dex/callback`.

The walkthrough, with the GitHub App's settings, is
[Install Forge](https://forgegate.io/docs/running-your-own-forge), and
[Chart values](https://forgegate.io/docs/chart-values) lists every setting;
`values.yaml` has the defaults, with comments.
