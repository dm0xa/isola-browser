# Changelog

Notable changes to isola, in
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. `release.yml`
publishes the section matching the tag.

## [Unreleased]

### Added

- The viewer now accounts for packet gaps on the video socket: `seq` jumps
  (TCP, so they mean the streamer shed packets) are counted as `packetsLost`
  with a 1 Hz `packetLossRatio`, and once a burst reaches five missing packets
  it asks for a keyframe through the existing recovery budget instead of
  waiting on the IDR the encoder may never send. The debug metrics also show
  the server's `dropped` counter when a newer streamer reports it.

### Changed

- The WebCodecs viewer no longer replays a decode backlog. `VideoDecoder`
  output is fire-and-forget, so a burst — or a tab that was throttled — left the
  past queued ahead of the live picture with nothing to drop it. Access-unit
  admission is now bounded at 250 ms of stream time: past that the client
  abandons the decoder queue, asks for a keyframe, and resumes on the next IDR.
  The delay and the drops are published at 1 Hz as `decodeBacklogMs`,
  `decodeDroppedFrames` and `decodeCatchUps`.
- An ABR step no longer costs the client its decoder or its canvas.
  `stream_reset` followed by `stream_init` with an unchanged codec reuses the
  same `VideoDecoder` — reset leaves it unconfigured, so it is reconfigured in
  place — and resizes the existing canvas instead of appending a new one. Before
  this, a bitrate-only step left the decoder unconfigured and every later access
  unit was silently refused for the rest of the connection, and a scale step
  allocated a new decoder and left the old canvas in the container.
- A session now runs at most two encoder processes. Each distinct
  (bitrate, scale, decode mode) used to get its own ffmpeg, and the ABR ladder
  gives one viewer four of them, so three viewers stepping the ladder
  differently ran three libx264 processes in a three-CPU container — the encode
  cost alone (0.3-1.2 core-seconds per second of video each) starves the frame
  pump and turns an adaptive step into a stall. A third configuration now joins
  the most expensive live run at or below its requested bitrate (else the
  cheapest one) instead of starting a third encoder; the client sees the real
  rung in the `stream_reset`/`stream_init` pair and in metrics, so nothing is
  silent. A viewer alone in the session, and the host + share-watcher pair, keep
  exactly the rung they ask for, and runs are never shared across decode modes
  or capture geometries. Measured with `scripts/ab/multi-rung.sh` at 720p/25fps
  on the dev host, three viewers on three rungs (interleaved, 3 reps):
  0.77 → 0.57 core-seconds per second of video, encoders 3 → 2.
- A viewer can no longer restart the session's encoder on demand, indefinitely:
  `request_keyframe` is now budgeted per connection — one forced restart per
  30 seconds, four per connection — and a share-link watcher is additionally
  limited to one forwarded request per 30 seconds by the relay. A viewer that
  still has no picture keeps a small retry allowance (three restarts, spaced by
  the existing 5 s cooldown) so a restart that produced no IDR cannot wedge it
  until it reconnects. Every forced restart replaces the encoder run for
  everyone watching it, so one stuck decoder could previously reset the host's
  stream forever.
- The encoder is unchanged: the quality sweep that followed this work
  (fifteen preset/search/aq combinations, recorded next to the libx264 argv)
  found no setting that improves quality without spending more CPU, so the
  released `superfast` + `subme=1:me:dia` tuning ships as it was. What changed
  around it is stability and cost, not the picture.

### Fixed

- A viewer whose socket hit the 10 s write deadline now loses its session: the
  socket is closed, so the subscription is released and the encoder stops.
  Before, the write goroutine exited but left the connection half-dead — the
  session kept an encoder and a mailbox for a viewer that would never read
  again, until the peer eventually closed.
- `POST /api/sessions/{id}/resize` rejects geometries that cannot survive the
  even-dimension rounding (1x1 became 0x0) instead of zeroing the session's
  geometry and leaving the next viewer waiting forever for a surface that never
  arrives.
- The 1 Hz `metrics` snapshot now carries `dropped`, the number of video packets
  this viewer's socket shed (mailbox overflow or discarded while waiting for a
  keyframe), so a stalling viewer is visible instead of looking like a slow
  encoder.
- `docs/transport.md` claimed an ABR `set_bitrate` reset every other viewer of
  the run; only the requesting viewer moves rungs, and the previous run stops
  when its last viewer leaves. The same page also said the streamer does not
  read the capture `ts_ns` header field; it does, nothing downstream uses it.

## [1.5.4] - 2026-11-04

### Added

- The UI installs as a Chromium app.
- Tor state no longer hides behind the settings menu: while Tor is enabled the
  landing page shows a small purple `Tor` chip and a purple halo behind the logo.
- Session launched through Tor carries a `Tor` badge in the toolbar.

