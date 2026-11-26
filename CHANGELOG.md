# Changelog

Notable changes to isola, in
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) format. `release.yml`
publishes the section matching the tag.

## [Unreleased]

### Changed

- **Custom data** docks to the right of the stream instead of covering it with a
  modal: the live page stays visible and clickable while you paste cookies,
  headers or storage. Escape and ✕ close it; the payload and counts are the same.
- **Resource graph: one card per site.** A page's own subdomains fold into its
  card instead of taking one each; a host on another domain keeps its own. The
  Graph tab, the hi-res PNG and `resource_graph` draw the same tree.

## [1.9.1] - 2026-11-25

### Fixed

- A sandbox that shuts itself down no longer looks like a stream timeout: the
  launch ends with `error_code: sandbox_exited` and the backend log names the
  exit, instead of dialing a removed container until the deadline.
- Tor sessions no longer die mid-bootstrap: the circuit gate is 45s (was 30s),
  and the streamer deadline follows it (75s, 90s with a pinned exit country).

## [1.9.0] - 2026-11-25

### Added

- Tor sessions are pre-warmed: `warm_pool.tor_size` (default `1`) keeps
  Tor-baked containers ready, claimed only by `tor_enabled` launches whose exit
  country matches `tor.exit_country`.
- `scripts/doctor.sh` gives a read-only verdict for a sandbox that will not
  start: Docker/Colima state, container port publishing, host and in-container
  stream probes, backend config.
- A failed launch carries `error_code` on `GET /api/sessions/{id}`
  (`sandbox_publish_unreachable`, `stream_not_ready`, `create_failed`), and the
  viewer turns it into the action that fixes it.

### Changed

- The viewer tab strip reads left to right: a fixed-width address bar, then
  back/reload, then scrollable tab chips with `+` after the last tab. Chips never
  shrink below 90px, tabs sharing a title are told apart by their URL tail, and
  the strip is reachable from the keyboard.

### Fixed

- A sandbox the host cannot reach is no longer treated as dead: the backend
  probes the streamer inside the container, uses the bridge IP only when it can
  dial it, and otherwise reports a publish failure naming the fix.
- A broken host publish path no longer drains the warm pool: the healthy
  container goes back into the pool and claims pause until probing recovers,
  while cold launches keep working.

## [1.8.0] - 2026-11-24

### Added

- `GET /api/captures` reports archive usage and retention: `usage.used_bytes`,
  `quota_bytes` and `retention_days`.
- Double- and triple-click work: `mousedown`/`mouseup` accept `click_count`.

### Changed

- The UI loads ~38% less on first paint and ships compressed: the binary
  install path serves gzip, fonts and icons cache for a week, and the viewer,
  network, captures and Analysis chunks load on demand. The toolbar logo no
  longer animates forever (idle viewer CPU ~6× lower).
- Filtering the network log no longer re-encodes kept entries (REST and MCP
  `get_network`).
- Resource capture is one recording per session: a retry returns the same
  archive, a start after Stop is `409 archive_exists`, and the status is
  `recording:false` before a start. The live status repeats the one-time
  `archive_key` to the owner, so a viewer opened mid-recording can browse the
  files after Stop.
- Authenticated owner API calls count as activity, and a finalized session
  keeps its token until it expires, so a REST client is not idle-reaped and the
  owner can still read a terminal row.
- `graph.png` renders once per session, and recon waits up to 20 s upstream.
- Share viewers stop when the link is revoked or the session ends.

### Removed

- The landing-page **What's new** list; the features are documented in
  [`docs/`](docs/README.md).

### Fixed

- Typing punctuation and symbols works: `!@#$%^&*()`, `,.;:'"[]{}<>/?\|` and a
  literal `+` reach the page.
- `GET /api/sessions/{id}` is owner-only again: a deep link without its token
  says so instead of opening a share view.
- Share links use the app's own origin on a default install (`cors.origin`
  defaults to `:8080`).
- `alert()`/`confirm()`/`prompt()` no longer freeze a tab, and a `beforeunload`
  handler no longer traps navigation.
