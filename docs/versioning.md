# Versioning

Isola follows [Semantic Versioning](https://semver.org/) on a single `1.x` line.
A version is decided by judgement at release time — nothing in the source tree is
bumped — and the git tag is the only place it lives.

## Where the version lives

- The annotated tag `vX.Y.Z` is the source of truth. `release.yml` stamps the
  pushed tag minus its `v` into `pkg/version.Version`.
- `backend/scripts/build.sh` derives the same fields from
  `git describe --tags --always --dirty`, and `backend/Dockerfile` from
  `git describe --tags --always`. Both therefore report the nearest tag, plus
  the commits since it when `HEAD` is untagged — `describe` yields
  `v1.5.2-3-gabc1234` and the stamp drops the leading `v` — and `build.sh`
  appends `-dirty` for a modified tree.
- A bare `go build` reports `dev`/`unknown`, and `frontend/package.json` keeps an
  inert `0.0.0`. Both are deliberate defaults: do not bump them per release.

`GET /api/config`, the landing-page footer and the viewer's debug info report
the stamped values; see [Binaries → Version Stamps](binaries.md#version-stamps).

## Choosing the number

Ask what a user, operator, or API client can observe after upgrading. The
largest impact in the release decides the bump; everything else rides along.

| The release… | Bump | Examples |
| :--- | :--- | :--- |
| Removes or renames something consumers rely on: a route, response field, error code, config key, env var, MCP tool, WebSocket input message, release asset name — or stops supporting a platform it used to support | **MAJOR** | Removing the deprecated `POST /api/sessions/{id}/share` route or a field from the network dump; renaming the `isola-<os>-<arch>-v<version>.tar.gz` assets that `scripts/install.sh` downloads; dropping an architecture from the release matrix |
| Adds a capability that did not exist | **MINOR** | A new endpoint or MCP tool, a new config key, a viewer feature, a new input message type |
| Corrects existing behaviour: bug fixes, the output of an existing feature, performance, dependency refresh (including security fixes), docs | **PATCH** | Regrouping the resource graph by page; the arrival-window fix for the stream signal |
| Changes nothing a user can observe (refactor, tests, CI, internal docs) | **none** | No release, no tag |

Rules of thumb:

- **Reworking an existing feature is a fix, not a feature.** If the capability
  already existed and now works correctly or more clearly, it is PATCH — even
  when the output looks different.
- **Contract changes weigh more than intent.** Changing a default, an error
  code, a field name, or an isolation guarantee is at least MINOR, because
  clients key off it. Removing it is MAJOR.
- **Raising a floor is a compatibility break.** A new minimum Docker, Node,
  Chromium, kernel or browser version that excludes a host which previously
  worked is MAJOR; one that only excludes hosts already outside the documented
  requirements is MINOR.
- **Tags are immutable.** Never move, reuse, or delete a published tag. A broken
  release is fixed by the next PATCH.
- **Release on demand, one release per batch** of user-facing changes. Security
  fixes do not wait for a feature.

The date in the CHANGELOG heading is the day the tag is pushed.

## Cutting a release

1. Every user-facing change already has an entry under `## [Unreleased]` in
   [`CHANGELOG.md`](../CHANGELOG.md). Keep entries to what changed for the
   reader — one or two lines, no implementation narrative.
2. Pick `X.Y.Z` with the table above and commit `chore(release): X.Y.Z` that
   inserts `## [X.Y.Z] - YYYY-MM-DD` between `## [Unreleased]` and the entries.
   The entries stay where they are. In the link block at the bottom, add
   `[X.Y.Z]` and re-point `[Unreleased]` at `compare/vX.Y.Z...HEAD`.
3. Preview the body `release.yml` will publish — it runs this exact script, so a
   wrong section is otherwise only visible to users:
   ```bash
   node scripts/changelog-section.mjs vX.Y.Z
   ```
   Run it after step 2: before the section exists the script falls back to
   `## [Unreleased]` and warns.
4. Push both, tag last:
   ```bash
   git push origin main
   git tag -a vX.Y.Z -m "isola vX.Y.Z"
   git push origin vX.Y.Z
   ```
5. The `main` push starts `ci`; the tag starts `release` (four tarballs plus
   `checksums.txt`) and `publish-browser-image` (multi-arch sandbox image, the
   long pole at roughly ten minutes). All three must be green and the release
   must carry all five assets before announcing:
   ```bash
   gh run list --branch vX.Y.Z                     # release + publish-browser-image
   gh run list --workflow ci.yml --branch main --limit 1
   gh release view vX.Y.Z --json assets
   ```

Three names are load-bearing: `.github/workflows/release.yml` keys off the `v*`
tag shape, the `cmd/single-user` package name, and the `pkg/version` ldflags
target. Renaming any of them changes what a tag does.