## [1.5.3] - 2026-11-04

### Fixed

- Sessions no longer get stuck at ~1 fps with a viewer attached: the sandbox
  runner re-applies the watched tab's frame rate when the streamer re-sends
  "capture on" (that message used to be ignored) and every 5 seconds while
  someone is watching. Scrolling and typing are smooth again instead of looking
  frozen behind a stream that still claimed to be active.
- `install.sh` fetches release assets from the versioned download path, so
  `ISOLA_VERSION=X.Y.Z` installs that release instead of 404ing on a file the
  newest release no longer publishes.
- Tor sessions no longer fail provisioning on a slow circuit: the wait for the
  streamer now includes the container's Tor bootstrap gate (30s, or 60s with a
  pinned exit country) instead of racing it with a flat 30s.

## [1.5.2] - 2026-11-02

### Changed

- Clicking a host in the resource graph now spotlights its whole branch — the
  host, its type groups and their files — and dims the rest, like clicking a
  type group already did.
- The resource graph groups requests by the page that loaded them, in visit
  order, instead of inferring a trigger chain from capture order. Across
  several navigations each page sits under the entry block, and an earlier page
  no longer looks like it loaded the next one.

### Fixed

- The stream-quality indicator no longer flashes red on link hover: arrival
  rate is measured over the same second as the server's egress.
- `blob:` and `data:` requests get their own URL as the host label in the
  resource graph instead of one nameless card.
- A request first seen at `responseReceived` takes its URL from the response, so
  the Network tab no longer shows a blank row and the graph no longer draws a
  nameless host.

## [1.5.1] - 2026-10-31

### Changed

- Dependency refresh: React 19, Vite 8 with `@vitejs/plugin-react` 6 (Rolldown
  bundler, Lightning CSS minification of the stylesheet), React Router 7,
  Vitest 5, jsdom 30, ESLint 10, `lucide-react` 1.x, Electron 45.1, and the
  weekly npm/Actions groups. React 19 needed only a ref-type widening in the
  two viewer shells and React Router 7 only the removal of the `v7_startTransition`
  opt-in; rendering, routes and icon geometry are unchanged.

### Security

- React Router moves to 7.18.5, clearing two moderate advisories in the 6.x line
  (open redirect through `<Link>`/`useNavigate`, and constructor injection in SSR
  hydration error deserialization). No 6.x release contains the fixes.

## [1.5.0] - 2026-10-31

### Changed

- Frontend styling moved to Tailwind CSS 4, compiled by the `@tailwindcss/vite`
  plugin instead of PostCSS; `postcss.config.js` and `tailwind.config.js` are
  gone, class-based dark mode is declared in `src/index.css`, and the utilities
  v4 renamed were migrated, so both themes render as before. v4 raises the
  browser floor to Chrome 111, Safari 16.4 and Firefox 128.

## [1.4.1] - 2026-10-26

### Security

- Dependency refresh: Electron 44.9.1, `follow-redirects` 1.16.1,
  `react-router-dom` 6.30.6 and the frontend dev chain. Every high-severity
  advisory is cleared.

### Changed

- Automatic renderer recovery shares one budget (one per URL, three per ten
  minutes); manual recover is unaffected. Inactive tabs and viewer-less
  sessions composite at 1 fps, and page loads read the surface back twice
  instead of five times.

### Fixed

- The installer fetches the release-matched sandbox image even when a local
  `isola-browser:electron` tag exists; a leftover image could mismatch the
  binary. A detached install no longer prints a `/dev/tty` error before taking
  its prompt defaults.

## [1.4.0] - 2026-10-26

### Security

- The control plane runs with the sandbox's hardening: read-only rootfs, dropped
  capabilities, no privilege escalation and configurable limits.
- API request bodies are capped at 64 KiB. Seccomp remains opt-in, now
  documented.

### Added

- **Analysis panel.** Headers, Trackers and Recon tabs, with a security-posture
  grade, TLS summary, tracker/beacon/cookie inventory and redirect chains that
  keep each hop's status.
- **Recon.** DNS/ASN enrichment and bounded host probes on demand; nothing runs
  until a host is named and no third-party service is contacted.
- **MCP.** Six tools for headers, posture, redirects, trackers, host
  intelligence and probing; network entries now carry headers, TLS and tracker
  labels.
- [`docs/third-parties.md`](docs/third-parties.md) lists what leaves the
  sandbox.

### Changed

- **GPU encoding is opt-in** via `deploy/docker-compose.gpu.yml`, which adds
  `/dev/dri` and `ISOLA_GPU=1` together. The base stack no longer mounts the
  device, so `docker compose up` works on GPU-less hosts, and the Vite dev
  server publishes on loopback only.
- **MCP `screenshot` returns an image block** instead of `data_base64`.
- **MCP filters mean one thing:** `host_contains` matches the hostname in every
  tool, and paths or queries use `url_contains`.
