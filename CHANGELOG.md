# Changelog

All notable changes to isola are documented in this file. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The `release` workflow publishes the section matching the pushed tag as the
GitHub release body, so keep `## [Unreleased]` current as changes land rather
than reconstructing it at tag time.

## [Unreleased]

### Added

- Configurable network capture window: `session.network_log_max_entries`
  (`ISOLA_NETWORK_LOG_MAX_ENTRIES`) replaces the hard-coded 400-request buffer,
  defaulting to 1000 and capped at 10000;
  `/api/config` publishes it as `network_log_max_entries`, and the in-viewer
  panel and popped-out monitor trim to the same window — including replayed
  `net_init` snapshots.
- Copy a captured request's URL from the resource graph tooltip.

### Changed

- Resource graph is now summary-first: hosts collapse to cards and open into
  resource-type groups with counts and failure badges, and a group reveals a
  bounded preview plus an explicit “show all”. The first view focuses the
  opened page's branch, and a disclosure click frames the branch it opened;
  “fit” still shows the whole chain. Long sibling runs wrap into sub-columns,
  labels give way to a class-coloured overview below 55% zoom, a draggable
  minimap and arrow-key panning aid navigation, and the graph follows the
  panel's text, flagged, method and status filters. The failed chip isolates
  failed requests, trigger lines animate only along the hovered path, and
  “fit” collapses expanded groups when the result would be unreadable.
- The network monitor's copy action emits headered TSV on every tab — ISO 8601
  UTC timestamps, raw byte counts, and one escaped record per line (requests,
  domains, console, cookies) — instead of space-padded text.
- Release versions now come from git tags alone. `pkg/version` defaults to
  `dev` and `frontend/package.json` to `0.0.0`; `backend/scripts/build.sh`
  strips the leading `v`, so local, Docker and release builds agree and no
  source bump is needed per release. A bare `go build` therefore reports
  `dev`/`unknown` by design.

### Fixed

- The network monitor no longer keeps a separate row for every snapshot of the
  same request. The streamer rebroadcasts a request as it loads, so the first
  row used to stay at `status 0`, `size 0` and an empty `sha1` even after the
  body was hashed; later snapshots now update that row in place, which also
  makes the copied JSON and the resource graph counts reflect the finished
  request.
- The network monitor's header copy button now copies the active tab — cookie
  rows on Cookies and console lines on Console — instead of always copying the
  visible network requests.

## [1.1.0] - 2026-10-19

### Added

- Animated floating chrome logo in the viewer and share toolbars.
- Manual install path for hosts without Docker Compose.

### Changed

- Sandbox upgraded from Electron 34 (Chromium 132) to Electron 44
  (Chromium 152). The image now runs `node node_modules/electron/install.js`
  itself, because the v44 package ships no `scripts` field.

### Fixed

- Live sessions were torn down by the health checker: three failed
  `docker inspect` probes were treated as a dead container. Probes now skip
  inconclusive results, only an absent container fails, and each probe has a
  10 s timeout.
- The idle reaper killed sessions that were in active use. A successful
  `keepalivePing` now records activity.
- Fresh tabs flashed white and were reported as stalled; new-tab capture was
  unstable, pages could be painted on the wrong canvas colour, and frame
  timestamps were not monotonic. Blank tabs now hold for 3 s behind a
  per-tab `surfaceEstablished` flag, empty documents are normalised, and
  `ts_ns` comes from wall-clock UTC.
- The page canvas is white again. Background colour and framebuffer prefill
  are `#ffffff`; the theme colour only applies to a genuinely empty document.
- The address bar kept the previous tab's URL after switching tabs.
- The embedded app shell was served with no cache policy, so UI fixes could
  silently not reach the browser. Hashed assets are now immutable for a year
  and everything else is `no-cache`.
- The per-session `view_password` cache key leaked on the orphan sweep.
- The launch overlay could be shown twice between launch and first frame.

## [1.0.1] - 2026-10-16

### Changed

- CI builds the sandbox image, and the installer derives its version at
  install time instead of baking one in.

## [1.0.0] - 2026-10-16

Initial release: a Go control plane that boots a disposable Docker sandbox
running offscreen Chromium.

[Unreleased]: https://github.com/dm0xa/isola/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/dm0xa/isola/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/dm0xa/isola/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/dm0xa/isola/releases/tag/v1.0.0
