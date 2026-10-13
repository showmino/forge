# A service's address, set in Forge

Date: 2026-10-13. Status: decided with the owner on 2026-10-13; built.

## Goal

A writer can change where a service is reached (the name before the domain,
and the domain) from the dashboard, without a change to its repo. It is a
danger-zone action: it moves a service's public address, and can take over a
name that is in use. The first user is `deogratias`, which takes the root of
`deogratias.cloud` over from the old `deogratias-cloud` site.

## Decisions (the owner's, 2026-10-13)

| Question | Choice |
| --- | --- |
| Where the setting lives | In the catalog (`Service.address`). It overrides forge.yaml's `run.host` and `run.domain`; clearing it goes back to them. A deploy whose forge.yaml asks for something else says so in its log |
| The root of a domain | `host: "@"`. Production is `https://<domain>`, staging `https://staging.<domain>` |
| A name already in use | Refused, as today, unless the writer claims it: they type each such address to confirm. The claim is kept on the service and is in the audit log. A claim covers names outside Forge only: an address another catalog service is deployed at, or has set, is refused all the same (DNS cannot tell the two apart) |
| When it applies | At the next deploy of each environment: staging on the next push, production on the next promote |

## API

`DeployService.SetAddress(service, host, domain, clear, claim[], validate_only, etag)`,
behind `services.write`, audited as `service.address`. It answers with the
address each environment gets (`PlannedAddress`: URL, public, in use, the
current URL), and with `validate_only` changes nothing: the dashboard shows
that before the writer confirms. `Service.address` (host, domain, claimed) is
output only on the catalog; `UpdateService` cannot set it.

## Addresses

- `host` is `""` (the service's name), `"@"` (the domain itself), or a DNS
  label that is neither another service's name nor an environment's
  (`staging`, `production`: `staging.<domain>` is the staging of the root).
- The root's production address needs a Gateway listener of its own: the
  public listener of a domain takes `*.<domain>`, which does not match the
  domain itself. `render.sh` adds `public-<zone>-root` (hostname `<domain>`)
  next to each `public-<zone>`.
- Forge makes no DNS record for the root: the platform's
  `CloudflareRecordsApplication` writes `<name>.<domain>` only. The root's
  record must exist already and point at the cluster's tunnel, and the
  tunnel must send the name to Forge's Gateway (`showmino/kubernetes`,
  `infrastructure/cloudflare/production/configmap.yaml`). Deleting the service
  leaves that record.
- The values file marks a public address that has no DNS record of Forge's
  with `public: true`, so its URL is `https://`.

## Rollout for deogratias

1. forge-api with this, and a render with the root listeners (the owner's OK).
2. In the dashboard: deogratias → Settings → Danger zone → Change address:
   host `@`, domain `deogratias.cloud`; claim `deogratias.cloud`.
3. The owner's pull request in `showmino/kubernetes`: the tunnel sends
   `deogratias.cloud` to `10.150.1.222` (Forge's Gateway) instead of
   `10.150.1.200`. That is the moment the old site stops answering there.
4. Push to deogratias (staging moves to `staging.deogratias.cloud`), then
   promote.
