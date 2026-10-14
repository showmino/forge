# File templates: add a file to an existing repo

Oct 14, 2026 · Showmino · Built

A service template makes a new repo. A landing zone is one `.bicepparam`
file in `showmino/landingzone`, so there is a second kind of template: one
that renders files and opens a pull request that adds them to a repo that
already exists. The first one is `landing-zone`, with the form of the old
portal.

## Decisions

| Question | Decision |
| --- | --- |
| What it does | forge-api renders the skeleton, commits the files on top of the target's default branch to a new branch `forge/<template>/<name>` and opens a pull request. Merging is a person's call in that repo; the repo's own workflows deploy it. |
| How it is started | The same `CreateService` call, `forge create` and dashboard form as a service template. `<name>` (the service name field) names the branch and is `.Service.Name` in the skeleton; the team is `.Service.Team`. No new RPC or command. |
| Where it is declared | `template.yaml`: `type: file` and `target: owner/repo`. Without `type`, a template is a service template. |
| Where the files go | The skeleton's paths, rendered as always, relative to the target repo's root. |
| Catalog | Nothing is registered: a landing zone is not a Forge service. |
| Safety | The target must be in the org. Nothing is written when a file is on the default branch already (`already_exists`), so a second create never overwrites a landing zone; a retry whose branch has an open pull request gets that pull request, and a branch whose pull request GitHub refused is deleted again. A file template may not write under `.git` or `.github` (lint and API refuse it: workflows there would run with the target's secrets). Every parameter value is one line; the team must be a GitHub team slug. Text goes into a file through an escaping function such as `bicepString`. |
| GitHub access | The Forge GitHub App, with Contents and Pull requests write on the target repo (given for `showmino/landingzone`). |

## `template.yaml`

```yaml
type: file
target: showmino/landingzone
parameters:
  - name: environments
    label: Environments          # what a form shows instead of the name
    type: list                   # several of values; given comma-separated, renders as a list
    values: [prod, staging, dev, test]
    default: [prod]
  - name: ipAddresses
    label: IP addresses needed
    type: int
    min: 1                       # bounds of an int
    max: 1024
  - name: costCenter
    label: Cost center
    type: string
    group: Tags                  # a heading the form shows it under
    placeholder: "60168"         # an example in the empty input
```

Three render functions come with it: `has` (`{% has .Params.environments "prod" %}`
gives `true` or `false`), `title` (`corp` gives `Corp`, for folder names)
and `bicepString` (escapes `\`, `'` and `${` for a single-quoted Bicep string).
The template has to use an escaping function for free text: a value is not
escaped by itself.

## The stream

Steps `fetch`, `render`, `pull_request`; the last message is a
`ProposedChange` (repo, branch, pull request URL, files). `validate_only`
returns a `CreatePlan` whose `repo_url` is the target repo. The CLI prints
the pull request's URL; `--dry-run` prints "Would add N files to <repo>".

## Not built

- A `team` parameter type (a GitHub team picked from a list): the form's
  owner team is that list already, and is `.Service.Team`.
- Labels and groups in `forge template get`'s table; JSON has them.
