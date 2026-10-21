# Changelog

Notable changes to isola, in
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. `release.yml`
publishes the section matching the tag, so keep `## [Unreleased]` current
instead of writing notes at tag time.

## [Unreleased]

### Added

- `GET /api/sessions/{id}/graph.png` and the `resource_graph` MCP tool render
  the captured network as a resource-graph PNG on the server: `theme`
  (`dark`/`light`), `direction`, `expand` (`origin`/`all`/`collapsed`), resource
  type and failed-only filters, and a 1–4× raster scale.

### Changed

- The resource graph's single collapse button is now a collapse-all /
  expand-all icon pair; both clear the pin and refit the graph.
- PNG export frames the graph's measured bounding box instead of its nominal
  layout size, so labels and selection rings can no longer be cropped at the
  right or bottom edge, and the Isola watermark now sits in the bottom-right
  padding instead of being stamped over the nodes. The image also carries a
  caption with the origin host, request/host/resource counts, any active
  filters and the capture time in UTC.

## [1.1.1] - 2026-10-21

### Added

- Configurable network capture window: `session.network_log_max_entries`
  (`ISOLA_NETWORK_LOG_MAX_ENTRIES`), default 1000, capped at 10000, replacing
  the hard-coded 400. `/api/config` publishes it; the panel and monitor trim to
  it, including replayed `net_init` snapshots.
- Copy a request's URL from the resource graph tooltip.

### Changed

- Resource graph is summary-first: hosts collapse into cards that open into
  resource-type groups with counts and failure badges, and a group reveals a
  bounded preview plus "show all". Opening a group pins its path while siblings
  fade; clicking a resource pins it, and the tooltip's funnel applies the
  domain filter. The graph opens fitted to the whole picture with only the
  opened page's host expanded; type groups start collapsed and expansion is
  not restored across page loads.
- Graph readability: sibling runs wrap, labels become class-coloured bars below
  50% zoom, 25/50/75/100% presets, draggable minimap, arrow-key panning, panel
  filters, a failed-only chip, and "fit" that collapses groups when the result
  would be unreadable.
- Versions come from git tags only: `pkg/version` defaults to `dev`,
  `frontend/package.json` to `0.0.0`, and `build.sh` strips the leading `v`. A
  bare `go build` reports `dev`/`unknown` by design.

### Fixed

- The network monitor kept a row per snapshot of a request, leaving the first at
  `status 0`, `size 0` and an empty `sha1`. Snapshots now merge in place.
- The network monitor's copy button always copied requests; it now copies the
  active tab as headered TSV with ISO 8601 UTC times and raw byte counts.

## [1.1.0] - 2026-10-19

### Added

- Floating chrome logo in the viewer and share toolbars.
- Manual install path without Docker Compose.

### Changed

- Electron 34 (Chromium 132) to 44 (Chromium 152); the image runs
  `node node_modules/electron/install.js`, since v44 ships no `scripts` field.

### Fixed

- Health checker killed live sessions after three failed `docker inspect`
  probes; inconclusive probes are now skipped and only an absent container
  fails.
- Idle reaper killed active sessions; `keepalivePing` now records activity.
- Fresh tabs flashed white and reported stalls; new tabs hold 3 s behind
  `surfaceEstablished`, empty documents are normalised, and `ts_ns` uses
  wall-clock UTC.
- Page canvas is white (`#ffffff`); theme colour only applies to an empty
  document.
- Address bar kept the previous tab's URL.
- App shell had no cache policy; hashed assets are now immutable for a year, the
  rest `no-cache`.
- Per-session `view_password` cache key leaked on the orphan sweep.
- Launch overlay could show twice.

## [1.0.1] - 2026-10-16

### Changed

- CI builds the sandbox image; the installer derives its version at install
  time.

## [1.0.0] - 2026-10-16

Initial release: a Go control plane that boots a disposable Docker sandbox
running offscreen Chromium.

[Unreleased]: https://github.com/dm0xa/isola/compare/v1.1.1...HEAD
[1.1.1]: https://github.com/dm0xa/isola/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/dm0xa/isola/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/dm0xa/isola/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/dm0xa/isola/releases/tag/v1.0.0
