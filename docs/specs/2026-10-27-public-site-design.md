# forgegate.io: Forge's public site

Approved by the owner on 2026-10-27 (the three choices below).

## What

The service `forgegate` (showmino/forgegate, production at
`https://forgegate.io`) becomes Forge's public site: a landing page at `/`
and Forge's documentation under `/docs`, built with Docusaurus. The repo
is public (since 2026-10-27).

## Choices

1. **The docs move.** forgegate's `docs/` becomes the one source of Forge's
   user documentation, written for anyone who runs or uses a Forge
   (`example.com`, `my-org`), not for showmino alone. It replaces
   `showmino/techdocs` `docs/forge/`, which keeps one page, "Forge at
   showmino": the addresses, the login, the vault and the domains of
   showmino's install, linking to forgegate.io. AGENTS.md's rule "every
   feature is documented" points to forgegate.
2. **Landing page and docs.** `/` says what Forge is and how to install it
   and links to GitHub; `/docs` is the documentation, with Docusaurus's local
   search (no outside service). No blog.
3. **No database.** forgegate's `main` PostgreSQL resource (staging) is
   removed: a static site needs none.

## How

- Docusaurus 3, TypeScript config, `markdown.format: detect`: `.md` files
  are plain CommonMark, so the same files read correctly in Forge's
  dashboard (its Docs tab renders forgegate's `docs/` too, and shows raw HTML
  as text). No MDX, no admonitions, no HTML in the docs.
- `docusaurus build` fails on broken links; it is the `test` job.
- The image is the static build behind nginx-unprivileged, non-root, with a
  read-only root filesystem: everything nginx writes is in `/tmp`.
  `/api/healthz` is a static file, so the health check stays.
- The repo is public, and the self-hosted runners refuse public repos, so CI
  runs on `ubuntu-latest` and the deploy job uses the public API address
  (`https://forge-api.showmino.com`), as forge-web does.
- Pages that describe what only showmino's `deploy/k3s` install has (public
  domains through Cloudflare, resources) say so, and what the chart offers
  instead (internal addresses under `apps.domain`). Secrets and previews say
  how the chart switches them on.
- The CLI's help examples that named showmino's own services and vault
  (`forge service address`, `forge create --secret`) use `example.com` and
  `vaults/Apps`, so the command reference stays the output of `--help`.

## Order

1. forgegate pull request: the site and the moved docs. Merging deploys
   staging; promoting to production needs the owner's OK.
2. techdocs pull request: `docs/forge/` replaced by "Forge at showmino",
   merged after forgegate's.
3. This repo: AGENTS.md's docs rule, the chart's pointers to the docs and the
   CLI's help examples, with this spec.
4. The database: `forge resource plan` without it, shown to the owner,
   applied with `--allow-destroy` after the OK.