- IME composition works in the viewer.
- Recon errors use the standard `{error:{code,message}}` envelope, and the panel
  shows the message.
- Deleting an ended session is idempotent (`200`), and creating a share while
  the sandbox is still starting answers `409 container_not_ready`.
- `browser_theme`, `frame_rate` and `bandwidth_kbps` are validated at creation
  (`400`).
- MCP `navigate` reports `document_seen` for that navigation only, and
  `screenshot` retries through the CDP re-attach after a navigation.
- `data:`, `blob:` and `file:` URLs are no longer listed as hosts or flagged as
  random domains.
- Viewer deep links: `#token=` is honored and scrubbed, a malformed session id
  ends in "Session not found", and a dropped launch request says the backend
  may be down. Focus returns to Session actions after **Custom data…**.
- The OpenAPI document includes `POST /api/sessions/{id}/audio`.

## [1.7.3] - 2026-11-22

### Fixed

- A page on an IPv6-preferring host (e.g. `ya.ru`) no longer stops at
  `ERR_ADDRESS_UNREACHABLE`: the sandbox has no IPv6 route, so the runner
  retries that main-frame load once, which succeeds over IPv4. Other load
  failures are reported unchanged.

## [1.7.2] - 2026-11-22

### Fixed

- A stream no longer stalls at "Establishing stream" when the viewer attaches
  before the sandbox's runner is listening: the resize the viewer asks for is
  now delivered as soon as the runner connects instead of being dropped. The
  encoder waited forever for a geometry the surface never reached.

## [1.7.1] - 2026-11-22

### Fixed

- A lowercase timezone (`europe/stockholm`) validates on Linux again: the
  registered spelling is resolved before the tz lookup instead of relying on
  macOS's case-insensitive filesystem. The tz database is embedded, so a host
  without `tzdata` can still validate a session's zone.

## [1.7.0] - 2026-11-22

### Added

- **Record** — an opt-in viewer control that saves every body the page downloads
  (scripts, styles, documents, images, fonts, XHR/fetch) with URL, status, MIME,
  sizes and SHA-256, deduplicated by content. Archives survive the session and
  open in a host-grouped Resources panel you can browse, preview and download.
- **Custom data** — `POST /api/sessions/{id}/seed` and the MCP `set_session_data`
  tool push cookies, extra request headers and web storage into a running page,
  with an optional reload. Owner-only; values are never logged or stored, and the
  viewer exposes it as **Custom data…**.
- **Traffic log over REST** — `GET /api/sessions/{id}/network` returns the same
  document as MCP `get_network`, with the same `type`, `host_contains`,
  `url_contains` and `limit` filters. Both share one filter package, so an
  argument cannot mean two things.
- **Timezone and locale per session** — `timezone` and `locale` on create are
  applied before the first script, so `Date`, `Intl`, `navigator.languages` and
  `Accept-Language` agree from the start. The launcher keeps both under
  **Advanced** and defaults to one coherent pair per launch.
- **Session parameters on hover** — the session-id chip shows resolution, frame
  rate, browser, identity and Tor; the debug overlay and copied debug report
  carry the same values.

### Changed

- A warm-pool container takes the session's identity over CDP when claimed, so a
  pinned or random locale/zone no longer forces a cold start.
- The landing page lists what is new in the running release; the **×** hides it
  until the next version.

### Fixed

- A session reports its exact requested resolution. Xvfb now runs one pixel
  larger per axis, so Chromium no longer clamps a 1280x720 page to 1279x719.
- The sandbox presents one consistent Chrome client: client hints,
  `navigator.platform`, languages and the UA agree, and the detectable
  `isolaBridge` preload global is gone.

## [1.6.5] - 2026-11-20

### Fixed

- A sandbox that fails to start is no longer reported as an expired session;
  the viewer points you at the isola log for the real reason.
- Troubleshooting covers a macOS launchd install that cannot find Docker's
  socket.

## [1.6.4] - 2026-11-17

### Changed

- Logs read like events now: one short line each, and no repeat noise from
  retries, ABR steps, blocked navigations or `x264` stats.

### Removed

- Internal cleanup: the session store's pass-through wrappers are gone and
  stale comments were trimmed. Behavior is unchanged.

