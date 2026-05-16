# CLAUDE.md

Project-specific context for Claude Code in `hashpoint-plugin-soggl`.

## What this project is

`hashpoint-plugin-soggl` (the repository / Go module) ships the
**`soggl`** Hashpoint plugin — a bridge to the internal **Soggl**
application. It implements the `tag_provider` capability: it supplies
tags and orders to the Hashpoint host by calling the Soggl API.

The plugin runs as a separate subprocess; the host communicates with it
over `net/rpc` multiplexed via `hashicorp/go-plugin`.

### Plugin identity vs. repository name

Two names coexist and must NOT drift:

| Concept                     | Value                                          |
|-----------------------------|------------------------------------------------|
| GitHub repository / Go module path | `hashpoint-plugin-soggl`                |
| Source directory under `cmd/`     | `cmd/hashpoint-plugin-soggl/`           |
| Plugin identity (everywhere else) | **`soggl`**                             |

"Plugin identity" means **all** of the following at the same time, and
they MUST agree exactly:

- `manifest.toml` → `name = "soggl"`
- `internal/plugin.Name` constant (returned in `Metadata().Name`)
- `.goreleaser.yml` → `project_name`, `builds[].id`, `builds[].binary`,
  and `archives[].ids` → all `soggl`
- The release asset filename → `soggl_<ver>_<os>_<arch>.zip` (+ `.sha256`)
- The single top-level directory inside that zip → `soggl/`
- The bundled binary → `soggl/soggl.exe`
- The catalog entry in `DustHoff/hashpoint-plugin-manager`'s `repo.json`
  → `"name": "soggl"`

The plugin manager (`DustHoff/hashpoint-plugin-manager`) resolves the
asset name with the pattern `{name}_{version}_{os}_{arch}.zip` where
`{name}` is the **catalog entry name** — divergence surfaces at install
time as `asset "<expected>" not found in release vX.Y.Z`. The repo /
module path stays the long form for historical reasons; only the ldflag
that injects `pluginVersion` references it
(`-X github.com/dusthoff/hashpoint-plugin-soggl/internal/plugin.pluginVersion=…`).

## Language and build constraints

- **Native Go only — no cgo.** Always build with `CGO_ENABLED=0`.
- Target artifact: a single Windows executable that ships alongside a
  `manifest.toml`.
- CI builds run in GitHub Actions. Do not introduce build steps that require
  non-Go toolchains.

## SDK contract

Built against the Hashpoint SDK:

- Repo: `https://github.com/DustHoff/hashpoint`
- SDK source: `plugin/sdk/sdk.go` (import path
  `github.com/dusthoff/hashpoint/plugin/sdk`)
- Plugin docs: `docs/plugins/README.md`

Interfaces this plugin must implement (from `sdk.go`):

- `sdk.Plugin` — `Init(ctx, host)`, `Metadata(ctx)`, `Configure(ctx, cfg)`
- `sdk.TagProviderHandler` —
  - `ListTags(ctx) ([]sdk.ImportedTag, error)`
  - `ListOrders(ctx) ([]sdk.Order, error)`

Entry point: `func main() { sdk.Serve(impl) }`. `sdk.PluginMap` auto-registers
the `tag_provider` adapter when the implementation satisfies
`TagProviderHandler`.

Capabilities declared in `Metadata()` must include `sdk.CapTagProvider` (and
only that, unless a new capability is added — see "Out of scope").

### Before implementing any feature or fix

**Always check the SDK first.** Before writing code, re-fetch the SDK and
verify:

1. `HostAPIVersion` — if it changed, this plugin's major version must follow
   (see "Versioning").
2. The signatures of `TagProviderHandler`, `Plugin`, and any `HostAPI` methods
   this plugin uses (`RequestEntraToken`, `RedeemSecret`, `Log`,
   `PublishTags`).
3. New `HostAPI` capabilities that may simplify the task at hand.

Do not assume the SDK shape from memory or prior conversations — re-read
`sdk.go`.

## Configuration

Configured via Hashpoint's Plugins tab; the host delivers values through
`Configure(ctx, sdk.PluginConfig)` in `Fields` (text/bool) and `Secrets`
(opaque handles redeemed via `host.RedeemSecret`).

This plugin's config fields:

| Key           | Type | Purpose                                                                              |
|---------------|------|--------------------------------------------------------------------------------------|
| `entra_scope` | text | Scope used when requesting the Entra access token for the Soggl API.                 |
| `soggl_host`  | text | Base hostname / URL of the internal Soggl service (e.g. `https://soggl.internal`).   |

No secret fields today — authentication uses the host-provided Entra token,
not a stored credential. If a future feature needs a stored secret, add it as
`FieldTypePassword` and redeem via `HostAPI.RedeemSecret`.

## Authentication to Soggl

Soggl is called with an Entra ID access token obtained from the host on demand:

```go
token, expiresAt, err := host.RequestEntraToken(ctx, []string{cfg.EntraScope})
```

- The token comes from the Hashpoint host's Entra session. The plugin does
  **not** run its own MSAL/OAuth flow.
- Send as `Authorization: Bearer <token>` to the Soggl API rooted at
  `cfg.SogglHost`.
- Treat `sdk.ErrEntraNotAvailable` as a transient/config error surfaced
  through logs, not a panic. Honour the returned `expiresAt` to avoid
  hammering the host on every call.

## Versioning (SemVer)

