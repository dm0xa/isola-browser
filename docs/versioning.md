# Versioning

Isola follows [Semantic Versioning](https://semver.org/). A version is decided by
judgement at release time — nothing in the source tree is bumped — and the git
tag is the only place it lives.

## Where the version lives

- The annotated tag `vX.Y.Z` is the source of truth. `release.yml` stamps the
  pushed tag minus its `v` into `pkg/version.Version`.
- Docker and `backend/scripts/build.sh` read `git describe --tags`, so a build
  from `main` reports the nearest tag plus the commits since it
  (`1.5.2-3-gabc1234`, `-dirty` when the tree is modified).
- A bare `go build` reports `dev`/`unknown`, and `frontend/package.json` keeps
  an inert `0.0.0`. Both are deliberate defaults: do not bump them per release.

`GET /api/config` and the embedded UI report the stamped values; see
[Binaries → Version Stamps](binaries.md#version-stamps).

## Choosing the number

Ask what a user, operator, or API client can observe after upgrading. The
largest impact in the release decides the bump; everything else rides along.

| The release… | Bump | Examples |
| :--- | :--- | :--- |
| Removes or renames something clients rely on: a route, response field, error code, config key, env var, MCP tool, WebSocket input message, release asset name, or a supported platform | **MAJOR** | Dropping `POST /api/sessions` legacy fields; raising the Docker or kernel floor to a version that excludes a supported host |
| Adds a capability that did not exist | **MINOR** | A new endpoint or MCP tool, a new config key, a viewer feature, a new input message type |
| Corrects existing behaviour: bug fixes, output of an existing feature, performance, dependency refresh, docs | **PATCH** | Regrouping the resource graph by page; the arrival-window fix for the stream signal |
| Changes nothing a user can observe (refactor, tests, CI, internal docs) | **none** | No release, no tag |

Rules of thumb:

- **Reworking an existing feature is a fix, not a feature.** If the capability
  already existed and now works correctly or more clearly, it is PATCH — even
  when the output looks different.
- **Contract changes weigh more than intent.** Changing a default, an error
  code, a field name, or an isolation guarantee is at least MINOR, because
  clients key off it. Removing it is MAJOR.
- **Security fixes are PATCH** unless the fix itself requires a breaking change.
- **A dependency bump is PATCH.** If it raises a floor (Node, Chromium, Docker,
  kernel, a browser's minimum version) it is at least MINOR, and MAJOR when it
  drops a platform that previously worked.
- **Tags are immutable.** Never move, reuse, or delete a published tag. A broken
  release is fixed by the next PATCH.
- The date in the CHANGELOG heading is the day the tag is pushed.

## Cutting a release

1. Every user-facing change already has an entry under `## [Unreleased]` in
   [`CHANGELOG.md`](../CHANGELOG.md). Keep entries to what changed for the
   reader — one or two lines, no implementation narrative.
2. Pick `X.Y.Z` with the table above, then preview the published body:
   ```bash
   node scripts/changelog-section.mjs vX.Y.Z
   ```
   `release.yml` publishes exactly this output as the GitHub release body, so a
   wrong section is only visible to users otherwise.
3. Commit `chore(release): X.Y.Z` that inserts `## [X.Y.Z] - YYYY-MM-DD` between
   `## [Unreleased]` and the entries. The entries stay where they are. In the
   link block at the bottom, add `[X.Y.Z]` and re-point `[Unreleased]` at
   `compare/vX.Y.Z...HEAD`.
4. Tag and push both:
   ```bash
   git push origin main
   git tag -a vX.Y.Z -m "isola vX.Y.Z"
   git push origin vX.Y.Z
   ```
5. The tag starts three workflows — `ci`, `release` (four tarballs plus
   `checksums.txt`) and `publish-browser-image` (multi-arch sandbox image, the
   long one). All three must be green before announcing.

Two names are load-bearing for the tag itself: `.github/workflows/release.yml`
keys off the `v*` tag shape and the `cmd/single-user` package name. Renaming
either silently changes what a tag does.