## [1.6.3] - 2026-11-17

### Fixed

- Sites that check the browser name no longer turn the sandbox away as
  "Electron": it now reports the Chrome version it actually runs.

## [1.6.2] - 2026-11-17

### Fixed

- The backend no longer crashes when an idle warm-pool container dies: the
  claim and reconcile paths used a test-only teardown hook.
- A claimed warm container is shielded from the orphan sweep until its session
  is active, so a sweep can no longer stop the sandbox a viewer is about to
  stream from.
- A Tor session with a pinned exit country gets its full 90 s stream budget
  instead of being cancelled at 60 s.
- `DELETE /api/sessions/{id}` finishes tearing the container down even when the
  client hangs up mid-request.
- The server stops accepting requests before draining the warm pool on
  shutdown, and the startup reconcile is time-bounded, so neither leaves an
  untracked container nor blocks the listener.
- `GET /api/sessions/{id}/intel` answers `502 intel_failed` when the upstream
  document exceeds 4 MiB instead of returning truncated JSON with `200`.
- An MSE viewer recovers from a packet-loss burst instead of asking for a
  keyframe every 5 s and eventually reconnecting.
- A stalled viewer input socket no longer freezes network/console capture, and a
  persistent frame-socket `accept` error no longer spins a core.
- The viewer disconnects on every unmount and times out its reconnect probe, so
  leaving a connecting session no longer leaks sockets and timers.
- The viewer revokes each blob URL when it is replaced and ignores a malformed
  text frame, so memory stays bounded across quality steps.

### Changed

- Sandbox start and teardown are faster: readiness is polled instead of two
  fixed one-second sleeps, shutdown kills instead of awaiting grace, a cold
  start discovers the container with one `docker inspect` instead of three, and
  the streamer wait polls every 100 ms. Same host: `POST /api/sessions` →
  `running` 1.59 s → 1.23 s, → first frame 2.84 s → 1.42 s.
- A warm-pool claim now requires an exact resolution match, like theme and
  frame rate. Reshaping an idle warm container is applied lazily by Chromium
  and cost more than the cold start it was meant to save, so a smaller request
  cold-starts at the size it asked for.
- A host install no longer attempts the container-only `browser-net` address
  lookup on its first session launch.
- The streamer recycles frame buffers per connection instead of allocating a
  surface per frame, and copies full-width damage as one contiguous run.
- The viewer shell is memoised, network/console/cookie captures publish once
  per 50 ms burst, URL analysis and graph layout are cached, and pan/zoom
  commits once per frame — so a busy capture no longer re-renders the whole
  panel per event.

## [1.6.1] - 2026-11-12

### Added

- Everything isola creates — containers, Compose services, networks, the data
  volume and the images — carries `isola.stack=1`, so
  `docker ps -a --filter label=isola.stack=1` lists the whole deployment.

### Changed

- A session whose container is gone is finalized as soon as the runtime confirms
  it, instead of after three 15 s health probes.

### Fixed

- A dead sandbox fails the relay handshake with `502` instead of upgrading the
  socket and closing it, so viewers no longer reconnect every 500 ms for the rest
  of the session's TTL.
- The viewer resets its reconnect backoff on the first decoded frame and stops
  retrying once the session reports `terminated` or `error`.

## [1.6.0] - 2026-11-05

### Added

- An [MCP usage guide](./docs/skills/isola-mcp-usage/SKILL.md): handshake, every tool and its arguments, recipes, REST fallback.
- The viewer counts video `seq` gaps as `packetsLost` (a 1 Hz `packetLossRatio`)
  and asks for a keyframe after five missing packets instead of waiting for an
  IDR that may never come. Debug metrics show the server's `dropped` counter.

### Changed

- WebCodecs backlog is capped at 250 ms of stream time: past that the client
  asks for a keyframe and resumes at the next IDR instead of replaying a stale
  queue. Published as `decodeBacklogMs`, `decodeDroppedFrames`, `decodeCatchUps`.
- An ABR step reconfigures `VideoDecoder` in place and resizes the existing
  canvas, so a bitrate-only step no longer leaves the decoder unconfigured and a
  scale step no longer orphans a canvas.
