# File templates: add a file to an existing repo

Oct 14, 2026 · Showmino · Draft for review

Today every template makes a new repo (`forge create`). A landing zone is one
`.bicepparam` file in `showmino/landingzone`, so it needs a second kind of
template: one that renders a file and opens a pull request in a repo that
already exists. The first one is `landing-zone`, with the form of the old
portal.

## Decisions

| Question | Decision |
| --- | --- |
| What it does | forge-api renders the file, pushes it to a new branch `forge/<name>` of the target repo and opens a pull request. Merging is a person's call in that repo; the repo's own workflows deploy it. |
| Where it is declared | In `template.yaml`: `type: file` and a `target` (repo and path, both rendered). Templates without `type` stay `service`. |
| Catalog | A file template registers nothing in the catalog: the landing zone is not a Forge service. |
| GitHub access | The Forge GitHub App, which reaches `showmino/landingzone` now that it is in the org. It needs Contents and Pull requests write there. |
| Dashboard | The create form takes a file template too: it shows its inputs the same way, and ends with a link to the pull request instead of the service. |
| CLI | `forge template apply <template> --set k=v ...`, with the same output as `forge create`. |

## `template.yaml`

```yaml
apiVersion: forge.dev/v1
kind: Template
name: landing-zone
title: Landing zone
description: An Azure subscription with its network, as a pull request to showmino/landingzone.
type: file                          # new; default "service"
target:
  repo: showmino/landingzone
  path: "landingZones/{% .Params.type | title %}/lz-{% .Params.type %}-{% .Params.workload %}.bicepparam"
parameters:
  - name: type
    label: Landing zone type
    type: enum
    values: [corp, online]
  - name: environments
    label: Environments
    type: list                      # new: several of values, checkboxes
    values: [prod, staging, dev, test]
    default: [prod]
  - name: vnet
    label: vNet enabled
    type: bool
    default: true
  - name: region
    label: vNet region
    type: enum
    values: [norwayeast, swedencentral]
    default: norwayeast
  - name: ip_addresses
    label: IP addresses needed
    type: int
    min: 1                          # new
    max: 1024                       # new
  - name: workload
    label: Workload name
    type: string
    pattern: "^[a-z][a-z0-9-]{1,40}$"
  - name: iac
    label: IaC type
    type: enum
    values: [bicep, terraform]
    default: bicep
  - name: github_team
    label: GitHub team
    type: team                      # new: a team of the org, picked from the list, or none
  - name: entra_user
    label: Entra ID user
    type: string
    placeholder: c0-user@coop.no    # new
  - name: service_owner
    label: Service owner
    group: Tags                     # new: inputs under a heading
  - name: team
    label: Team
    group: Tags
  - name: system
    label: Service
    group: Tags
  - name: cost_center
    label: Cost center
    group: Tags
    pattern: "^[0-9]+$"
```

The skeleton is the one file, `skeleton/file.bicepparam`, rendered as
templates are today. For the inputs above it gives your example: `using
'../../main_corp.bicep'`, `subscriptionAliasName = 'lz-corp-<workload>-${environment}'`,
the four environment flags, `parTags`, and the fixed values as they are.

## What changes in Forge

1. **Template format** (`internal/template`): `type`, `target`, and on a
   parameter `label`, `placeholder`, `group`, `min`/`max`, the types `list`
   and `team`, and a `title` function for paths (`corp` gives `Corp`).
   `forge template lint` checks them. The protos get the same
   fields; the dashboard shows `label` instead of the name when there is one.
2. **API**: `TemplateService.ApplyTemplate`, an operation with the steps
   fetch, render, branch, push, pull request. It refuses when the file already
   exists on the target's default branch (`already_exists`), so applying twice
   does not overwrite a landing zone. Audited as `template.apply`.
3. **CLI**: `forge template apply`.
4. **Dashboard** (`forge-web`): the create form handles a file template: the
   new input types, labels, groups, and the pull request link at the end.
5. **Templates** (`forge-templates`): `landing-zone`, a minor release.

## Open questions

- **Online:** the target path is `landingZones/Online/` and the file says
  `using '../../main_online.bicep'`. You add `main_online.bicep` and the
  folder in `showmino/landingzone`.
- **vNet region:** the picture lets you tick both regions, but the file has one
  `parVirtualNetworkLocation`. Proposed: one choice.
- **Entra ID user:** `main_corp.bicep` has no parameter for it, and your example
  leaves it out. Proposed: leave it out until the Bicep takes it.
- **Deploy on merge:** `portal.yaml` only runs for
  `lz-corp-matrix-vending.bicepparam`, so a new file is not deployed when its
  pull request merges. That is for `showmino/landingzone` to change, not Forge.