Version components follow Semantic Versioning and are derived automatically
from Conventional-Commit prefixes by the release workflow on push to `main`:

- **MAJOR** — `feat!:` / `fix!:` / `chore!:` or a `BREAKING CHANGE:` footer.
- **MINOR** — `feat:` commit.
- **PATCH** — `fix:` commit.
- Other prefixes (`chore:`, `ci:`, `docs:`, `test:`, `refactor:`, …) do **not**
  bump the version. A push containing only such commits is a no-op for the
  release workflow.

**Plugin-MAJOR must mirror SDK-MAJOR.** The plugin's MAJOR version equals
`sdk.HostAPIVersion` (see `require github.com/dusthoff/hashpoint vX.Y.Z` in
`go.mod`). Today `HostAPIVersion = 1`, so plugin versions are `1.x.y`. If
the SDK ever bumps to v2, the next plugin release MUST include a `feat!:` /
`fix!:` / `chore!:` commit (or `BREAKING CHANGE:` footer) so the plugin-major
follows — even if the plugin's own changes are not breaking. Rationale: the
SDK major defines the plugin contract (`HostAPIVersion`, interfaces); a
mismatch would render the version number useless as a compatibility signal.

**The git tag is the single source of truth for the version.** In-source
version values are placeholders and MUST NOT be edited by hand:

- `pluginVersion` in `internal/plugin/plugin.go` stays at `"dev"`.
- `version` in `manifest.toml` stays at `"0.0.0-dev"`.

GoReleaser injects the real version at release time:

- `pluginVersion` via `-X` ldflag (see `.goreleaser.yml` → `builds.ldflags`).
- `manifest.toml.version` via `scripts/inject-manifest-version.sh`, run as a
  `before.hooks` step. The script writes a version-substituted
  `manifest.toml.versioned` (repo root, gitignored); the archive references
  it as `manifest.toml`. The substitution happens outside `dist/` because
  GoReleaser's "ensuring distribution directory" pipe runs after
  `before.hooks` and rejects a non-empty `dist/` even with `--clean`.

Local `go build` / `go test` therefore show `"dev"` as the version. Release
binaries show the tag version. `TestMetadata_VersionIsPlaceholder` in
`internal/plugin/plugin_test.go` blocks accidental hardcoding of the
placeholder.

The `api_version` field in `manifest.toml` IS edited by hand and MUST equal
`sdk.HostAPIVersion`. `TestManifestApiVersionMatchesSDK` guards this.

## Release workflow

- Every change ships as a **PR** against `main`. No direct commits to the
  default branch.
- Commits MUST follow Conventional Commits (`feat:`, `fix:`, `chore:`,
  `ci:`, `docs:`, `refactor:`, …). The PR title (squash-merge subject) is
  what the release workflow inspects, so the PR title MUST follow
  Conventional Commits.
- On every push to `main`, `.github/workflows/release.yml`:
  1. Runs `mathieudutour/github-tag-action`, which inspects commits since
     the last tag, decides the bump, and pushes the new `vX.Y.Z` tag. If
     no bumpable commit has landed, no tag is created and the workflow
     ends.
  2. Runs GoReleaser against the new tag, building the Windows binary,
     injecting the version, and publishing a GitHub Release with the
     archive + `.sha256` sidecar.

  A baseline tag is **not** seeded automatically: the default
  `GITHUB_TOKEN` does not carry the `workflows` permission and is refused
  by the GitHub API when it tries to push a tag onto a commit that
  contains `.github/workflows/*` files (which our initial commit does).
  The baseline `v1.0.0` is therefore set **once manually** by the
  maintainer from a workstation that has `workflows`-scoped credentials:

  ```sh
  git tag v1.0.0 <initial-or-current-main-sha>
  git push origin v1.0.0
  ```

  After that the workflow runs unattended on every push.
- The workflow can also be triggered manually via `workflow_dispatch`; in
  that mode it forces a **PATCH** bump so a release-tooling-only change can
  produce a fresh release without a bumpable commit.
- **Every release artifact MUST ship with a SHA256 checksum as a `.sha256`
  sidecar file.** Configured in `.goreleaser.yml` under `checksum`:
  - `split: true` (one sidecar per artifact, not a combined checksums file)
  - `algorithm: sha256`
  - `name_template: "{{ .ArtifactName }}.sha256"`

  Example:

  ```yaml
  checksum:
    split: true
    algorithm: sha256
    name_template: "{{ .ArtifactName }}.sha256"
  ```

  A release without per-artifact `.sha256` sidecars is invalid.

## Target project layout

The repo is currently empty. When initializing, prefer:

- `go.mod` with the module path matching the GitHub repo URL.
- `cmd/hashpoint-plugin-soggl/main.go` — entry point, calls `sdk.Serve`.
- `internal/soggl/` — Soggl HTTP client (stdlib `net/http`).
- `internal/plugin/` — `Plugin` + `TagProviderHandler` implementation.
- `manifest.toml` at repo root.
- `.github/workflows/` — CI build + release pipelines.

## Out of scope

- cgo, any C/C++ dependencies, or non-Go build tools.
- Capabilities other than `tag_provider`. Adding another capability (e.g.
  `process_autotag`) is a feature → minor bump, and it must be reflected in
  `Metadata().Capabilities`.
- Storing Soggl credentials in plugin config — auth is always via the host's
  Entra token.
