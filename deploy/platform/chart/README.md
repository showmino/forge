# forge

Forge itself for your GitHub organization: the API, the dashboard, the login
(Dex) and a database, on one host. Not the chart a Forge service is deployed
with (`deploy/argocd/chart`, published as `forge-chart`).

```sh
kubectl create namespace forge
kubectl -n forge create secret generic forge-github \
  --from-file=app-private-key=my-app.private-key.pem \
  --from-literal=client-id=<the App's client ID> \
  --from-literal=client-secret=<a client secret of the App>
helm install forge oci://ghcr.io/showmino/charts/forge -n forge \
  --set github.org=my-org --set github.appID=123456 \
  --set host=forge.example.com \
  --set ingress.className=nginx --set ingress.tls.secretName=forge-tls
```

The dashboard is at `/`, the API at `/forge.v1.*` and `/cli/`, the login at
`/dex`. The GitHub App's callback URL is `https://<host>/dex/callback`.

The walkthrough, with the GitHub App's settings, is
[Install Forge](https://forgegate.io/docs/running-your-own-forge), and
[Chart values](https://forgegate.io/docs/chart-values) lists every setting;
`values.yaml` has the defaults, with comments.
