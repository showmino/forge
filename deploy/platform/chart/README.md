# forge-platform

Deploys a whole Forge instance (Dex, forge-api, previews, deploys) for a
GitHub org and Kubernetes cluster of your own.

This is not the chart a Forge service deploys with (`deploy/argocd/chart`,
published as `forge-chart`) -- this is the platform itself.

Full walkthrough, including the GitHub App setup and the two Secrets this
chart expects to already exist: `docs/forge/17-running-your-own-forge.md` in
`showmino/techdocs`.

MVP scope: core login, previews and deploys. Not yet covered: public
domains (`FORGE_PUBLIC_DOMAINS`), the resources/runner feature, or a secrets
backend other than 1Password's default -- those remain
`deploy/k3s/render.sh`-only for now, which also remains how showmino's own
production is rendered; this chart does not replace it.

The database is optional: `database.managed: false` skips the
CloudNativePG `Cluster`/`Database` this chart creates by default, and lets
you bring your own Postgres-compatible database instead -- create the
Secret named by `database.secretName` yourself first (keys `uri` and
`password`); see `values.yaml` for the exact shape.

```sh
helm install my-forge . \
  --set githubOrg=my-org \
  --set githubAppID=123456 \
  --set exposure.dexHost=dex.example.com \
  --set exposure.apiHost=forge.example.com
```

See `values.yaml` for every setting.
