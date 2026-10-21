# Changelog

Notable changes to isola, in
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. `release.yml`
publishes the section matching the tag.

## [Unreleased]

### Added

- The landing page accepts `?q=`, so isola can be registered as a browser search
  engine (`https://isola.example/?q=%s`): the text is prefilled into the address
  box and normalised on submit like typed input, and adding `&autorun=1`
  launches immediately. Both parameters are consumed from the URL, so a reload
  never relaunches or re-prefills.
- The sandbox viewport follows the viewer's capture box: entering fullscreen (or
  resizing the window) now reshapes the remote browser instead of letterboxing
  the launch geometry. `POST /api/sessions/{id}/resize` resizes the runner's
  offscreen windows, re-keys the encoder and resets attached viewers onto the
  new geometry (`stream_reset` reason `resize`); the viewer debounces the
  request and reshapes inside the session's launch resolution, so the pixel
  budget never grows.
- The installer reports whether the host enforces an AppArmor/SELinux profile
  for session containers; it never writes `ISOLA_REQUIRE_MAC` itself, so the
  fail-closed default is left to the operator.
- The service environment lives in `<install dir>/isola.env` on both systemd and
  launchd installs; the LaunchAgent has it rendered into
  `EnvironmentVariables`.

### Changed

- Resource graph requests are validated by the API before they reach the
  streamer, and a render is now bounded: at most two run concurrently per session
  (`429 graph_render_busy`), each has a 45 s budget (`504 graph_render_timeout`),
  and an image over 32 MiB is refused instead of buffered. A session whose
  container is still starting returns the documented `409 container_not_ready`
  rather than `404`. The `resource_graph` MCP tool enforces `scale` 1–4 locally,
  applies the same 45 s deadline and caps the embedded PNG at 8 MiB.
- The installer falls back to `~/.isola` when the invoking user cannot create
  `/opt/isola`, so an unprivileged install works on any host without setting
  `ISOLA_INSTALL_DIR`.
- The installer pulls the sandbox image for the installed release
  (`ghcr.io/dm0xa/isola-browser:v<version>`) and only then falls back to the
  floating `:electron` tag, keeping the binary and the streamer's ISOL wire
  header / ABR ladder in step. `ISOLA_BROWSER_IMAGE_REMOTE` still pins one exact
  reference; empty skips the registry.
- Re-running the installer now regenerates the systemd unit / LaunchAgent and
  restarts the service, so upgrades pick up new defaults instead of leaving a
  stale unit in place. A unit without the installer marker is backed up first.
- The LaunchAgent sets `PATH`, so the service can find the `docker` CLI.
- The install no longer aborts on a missing controlling terminal: prompts take
  their default instead of failing under `set -e`.

### Fixed

- Two concurrent resource-graph renders could kill the whole streamer, taking
  every attached video stream with them: the font-face cache was an
  unsynchronised map, and each cache entry was an `opentype.Face`, which is not
  safe for concurrent use. Faces are now built per render from shared immutable
  parsed fonts, and the graph/font tests run under the race detector in CI.
- `GET /api/sessions/{id}/graph.png` no longer reports success for an image it
  did not deliver: the body is read in full before the status line is written,
  responses carry `Content-Length`, a truncated or failed render is `502`, and
  the 15 s server write deadline no longer cuts off a large graph. Renderer
  failures are no longer reported as `400 invalid_graph_options`, which is now
  reserved for real option errors.
- Installation aborted when the default install directory could not be created
  by the invoking user.
- Fullscreen filled the screen with black bands above and below the stream: the
  viewer is now sized from the fullscreen element (100% height chain) instead of
  `100vh`, which kept the pre-fullscreen height in some browsers, and the
  fullscreen backdrop is painted with the app background instead of the UA's
  black.

## [1.1.1] - 2026-10-21

### Added

- Configurable network capture window: `session.network_log_max_entries`
  (`ISOLA_NETWORK_LOG_MAX_ENTRIES`), default 1000, capped at 10000, replacing
  the hard-coded 400. `/api/config` publishes it; the panel and monitor trim to
  it, including replayed `net_init` snapshots.
- Copy a request's URL from the resource graph tooltip.
- `GET /api/sessions/{id}/graph.png` and the `resource_graph` MCP tool render
  the captured network as a resource-graph PNG on the server: `theme`
  (`dark`/`light`), `direction`, `expand` (`origin`/`all`/`collapsed`), resource
  type and failed-only filters, and a 1–4× raster scale.

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
- The single collapse button is now a collapse-all/expand-all pair; both clear
  the pin and refit the graph.
- PNG export frames the graph's measured bounding box instead of its nominal
  layout size, so labels and selection rings can no longer be cropped, and the
  watermark sits in the bottom-right padding instead of over the nodes. The
  image carries a caption with the origin host, request/host/resource counts,
  active filters and the UTC capture time.
- Versions come from git tags only: `pkg/version` defaults to `dev`,
  `frontend/package.json` to `0.0.0`, and `build.sh` strips the leading `v`. A
  bare `go build` reports `dev`/`unknown` by design.

### Fixed

- The network monitor kept a row per snapshot of a request, leaving the first at
  `status 0`, `size 0` and an empty `sha1`. Snapshots now merge in place.
- The network monitor's copy button always copied requests; it now copies the
  active tab as headered TSV with ISO 8601 UTC times and raw byte counts.
- The network panel listed a phantom second browser tab. Traffic captured before
  the runner reported its tab strip (the launch-page load, usually the largest
  burst of a session) was attributed to the "no tab known yet" id, and the
  panel's per-tab filter rendered that bucket as an extra tab named after its
  busiest host. Those entries are now assigned to the tab that turns out to be
  active as soon as the strip arrives, and re-published so an attached viewer
  corrects the rows in place.

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

[1.1.1]: https://github.com/dm0xa/isola/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/dm0xa/isola/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/dm0xa/isola/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/dm0xa/isola/releases/tag/v1.0.0
