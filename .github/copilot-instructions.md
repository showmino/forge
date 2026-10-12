# Instructions for GitHub Copilot

The instructions for this repository are in [`AGENTS.md`](../AGENTS.md) at the
repo root, shared by every coding agent so that there is one copy. Read it
before you change anything, then:

- [`docs/agents/status.md`](../docs/agents/status.md): what is built, what is live, what is left.
- [`docs/agents/verifying.md`](../docs/agents/verifying.md): the runbook for the cluster.
- [`docs/index.md`](../docs/index.md): the approved design.

The rules in `AGENTS.md` under "Rules" are the owner's and are not optional:
the cluster is production, changes to it and deletions need the owner's OK,
merge only when every check has passed, never print a secret.

Do not add project knowledge to this file. Put it in the files above, in the
same pull request as the change it describes.