- Posture accounts for transport trust and caps the grade on untrusted or
  plaintext connections. Recon follows the Tor circuit, where ASN lookups are
  unavailable.
- Session inspection reports the posture grade and tracker counts; the viewer
  hides recon actions when an operator disables the feature, and the network
  panel is opaque.

### Fixed

- Text and emoji fonts ship in the image; regional-indicator flags no longer
  render as tofu.
- Blank frames painted during navigation are withheld from the viewer.
- Deleting a session while its container is being created no longer leaves the
  container running.
- MCP `create_session` works when `ISOLA_API_TOKEN` is set, forwards the
  caller's address to the per-client rate limit, and terminates a session that
  never becomes usable.
- MCP `click`, `type` and `navigate` are time-bounded; `navigate` retries once,
  and `get_network` refuses a capture window over 8 MiB with an actionable
  error.
- Filtering the network log no longer drops fields, and the request graph keeps
  the opened page in the leftmost column.
- Recon can trust an operator-supplied CA bundle on TLS-inspecting networks.

## [1.3.0] - 2026-10-22

### Added

- `demo/` — a static, no-build interface demo that reproduces the landing page
  (pinned to `example.com`).

### Changed

- `browsers/isola-streamer/go.mod` now declares `go 1.26.0` (was `1.22`). The
  module was always compiled by the 1.26 toolchain, and the patched `x/net`
  requires it; local builds therefore need Go 1.26, as both modules now pin
  `toolchain go1.26.10`.
- Local development prerequisites are now **Node 22+** (was 20), matching the
  builder images; Node 20 reached end-of-life in April 2026.
- Dependency updates are automated: Dependabot watches both Go modules, both
  npm trees, GitHub Actions and the Docker base images (runtime majors are left
  for deliberate migrations), and CI plus the release build pin Go 1.26.10 to
  match the builder images.

### Fixed

- MCP `create_session` no longer fails with `401 unauthorized` when
  `ISOLA_API_TOKEN` is also configured.
- Deleting a session while its container was still being created no longer
  leaves an untracked container behind. `ActivateSession` used to overwrite the
  terminated status with `running`, resurrecting the row and hiding the
  container from the orphan sweep; it now refuses anything but a provisioning
  row, and the create path stops the container it just started.
- The orphan sweep no longer skips one container — always the last line of
  `docker ps`, and a lone orphan every time. Trimming the whole output dropped
  the empty `isola.warm` field, so that line failed to parse as a session
  container; it is now trimmed per line.

### Security

- The streamer is off the 2020 `golang.org/x/net` pseudo-version that `cmux`
  pinned: `x/net` v0.61.0 (with `x/image` v0.47.0 and `x/text` v0.43.0),
  closing the HTTP/2 and image-decoder advisories `govulncheck` reported. The
  backend moves to `go-chi/chi` v5.3.0 and `golang.org/x/sys` v0.49.0. Both
  modules pin `toolchain go1.26.10`, and the Docker builders use
  `golang:1.26.10-alpine`, so a stale cache cannot build against an unpatched
  1.26.0 standard library.
- Electron is pinned to `44.9.0` with a committed `package-lock.json` and
  `npm ci` in place of a floating `npm install`, so an image build can no
  longer resolve an unpatched 44.0–44.2 (CVE-2026-113800).
- The frontend builder moves from `node:20-alpine` — end-of-life since April
  2026 — to `node:22-alpine`.

## [1.2.0] - 2026-10-21

### Added

- The landing page accepts `?q=` for registration as a browser search engine
  (`/?q=%s`): the text is prefilled into the address box and normalised on
  submit like typed input, and `&autorun=1` launches immediately. Both
  parameters are consumed, so a reload never relaunches or re-prefills.
- The sandbox viewport follows the viewer's capture box, so fullscreen and
  window resizes reshape the remote browser instead of letterboxing it.
  `POST /api/sessions/{id}/resize` resizes the runner's offscreen windows,
  re-keys the encoder and resets attached viewers (`stream_reset` reason
  `resize`); the viewer debounces the request, keeps the session's launch
  resolution as the pixel budget, and sizes itself from the fullscreen element.

### Changed

- A warm claim now serves any resolution that fits inside the pooled box and
  reshapes the display to it before the viewer attaches, so launches that adapt
  the resolution to the window (the UI fits it to the viewer aspect) are warm
  too. Larger resolutions, and a different theme or frame rate, still
  cold-start.
- The warm pool fills its slots one at a time, with a pause between boots,
  instead of booting every container at once; a failed fill waits for the next
  reconcile pass instead of retrying immediately.
- `scripts/dev-rebuild.sh` reads `WARM_POOL_ENABLED` and `WARM_POOL_SIZE` from
  `.env`, so the dev server keeps the same pre-started containers as Compose.
