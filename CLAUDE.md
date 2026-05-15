# CLAUDE.md

Project-specific context for Claude Code in `hashpoint-plugin-soggl`.

## What this project is

`hashpoint-plugin-soggl` is a Hashpoint plugin that bridges Hashpoint with the
internal **Soggl** application. It implements the `tag_provider` capability —
it supplies tags and orders to the Hashpoint host by calling the Soggl API.

The plugin runs as a separate subprocess; the host communicates with it over
`net/rpc` multiplexed via `hashicorp/go-plugin`.

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

Version components are driven by SDK and change type:

- **MAJOR** = the `sdk.HostAPIVersion` this plugin is built against. Today
  `HostAPIVersion = 1`, so plugin versions are `1.x.y`. A bump of the SDK API
  version forces a major bump here.
- **MINOR** — incremented on every new feature.
- **PATCH** — incremented on every bug fix.

The version string lives in three places that **must** stay in lockstep — a
release is only valid if all three agree:

1. **Git tag** — the release tag (e.g. `v1.4.2`) pushed to GitHub.
2. **`manifest.toml`** — `version = "x.y.z"` and `api_version = <HostAPIVersion>`.
3. **`Metadata()` return** — `Version: "x.y.z"`, `APIVersion: sdk.HostAPIVersion`.

Any drift (tag without matching manifest, manifest without matching
`Metadata()`, etc.) is a release blocker. CI should fail the build if these
diverge.

## Release workflow

- Every change ships as a **PR** in the GitHub repository. No direct commits
  to the default branch.
- A PR is either a feature (minor bump) or a fix (patch bump); the PR
  description states which and updates `manifest.toml` + `Metadata()`
  accordingly.
- GitHub Actions builds the release artifact (plugin executable +
  `manifest.toml`) on tag push. The tag, `manifest.toml` `version`, and
  `Metadata().Version` must all match — see "Versioning".
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
