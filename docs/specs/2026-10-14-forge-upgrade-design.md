# `forge upgrade`

Oct 14, 2026 · Showmino · Built

The forge repo is private, so a plain download, Homebrew or winget would need
a GitHub token on every machine. `forge upgrade` gets the CLI from the Forge
API instead, which can read the releases as the Forge GitHub App.

## Decisions

| Question | Decision |
| --- | --- |
| Which version | The API's own version (`GetServerInfo`), not the newest release: the CLI and the API then always match, which is what the version-skew check wants. |
| Where the builds come from | forge-api proxies the release assets: `GET /cli/releases/v<X.Y.Z>/<file>`, where file is `checksums.txt` or a CLI archive (`forge_<v>_<linux\|darwin>_<amd64\|arm64>.tar.gz`, `forge_<v>_windows_<amd64\|arm64>.zip`). Any other name is 404. No login, like `/cli/forge-linux-<arch>`: the binaries are not secret. The approved first idea, putting every build into the forge-api image, does not work with goreleaser's `dockers_v2`: its build context holds only the image's own Linux platforms. |
| Which repo | `--cli-repo` / `FORGE_CLI_REPO`, default `showmino/forge`; the App needs read access to its releases (Contents: read). |
| Integrity | The archive's SHA-256 must match the release's `checksums.txt`; nothing is replaced otherwise. The cosign signature on `checksums.txt` is not checked: both come over TLS from the same API. |
| Replacing itself | The binary is written next to the running one as `<exe>.new` and renamed over it. Windows cannot replace a running program but can rename it, so there the old one becomes `<exe>.old` (removed by the next upgrade). A path the user may not write gives a hint to use sudo. |
| Flags | `--check` only says whether the API's version differs. |

The first version with `forge upgrade` has to be installed by hand once on
each machine; after that, `forge upgrade` is enough.