- The resource-graph renderer batches same-colour shapes into one rasteriser
  pass and fits the image into the requested `width`×`height` (8192 px per edge,
  24 MP). A 200-entry `expand=all` render takes ~0.2 s, and expanded groups list
  at most 12 resources plus a "show all N more" row.
- Graph options are validated in the API (`pkg/graphopts`) and every render is
  bounded: two in flight per session (`429 graph_render_busy`), 45 s
  (`504 graph_render_timeout`), 32 MiB. A session whose container is starting
  returns `409 container_not_ready`. The `resource_graph` MCP tool validates
  `scale` locally, applies the same deadline and caps the PNG at 8 MiB.
- The installer pulls the sandbox image for the installed release
  (`ghcr.io/dm0xa/isola-browser:v<version>`) and falls back to `:electron`,
  keeping the binary and the streamer's ISOL wire header / ABR ladder in step.
  `ISOLA_BROWSER_IMAGE_REMOTE` pins one reference; empty skips the registry.
- The installer resolves its directory by writability (`/opt/isola`, else
  `~/.isola`), reads the environment from `<install dir>/isola.env` for both
  systemd and launchd, sets `PATH` in the LaunchAgent, and reports the host's
  AppArmor/SELinux state without writing `ISOLA_REQUIRE_MAC`.
- Re-running the installer regenerates the unit and restarts the service; a unit
  without the installer marker is backed up first. The install no longer aborts
  without a controlling terminal, taking prompt defaults instead.
- The project moved to `github.com/dm0xa/isola-browser`; previous URLs redirect.

### Fixed

- The warm pool's orphan sweep compared the 12-character `docker ps` ID with the
  full create ID, so it removed the pooled containers it exists to protect. IDs
  now match on prefix.
- A live resize could feed the encoder frames of the previous geometry, which
  corrupted the rest of the run (horizontal smearing). The pump now waits for
  the composed surface to match the run's key.
- Streamer network capture built wire messages outside the lock, racing
  redirects, tab reassignment and body hashing. Messages are built under the
  lock and broadcast after release, so a slow viewer cannot stall capture
  either. CI runs the streamer suite with `-race`.
- A request first seen at `responseReceived` now enters the rolling capture
  window, so it is evicted and replayed like any other.
- A tab strip with no active tab no longer resets attribution to `-1`.
- `failed=1` graphs keep the opened page as the origin.
- Console dedupe no longer drops a legitimate repeat and is bounded by the
  300-line buffer. Lines carry a stable id, so a re-published attribution folds
  in place while two tabs' identical lines stay distinct.
- Concurrent graph renders could kill the streamer: the font-face cache was
  unsynchronised and `opentype.Face` is not safe for concurrent use. Faces are
  built per render from shared parsed fonts.
- `GET /api/sessions/{id}/graph.png` reads the body in full before writing the
  status and sends `Content-Length`; a truncated render is `502`, renderer
  failures are no longer `400 invalid_graph_options`, and the 15 s write
  deadline no longer cuts off a large graph.
- The network export escapes a literal backslash and a lone carriage return,
  the URL copy reports a clipped URL, copy confirmation is scoped to the copied
  node, and the graph's notice/copy timers are cleared on unmount.
- `buildGraph` parses each URL once instead of up to 40 times per row per
  network event, and the minimap paints into a single canvas.
- `scripts/dev-rebuild.sh` stops a backend started from an absolute path as well
  as `./isola-su`, waits 30 s for `/health`, and takes the Docker socket from
  the active context, so colima and OrbStack are preferred over an unrelated
  daemon.

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

[Unreleased]: https://github.com/dm0xa/isola-browser/compare/v1.5.4...HEAD
[1.5.4]: https://github.com/dm0xa/isola-browser/compare/v1.5.3...v1.5.4
[1.5.3]: https://github.com/dm0xa/isola-browser/compare/v1.5.2...v1.5.3
[1.5.2]: https://github.com/dm0xa/isola-browser/compare/v1.5.1...v1.5.2
[1.5.1]: https://github.com/dm0xa/isola-browser/compare/v1.5.0...v1.5.1
[1.5.0]: https://github.com/dm0xa/isola-browser/compare/v1.4.1...v1.5.0
[1.4.1]: https://github.com/dm0xa/isola-browser/compare/v1.4.0...v1.4.1
[1.4.0]: https://github.com/dm0xa/isola-browser/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/dm0xa/isola-browser/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/dm0xa/isola-browser/compare/v1.1.1...v1.2.0
[1.1.1]: https://github.com/dm0xa/isola-browser/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/dm0xa/isola-browser/compare/v1.0.1...v1.1.0
[1.0.1]: https://github.com/dm0xa/isola-browser/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/dm0xa/isola-browser/releases/tag/v1.0.0
