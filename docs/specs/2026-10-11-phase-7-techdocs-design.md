# Phase 7: TechDocs

Date: 2026-10-11. Status: design, awaiting the owner's approval.

## Goal

Documentation as code: a service's docs are Markdown files in its repo, next to the code, and everyone reads them in the Forge dashboard, in one place and in one look. There is no build step and nothing to publish: a page is current the moment its change merges.

Exit gate (from `docs/index.md`): a developer adds a Markdown page to a service's repo and merges it, and a reader finds and reads it in the dashboard within a minute, without a terminal and without a build.

## Decisions

| Decision | Choice |
| --- | --- |
| Where docs live | A `docs/` folder of Markdown in each service's repo. `docs/index.md` is the start page; a repo without it has no docs |
| Page order and titles | An optional `docs:` list in `forge.yaml` names the pages in order. Without it: `index.md` first, then the other files by path. A page's title is its first `#` heading, else its file name |
| How pages are made | forge-api reads the files from GitHub and the dashboard renders the Markdown. No MkDocs, no CI step, no storage |
| Which version | The repo's default branch. Every page says which commit it shows and when it last changed |
| Who may read | Anyone with `services.read`, as for the catalog. Nothing in this phase writes, so nothing is audited |
| First version | A Docs tab on every service page, a Docs page listing every service that has docs, `forge docs` in the CLI, starter docs in both templates. Search is this phase's last milestone |

Not in this phase: documentation that belongs to no service (handbooks, platform guides), MkDocs sites and plugins, docs per release or per environment, editing in the browser, PDF export.

## forge.yaml

```yaml
docs:
  - title: Overview        # optional; default the page's first heading
    path: index.md         # under docs/
  - path: runbook.md
  - path: api/errors.md
```

Unknown files are still readable by their address; the list only sets the order and the titles of the side menu. `forge docs lint` reports a listed page that does not exist.

## API

A new `DocsService`, read-only, all behind `services.read`:

| Call | Returns |
| --- | --- |
| `ListDocSets` | The services that have docs: service, team, number of pages, commit, when it last changed |
| `GetDocSet(service)` | The pages in order (path, title), the commit, the repo address |
| `GetDocPage(service, path)` | The page's Markdown, its title, the commit, a link to edit it on GitHub |
| `GetDocAsset(service, path)` | An image a page uses, with its media type. The repos are private, so the browser cannot fetch it from GitHub itself |
| `SearchDocs(query)` (milestone 3) | Matching pages with the lines that matched |

How forge-api reads: with the GitHub App it already has (contents: read). Per repo it asks GitHub for the default branch's head commit at most once a minute; the list of files and every page read are kept in memory until that commit changes. `ListDocSets` uses the same cache, refreshed in the background, so the Docs page does not wait on GitHub for every service.

Limits, because a repo's content is not trusted: only paths under `docs/`, no `..`; pages are `.md`, at most 1 MiB; assets are png, jpeg, gif, webp or svg, at most 5 MiB; at most 500 pages per service.

## CLI

| Command | Does |
| --- | --- |
| `forge docs list` | The services that have docs |
| `forge docs get <service> [page]` | Prints a page's Markdown (the start page by default); `-o json` for the page with its metadata |
| `forge docs lint [dir]` | In a repo: checks `docs/index.md` exists, that pages in `forge.yaml` exist, and that links and images between pages resolve. Needs no API |

## Dashboard

- **Service page:** a Docs tab, shown when the service has docs. Left: the pages. Middle: the page. Right: the headings of this page. A line under the title names the commit and when it changed, with "Edit on GitHub".
- **Docs page** (sidebar, under Build): every service that has docs, with its team and when its docs last changed; from milestone 3, the search box.
- **Rendering:** CommonMark with tables, task lists, footnotes and highlighted code, in the Forge Console type and colours, light and dark. Links between pages become dashboard links, images are fetched through the API, links to other sites open as normal links. Raw HTML in a page is shown as text, never run: a docs page cannot add scripts, frames or styles to the dashboard.
- As everywhere, the page shows its CLI command (`forge docs get payments runbook.md`).

## Templates

Both templates get `docs/index.md` (what the service is, how to run it, how it ships) and `docs/runbook.md` (a skeleton: alerts, dashboards, common fixes), released as the next minor version. An existing service gets docs by adding the folder.

## Milestones

| Milestone | Scope |
| --- | --- |
| 1 | `DocsService` without search, the cache, the `docs:` list in `forge.yaml`, `forge docs list/get/lint`. Live on k3s |
| 2 | The Docs tab and reader, images, the Docs page, starter docs in the templates |
| 3 | Search across all docs, diagrams (Mermaid) in pages, empty and error states, the exit gate |

## Testing

Short and fast, as in Phase 6.

- forge: unit tests for the page order and titles, the path and size limits, the cache (a moved head refreshes, an unchanged one does not ask GitHub again) against the fake GitHub; CLI goldens; `forge docs lint` on a sample folder.
- forge-web: render tests for the reader against Connect's in-memory transport: links rewritten, raw HTML shown as text, a missing page, a service without docs.
- One check by hand on k3s at the end of each milestone.