- A session runs at most two encoders: a third rung joins the closest live run
  at or below its bitrate, never across decode modes or capture geometries, and
  the client sees the real rung. Three viewers on three rungs: 0.77 → 0.57
  core-seconds per second of video.
- `request_keyframe` is budgeted per connection — one per 30 s, four total — and
  a share-link watcher forwards one per 30 s. A pictureless viewer keeps three
  restarts on the existing 5 s cooldown.
- The encoder tuning is unchanged: a fifteen-variant sweep found nothing that
  improves the picture for less CPU than `superfast` + `subme=1:me:dia`.

### Fixed

- The encode pump holds the 25 fps ceiling on both feed paths (the ticker path
  now checks the encode interval too), with a tenth of the interval of slack.
- The relay closes both sockets when either side dies and writes every message
  under a 30 s deadline, so a disconnected viewer or wedged streamer cannot park
  a goroutine until the 90 s read deadline.
- Container→client messages are capped at 32 MiB and TEXT frames at 1 MiB;
  client messages stay at 12 MiB.
- Activity tracking refreshes at most once per second and no longer takes the
  proxy-wide mutex per frame: 178-186 → 10-13 ns/op.
- A viewer whose socket hits the 10 s write deadline, or whose metrics or audio
  write fails, loses its session; the encoder stops with the last viewer and
  teardown runs exactly once.
- An unknown `/api/...` path returns a JSON `404` (`not_found`) instead of the
  SPA shell; client routes still serve the shell.
- The streamer's `/resize` requires `POST` (`405` otherwise), so a prefetch or
  link preview can no longer re-key every viewer's encoder.
- When no encoder can start, the streamer sends `stream_error`
  (`reason: encoder_error`, `retryable: true`) and closes with code `4001`
  instead of a `stream_reset` no client can follow.
- `POST /api/sessions/{id}/resize` rejects geometries that round to 0 (1x1 became
  0x0).
- The 1 Hz `metrics` snapshot carries `dropped`, split into `drop_overflow` and
  `drop_awaiting_key`.
- `docs/transport.md` no longer claims an ABR `set_bitrate` resets other viewers,
  or that the streamer ignores the capture `ts_ns` field.
- The streamer's parsers treat their input as hostile: bounds-checked capture
  headers, a 5 s stalled-frame drop, 32 MiB fMP4 boxes with resync, an 8 MiB
  Annex-B cap, and no spin on a zero-byte read. Valid input stays byte-identical.
- Warm-pool sessions no longer open on an unnamed `about:blank` tab.

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

[Unreleased]: https://github.com/dm0xa/isola-browser/compare/v1.9.1...HEAD
[1.9.1]: https://github.com/dm0xa/isola-browser/compare/v1.9.0...v1.9.1
[1.9.0]: https://github.com/dm0xa/isola-browser/compare/v1.8.0...v1.9.0
[1.8.0]: https://github.com/dm0xa/isola-browser/compare/v1.7.3...v1.8.0
[1.7.3]: https://github.com/dm0xa/isola-browser/compare/v1.7.2...v1.7.3
[1.7.2]: https://github.com/dm0xa/isola-browser/compare/v1.7.1...v1.7.2
[1.7.1]: https://github.com/dm0xa/isola-browser/compare/v1.7.0...v1.7.1
[1.7.0]: https://github.com/dm0xa/isola-browser/compare/v1.6.5...v1.7.0
[1.6.5]: https://github.com/dm0xa/isola-browser/compare/v1.6.4...v1.6.5
[1.6.4]: https://github.com/dm0xa/isola-browser/compare/v1.6.3...v1.6.4
[1.6.3]: https://github.com/dm0xa/isola-browser/compare/v1.6.2...v1.6.3
[1.6.2]: https://github.com/dm0xa/isola-browser/compare/v1.6.1...v1.6.2
[1.6.1]: https://github.com/dm0xa/isola-browser/compare/v1.6.0...v1.6.1
[1.6.0]: https://github.com/dm0xa/isola-browser/compare/v1.5.4...v1.6.0
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
