# API reference

The REST API is served from the same origin as the UI, under `/api`. Requests and responses are JSON unless noted. The live OpenAPI 3.0 document is available at **`GET /api/openapi.json`** — treat it as the source of truth for field-level detail.

Base URL for a local deployment: `http://localhost:8080`.

## Authentication

| Scope | How |
| :--- | :--- |
| **Public** | `GET /api/config`, `GET /api/share/{token}`, `GET /api/openapi.json` need no credentials. |
| **Session create** | `POST /api/sessions` is open by default. When `api.token` / `ISOLA_API_TOKEN` is set (one or more, each ≥ 32 chars), it requires `Authorization: Bearer <token>`. |
| **Session owner** | Every other session endpoint requires the session token returned at creation, sent as the `X-Session-Token` header. The query-string form (`?session_token=`) is accepted **only on the `/ws/{sessionId}` handshake**, never on the HTTP API — a token in a URL leaks through proxy logs, browser history and `Referer` headers. |
| **MCP** | `POST /mcp` requires `Authorization: Bearer <token>` when MCP is enabled (`mcp.token` / `ISOLA_MCP_TOKEN`, one or more, each ≥ 32 chars). Separate from the API create tokens unless you choose to reuse a secret. |

## Endpoints

| Method | Path | Auth | Description |
| :--- | :--- | :--- | :--- |
| `GET` | `/api/config` | none | Client-visible feature flags and settings (resolutions, transports, version, `create_auth_required` when API token is set, `tor_exit_country` when a Tor exit-country default is configured, `network_log_max_entries` when a capture window is configured, `intel_enabled` for the on-demand recon surface, `capture_enabled` for the resource recorder). |
| `POST` | `/api/sessions` | optional bearer | Create a session. |
| `GET` | `/api/sessions/{id}` | session | Session details, including `cdp_url`. |
| `PATCH` | `/api/sessions/{id}` | session | Extend the session TTL. |
| `POST` | `/api/sessions/{id}/resize` | session | Change the capture resolution. |
| `POST` | `/api/sessions/{id}/audio` | session | Toggle audio capture. |
| `POST` | `/api/sessions/{id}/seed` | session | Push custom data (cookies, request headers, web storage) into the live page. |
| `GET` | `/api/sessions/{id}/graph.png` | session | Render the captured network as a resource-graph PNG (`theme`, `direction`, `expand`, `type`, `failed`, `scale`, `width`, `height`, `origin`, `caption`). |
| `GET` | `/api/sessions/{id}/intel` | session | Recon for a host: passive DNS/ASN enrichment and/or bounded active probes (`host`, `ips`, `mode`, `checks`). |
| `GET` | `/api/sessions/{id}/network` | session | The captured network log: every request with headers, TLS verdict, redirects and tracker labels (`type`, `host_contains`, `url_contains`, `limit`). REST twin of MCP `get_network`. |
| `DELETE` | `/api/sessions/{id}` | session | Terminate the session. |
| `POST` | `/api/sessions/{id}/capture/start` | session | Start recording downloaded resources. Returns `archive_id` and a one-time `archive_key`. |
| `POST` | `/api/sessions/{id}/capture/stop` | session | Stop recording, final pull, totals. |
| `GET` | `/api/sessions/{id}/capture` | session | Live recording status for the session. |
| `GET` | `/api/captures` | optional bearer | List archives (same bearer posture as session creation), plus `usage.used_bytes`, `usage.quota_bytes` (0 = unlimited) and `usage.retention_days` (0 = kept indefinitely). |
| `GET` | `/api/captures/{archiveID}` | archive key | Archive `meta.json`, including every resource row. |
| `GET` | `/api/captures/{archiveID}/blobs/{sha256}` | archive key | One stored body by digest, as an attachment. |
| `GET` | `/api/captures/{archiveID}/files/{path...}` | archive key | One stored body by its readable archive path. |
| `GET` | `/api/captures/{archiveID}/download` | archive key | Zip of `meta.json`, `index.txt` and every stored file at its readable path. |
| `DELETE` | `/api/captures/{archiveID}` | archive key or create bearer | Delete the archive directory. |
| `POST` | `/api/sessions/{id}/shares` | session | Create a view-only share link. |
| `GET` | `/api/sessions/{id}/shares` | session | List share links. |
| `DELETE` | `/api/sessions/{id}/shares/{token}` | session | Revoke a share link. |
| `GET` | `/api/share/{token}` | none | Resolve a share token to its session. |
| `GET` | `/api/openapi.json` | none | OpenAPI 3.0 specification. |
| `WS` | `/ws/{sessionId}` | token (query) | Video and input relay (see [WebSocket](#websocket)). |
| `POST` | `/mcp` | bearer | MCP endpoint (see [Automation](automation.md#mcp-model-context-protocol)). |

> `POST /api/sessions/{id}/share` (singular) is a deprecated alias of `/shares`.

## Create a Session

`POST /api/sessions`

| Field | Type | Notes |
| :--- | :--- | :--- |
| `url` | string | Initial page to load. |
| `resolution` | string | e.g. `"1280x720"`. Capped by `session.max_resolution`. |
| `browser_type` | string | Accepted for compatibility; has no effect (a single browser image is used). |
| `browser_theme` | string | `dark`, `light`, or `auto`; anything else is `400 invalid_browser_theme`. |
| `transport` | string | Ignored; always `isola`. |
| `audio_enabled` | bool | Capture Opus audio. |
| `bandwidth_kbps` | int | Encoder bitrate cap in kbps; `0` derives it from the resolution. Negative values are `400 invalid_bandwidth`. |
| `frame_rate` | int | Capture framerate, `0`–`120` (default `30`); out-of-range values are `400 invalid_frame_rate`. |
| `tor_enabled` | bool | Route the session through Tor. |
| `tor_country` | string | Pin Tor exit nodes to one or more ISO 3166-1 alpha-2 countries (comma-separated, e.g. `"se"` or `"se,no"`). Requires `tor_enabled`; overrides the server default (`tor.exit_country`). Validation is structural (two letters, at most 10 codes); a code with no reachable exit fails the session rather than leaking through another country. |
| `timezone` | string | Page-visible IANA timezone (e.g. `"Europe/Stockholm"`), validated with the host's zoneinfo; unknown names are `400 invalid_timezone`. Overrides the server default (`session.timezone`); empty keeps the deployment default, and with neither set the sandbox stays on UTC. A warm-pool claim applies the requested identity over CDP, so naming one does not force a cold start. |
| `locale` | string | Page-visible BCP-47 tag (e.g. `"sv-SE"`), validated as `^[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*$`; malformed tags are `400 invalid_locale`. Drives `navigator.languages`, `Intl` formatting and `Accept-Language`; the tag is expanded to Chrome's list (`sv-SE` → `sv-SE,sv;q=0.9,en-US;q=0.8,en;q=0.7`). Overrides the server default (`session.locale`); empty keeps the deployment default, and with neither set the sandbox stays on `en-US`. |
| `network_monitor` | bool | Stream CDP network events to the viewer's Network tab. |
| `fingerprint` | string | Client fingerprint, used for rate limiting. |

```bash
curl -s -X POST http://localhost:8080/api/sessions \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com", "resolution": "1280x720", "network_monitor": true}'
```

When `ISOLA_API_TOKEN` is configured:

```bash
API_TOKEN="<from .env or config>"
curl -s -X POST http://localhost:8080/api/sessions \
  -H "Authorization: Bearer $API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com"}'
```

A `401` with `"error":"unauthorized"` means the bearer is missing or wrong. Check `GET /api/config` for `create_auth_required: true`.

A `429` with code `max_sessions_reached` means the instance is at its `ISOLA_MAX_SESSIONS` cap. The response carries a `Retry-After` header (seconds) as a backoff hint, and the cap is published as `max_sessions` in `GET /api/config` (`0` = unlimited). isola allows concurrent sessions by default — each launch adds one and does not terminate the caller's others — so on a shared host set the cap to what the machine can hold (see [Configuration → Cap concurrent sessions](configuration.md#cap-concurrent-sessions)).

Response:

| Field | Notes |
| :--- | :--- |
| `id` | Session id. |
| `session_token` | Owner token — required on every subsequent call and on the control WebSocket. |
| `ws_url` | Relative WebSocket URL for video. |
| `cdp_url` | Compatibility field; resolves to the input channel (`/ws/{id}?channel=input`). It carries Isola's JSON input protocol, not raw CDP — see [Automation](automation.md). |
| `status` | `provisioning`, `running`, `terminated`, or `error`. |
| `bandwidth_kbps`, `transport`, `expires_at`, `max_expires_at`, `remaining_min`, `extend_available` | Session state. |

A session is usable once `status` is `running`.

A launch failure leaves the session in `status: "error"` with a stable `error_code`:

| `error_code` | Meaning |
| :--- | :--- |
| `sandbox_publish_unreachable` | The runtime stopped forwarding published ports, so the backend cannot dial the sandbox. The container itself may be alive; restarting the runtime (e.g. `colima restart`) is the fix. |
| `sandbox_exited` | The sandbox stopped on its own before its streamer answered — a Tor circuit that missed the bootstrap deadline, or a browser process that died. |
| `stream_not_ready` | The container stayed alive but its streamer never answered before the deadline. |
| `create_failed` | Any other launch error. |

`error_code` is absent on healthy sessions.

## Control a Session

```bash
TOKEN=<session_token>

# Extend the TTL (returns expiry, remaining minutes, viewer count)
curl -s -X PATCH http://localhost:8080/api/sessions/$ID -H "X-Session-Token: $TOKEN"

# Change resolution (resizes the sandbox viewport; attached viewers reset)
curl -s -X POST http://localhost:8080/api/sessions/$ID/resize \
  -H "X-Session-Token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"width": 1698, "height": 1018}'

# Toggle audio
curl -s -X POST http://localhost:8080/api/sessions/$ID/audio \
  -H "X-Session-Token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"enabled": true}'

# Terminate
curl -s -X DELETE http://localhost:8080/api/sessions/$ID -H "X-Session-Token: $TOKEN"
```

A resize changes the **sandbox viewport**, not just the encoder: the page reflows
to the new shape, and attached viewers receive a `stream_reset`
(`reason: "resize"`) followed by a fresh `stream_init`. Both dimensions must be
at least 64 and cannot exceed the session's launch resolution, so a request
reshapes that pixel budget rather than growing it. The viewer issues this
automatically when fullscreen or a window resize changes its capture box.

## Send custom data to the page

`POST /api/sessions/{id}/seed` injects cookies, extra request headers and web
storage into a **running** session. The streamer applies it over CDP before
responding; there is no delivery lag and nothing is queued for a later page.

```bash
curl -s -X POST http://localhost:8080/api/sessions/$ID/seed \
  -H "X-Session-Token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"cookies":[{"name":"sid","value":"abc123","same_site":"lax","http_only":true}],"reload":true}'
# -> {"cookies":1,"headers":0,"storage":0,"reloaded":true}
```

| Field | Type | Notes |
| :--- | :--- | :--- |
| `cookies` | array | `name`, `value`, `domain` (or `url`), `path`, `same_site` (`strict`/`lax`/`none`), `secure`, `http_only`, `expires` (unix seconds). With neither `domain` nor `url`, the session target's host is used. `__Host-`/`__Secure-` names are rejected (`400`): Chromium refuses them with a `Domain` attribute, and these are always domain cookies. |
| `headers` | array | `name`, `value`, optional `domains` (suffix match against the page's current main-frame host). The header set is tab-wide: Chromium sends it on every request that tab makes, including third-party subresources. |
| `storage` | array | `kind` (`local` default, or `session`), `origin` (omit for the origin the page is on now), `items` (string map). |
| `reload` | bool | Reload after applying, so cookie/header changes and SPA boot take effect in the same call. Never implied, and the only field a body may contain on its own (reload the page after an earlier call seeded it). |

Limits: 50 cookies, 20 headers, 10 storage origins per call, 64 KiB body,
4096 bytes per value (cookie/header names 256 bytes). Unknown fields are
rejected with `400 invalid_seed`, so a typo like `httpOnly` cannot silently
drop an attribute.

Behaviour worth knowing:

- Owner-only: the call needs `X-Session-Token`. A view-only share link cannot seed, and the viewer hides the control for one. Running sessions only — a session still provisioning answers `409 container_not_ready`.
- Cookies are **domain** cookies: they apply to the domain and its subdomains, from any tab, and survive navigation.
- `same_site: "none"` requires `secure: true`.
- `headers` are per tab (`Network.setExtraHTTPHeaders`, with the `Network` domain enabled on that page on demand): a tab opened later starts clean, and a second call replaces the previous header set. `domains` only gates inclusion against the current main-frame host — it does not scope the header per URL, so a credential header is still sent to third-party hosts the tab loads. The set is dropped when the viewer's input connection re-attaches (e.g. a tab switch); re-run the call to restore it.
- `storage` needs the page to already be on that origin. A mismatch, or a page with no origin (`about:blank`), returns `409 seed_origin_mismatch` — navigate first.
- `local` storage persists across reloads and new tabs. `session` storage lives in the tab, so the streamer also registers an init script that re-applies it on every new document (a reload wipes it first). Each origin keeps one such script, built from every key seeded so far: a later call adds keys (and overwrites the ones it names) instead of dropping the earlier call's. The script belongs to the page target, so a tab switch loses it until the next seed.
- Values are never logged and never stored: the request body is used once and the response carries counts only. Every response — including errors — is `Cache-Control: no-store`.
- Application is sequential — cookies, then headers, then storage, then reload. A failure aborts the remaining steps and returns the error, so an earlier step may already be applied; fix the payload and re-run (the call is idempotent for the same values).
- A forward that exceeds the 10 s streamer budget answers `504 seed_timeout`; an unreachable streamer is `502 seed_failed`, and a detached CDP session is `502 seed_unavailable`.

## Recon intelligence

`GET /api/sessions/{id}/intel` proxies the streamer's recon engine. It is
on-demand only: nothing leaves the sandbox until a caller names a host, and the
caller must own the session (`X-Session-Token`).

| Query | Meaning |
| :--- | :--- |
| `host` | Bare hostname to enrich or probe (required unless `ips` is given). URLs, ports and credentials are rejected. |
| `ips` | Comma-separated addresses to enrich instead of resolving `host`. |
| `mode` | `dns` (passive), `probe` (active checks), `all` (both). Inferred when omitted. |
| `checks` | Comma list or `all`: `robots`, `sitemap`, `securitytxt`, `wellknown`, `favicon`. Default `robots,securitytxt,sitemap`. Every check fetches the host itself; no provider is contacted. |

Passive mode resolves IPs, PTR, the CNAME chain and ASN/org/country (Team Cymru
over DNS — WHOIS on port 43 is outside the sandbox allowlist). Probe mode makes
one bounded GET per check (8 s, 2 MiB, at most three redirects that must stay on
the same registrable domain) and refuses targets that resolve to a private
address. No check sends the hostname to a provider. The proxied reply is capped
at 4 MiB; a larger document is answered with `502 intel_failed` rather than a
truncated `200`, so a client can always trust a successful body to be complete.

```bash
curl -s "http://localhost:8080/api/sessions/$ID/intel?host=example.com&mode=dns" \
  -H "X-Session-Token: $TOKEN"

curl -s "http://localhost:8080/api/sessions/$ID/intel?host=example.com&mode=probe&checks=robots,favicon" \
  -H "X-Session-Token: $TOKEN"
```

The same analysis is already attached to the network log: every entry from
`GET /api/sessions/{id}/network` may carry `headers`, `tls`, `redirects`,
`posture`, `ttfb_ms`, `from_disk_cache`, `from_service_worker`, `origin_host`,
`third_party`, `tracker` and `beacon`. Those fields survive `type`,
`host_contains` and `limit` filtering.

`origin_host` is the page the request belongs to — the tab's main-frame document
host at capture time. An iframe document (consent wall, payment widget) inherits
its parent page instead of replacing it, so grouping by `origin_host` yields the
navigations the user actually made, in visit order.

`tls.security_state` is CDP's own verdict on the connection (`secure`, `info`,
`neutral`, `insecure`, `insecure-broken`, `unknown`). It matters because the
sandbox browser runs with certificate errors ignored: `insecure` means the
certificate was presented but its chain was never trusted. `/posture` carries
the same value as `tls_state`, and an `insecure`/`insecure-broken` or `neutral`
transport caps the letter grade at `C` with an explanatory `note` — header
posture is only as trustworthy as the connection that delivered the headers.

## Network log

`GET /api/sessions/{id}/network` returns everything the sandbox captured for the
session. With any filter it answers `{"entries": [...], "count": N}`; with no
filter it is the streamer's document passed through byte-for-byte (its own
`entries` array, no `count`). Each entry is one request: `id`,
`url`, `method`, `status`, `type`, `mime`, `bytes`, `ip`, `protocol`, `failed`,
`tab`, `ts`, plus the `headers`, `tls` verdict, `redirects`, `posture`,
`origin_host`, `third_party`, `tracker` and `beacon` fields described above.
This is the REST twin of MCP `get_network` — same streamer document, same filter
code — so a script does not need an MCP connection to collect a session's
traffic.

| Query | Meaning |
| :--- | :--- |
| `type` | Only entries of this resource type (`Document`, `Script`, `XHR`, …), case-insensitive. |
| `host_contains` | Only entries whose **hostname** contains this substring. A path or query does not match here. |
| `url_contains` | Only entries whose full URL contains this substring — use it for a path, query or scheme. |
| `limit` | Maximum entries to return after filtering (1–1000). |

Filters are optional. With none of them the reply is the streamer's document
passed through unchanged. With any of them the response is re-serialized as
`entries` + `count`, with every other field of an accepted entry preserved
verbatim. Values that cannot be used (`limit=0`, `limit=ten`, a `type` over 64
bytes, a `host_contains` over 253) are rejected with `400 invalid_network_query`
rather than silently ignored.

The body is buffered and capped at 8 MiB: a larger log answers
`502 network_failed` with an actionable message instead of a truncated `200`, so
a successful reply is always complete. A streamer that cannot be reached is
`502`, one that does not answer within 15 s is `504`. Every response is
`Cache-Control: no-store`.

```bash
# Every document response that came back insecure
curl -s "http://localhost:8080/api/sessions/$ID/network?type=Document" \
  -H "X-Session-Token: $TOKEN" | jq '.entries[] | select((.tls.security_state // "") | startswith("insecure"))'

# Third-party scripts, capped at 50
curl -s "http://localhost:8080/api/sessions/$ID/network?type=Script&url_contains=.js&limit=50" \
  -H "X-Session-Token: $TOKEN"
```

The viewer shows the same data in the **Analysis** panel; the MCP equivalent
tools (`get_headers`, `get_posture`, `get_redirects`, `get_trackers`,
`get_links`) are shaped projections of this log.

## Resource graph image

`GET /api/sessions/{id}/graph.png` renders the session's captured network as a
PNG: the entry block, the pages the capture visited in order, each page's hosts,
their resource-type groups, and the files behind them — the same summary the
viewer's **Graph** tab draws. Hosts are placed by the page that loaded them
(`origin_host`), never by capture order, so a page visited earlier or the search
engine that led to the current one never appears to have been loaded by it. A
page's own subdomains fold into its card (one card per site); a host on another
domain keeps its own. It is rendered by the streamer from its own capture, not
from a screenshot of the page, so it reflects the current window
(`session.network_log_max_entries`, default 1000) even while the tab is loading
or blank.

```bash
# Light theme, every host and group expanded, 3× raster
curl -s "http://localhost:8080/api/sessions/$ID/graph.png?theme=light&expand=all&scale=3" \
  -H "X-Session-Token: $TOKEN" -o graph.png

# Only the failing scripts and stylesheets, flowing top to bottom
curl -s "http://localhost:8080/api/sessions/$ID/graph.png?failed=1&type=script,stylesheet&direction=vertical" \
  -H "X-Session-Token: $TOKEN" -o failures.png
```

The response is `Content-Type: image/png` with `Cache-Control: no-store` and a
`Content-Length` — a live capture is never cached, and a truncated render is an
error rather than a short image. Options are validated by the API before
proxying: an unknown or out-of-range option returns
`400 invalid_graph_options`, whose message carries the reason (for example
`theme must be dark or light`). A failed render, or an image larger than 32 MiB,
returns `502 graph_render_failed`; a render that exceeds 45 seconds returns
`504 graph_render_timeout`. Renders are serialised per session — at most two run
at once, and a further concurrent request gets `429 graph_render_busy` with
`Retry-After`. A capture larger than `width`×`height` at the requested `scale`
is scaled down to fit that frame instead of being rasterised at full size, and
the raster is hard-capped at 8192 px per edge and 24 MP.

| Parameter | Values | Default | Meaning |
| :--- | :--- | :--- | :--- |
| `theme` | `dark`, `light` | `dark` | Image palette. |
| `direction` | `horizontal`, `vertical` | `horizontal` | Flow of the request chain. |
| `expand` | `origin`, `all`, `collapsed` | `origin` | `origin` shows the opened host's type groups, `all` opens every group to its files, `collapsed` draws hosts only. |
| `type` | comma list of `document`, `script`, `stylesheet`, `image`, `font`, `xhr`, `media`, `other` | all | Keep only these resource types. |
| `failed` | `0`, `1` | `0` | Draw only failed requests. |
| `scale` | `1`–`4` | `2` | Raster scale (1× is logical size). |
| `width`, `height` | 320–8000 | `1400`, `900` | Frame the image is fitted into (layout units, multiplied by `scale`), and where long sibling runs wrap. A graph larger than the frame is scaled down to fit; the raster is capped at 8192 px per edge and 24 MP. |
| `origin` | hostname | the opened page | Start the graph from another host. |
| `caption` | `0`, `1` | `1` | Draw the provenance line (origin, counts, theme, UTC time). |

A session whose container is still starting returns `409`
(`container_not_ready`); a terminated session returns `404`. MCP clients get the
same image from the `resource_graph` tool
([Automation → tools](automation.md#tools)).

## Resource capture

With `capture.enabled` (see [Configuration](configuration.md#capture)), the
viewer's **Record** row saves every response body the sandbox downloads and
keeps the archive after the session ends. A session is recorded **once**: the
archive is keyed by the session id, so starting again while recording returns
the same archive and key (idempotent `200`), and starting after a clean Stop
returns `409 archive_exists`. `GET /api/sessions/{id}/capture` before a
recording answers `200` with `recording:false` and zero counters. While a
recording is live, the owner-only status response also carries `archive_key`, so
a viewer that attached mid-recording can browse the files after Stop; the key is
never persisted and disappears with the recording.

```bash
# Start: save the one-time archive key, it is not shown again
curl -s -X POST "http://localhost:8080/api/sessions/$ID/capture/start" \
  -H "X-Session-Token: $TOKEN"
# -> {"archive_id":"<session id>","archive_key":"<64 hex>","started_at":"..."}

# Live progress
curl -s "http://localhost:8080/api/sessions/$ID/capture" \
  -H "X-Session-Token: $TOKEN"

# Stop and pull the remainder
curl -s -X POST "http://localhost:8080/api/sessions/$ID/capture/stop" \
  -H "X-Session-Token: $TOKEN"
# -> {"items":84,"bytes":3128442,"deduped":12,"complete":true}

# After the session is gone, the archive key is the only credential
curl -s "http://localhost:8080/api/captures/$ID" -H "Authorization: Bearer $KEY"
curl -s -OJ "http://localhost:8080/api/captures/$ID/blobs/$SHA256" \
  -H "Authorization: Bearer $KEY"
curl -s -o "$ID.zip" "http://localhost:8080/api/captures/$ID/download" \
  -H "Authorization: Bearer $KEY"
```

`meta.json` records the session, start URL, `created_at`/`ended_at`, `complete`,
counts and sizes, and one row per request: `url`, `final_url`, `method`,
`status`, `mime`, `type`, `size_wire`, `size_decoded`, `sha256`, `sha1`,
`file` (readable path under the archive), `filename`, `tab`, `origin_host`,
`ts_requested`/`ts_finished`/`ts_saved`, cache/service-worker flags, and `state`
(`captured`, `too_large`, `excluded_type`, `missing_body`, `failed`). Rows with
`state != captured` have no stored bytes. The same URL can appear more than
once with different `sha256` values; identical bodies are stored once.

Storage is a readable URL tree under
`$ISOLA_DATA_DIR/captures/<archive_id>/files/<host>/<path>…`, with `index.txt`
(one `url<TAB>file<TAB>status<TAB>mime<TAB>size<TAB>sha256[:12]` line each;
identical repeated requests collapse into one line with a trailing `xN`) and
`meta.json`. `filename` is always the basename of `file`. `GET
/api/captures/{id}/files/{path...}` serves the readable path; `/blobs/{sha256}`
remains as a digest-addressed alias. Both responses are
`Content-Disposition: attachment` with `X-Content-Type-Options: nosniff` — a
captured document is hostile input and must never be rendered on the isola
origin. The viewer therefore previews text/images only, and shows HTML as
source.

Capture cannot see everything by construction: Chromium memory-cache hits,
`blob:`/`data:` payloads, `Media` responses, WebSocket/SSE frames, POST bodies,
and worker/OOPIF traffic are missing. HLS segments fetched by `hls.js` arrive as
XHR and are captured individually. A session terminated before the final pull
(e.g. a container crash) keeps the rows pulled so far and is marked
incomplete.

## Shares

Create a view-only link, list the links, and revoke one:

```bash
curl -s -X POST http://localhost:8080/api/sessions/$ID/shares \
  -H "X-Session-Token: $TOKEN"
# -> {"token":"...","share_url":"http://localhost:8080/share/<token>"}

curl -s http://localhost:8080/api/sessions/$ID/shares -H "X-Session-Token: $TOKEN"
curl -s -X DELETE http://localhost:8080/api/sessions/$ID/shares/<token> \
  -H "X-Session-Token: $TOKEN"
```

A share token grants **view-only** access: it can open the video socket but cannot send input or control the session (attempts return `403`).

## Errors

Errors use a stable envelope:

```json
{ "error": { "code": "invalid_resolution", "message": "resolution exceeds maximum" } }
```

## WebSocket

A single endpoint, `/ws/{sessionId}`, carries both directions of the session. Select direction with query parameters:

| Parameter | Values | Purpose |
| :--- | :--- | :--- |
| `channel` | `isola` (default) or `input` | `isola` = video/state relay; `input` = JSON input relay. |
| `host` | `1` | Marks the connection as the host (allowed to send input). |
| `session_token` | owner token | Full control. |
| `share_token` | share token | View-only; input/control is refused. |
| `admin_token` | admin token | Administrative access. |

```
ws://localhost:8080/ws/<SESSION_ID>?channel=isola&session_token=<TOKEN>
ws://localhost:8080/ws/<SESSION_ID>?channel=input&host=1&session_token=<TOKEN>
```

Video and audio arrive on the `channel=isola` socket as H.264/Opus frames; input on the `channel=input` socket is Isola JSON relayed to the container and applied to the page. Binary packet layout and `stream_init` are in [Transport](transport.md). For building a custom viewer or controller, see [Session WebSocket](automation.md#session-websocket-custom-clients).

The relay dials the container **before** it completes the client handshake. When the sandbox's streamer is unreachable the handshake fails with HTTP `502` and no socket is opened; when the container runtime then confirms the container is gone, the session is finalized immediately rather than on the next health-check round. Retry a failed handshake like any other disconnect (exponential backoff, never a tight loop) and stop once `GET /api/sessions/{id}` reports `terminated` or `error` — a dead session keeps answering `200` with its row until cleanup.

The relay applies per-message and per-write bounds so neither side can pin it:
12 MiB read limit on client messages, 32 MiB on container messages, 1 MiB on
container TEXT (control) frames, a 30 s write deadline on every relayed message,
and a 90 s container read deadline. A client that exceeds a limit, or whose
socket stops accepting writes for 30 s, is disconnected; the relay closes both
sockets together and never leaves the container subscription behind. Idle
viewers are fine — there is no client read deadline, and the 30 s ping is what
proves liveness. See [Transport → Relay lifecycle and limits](transport.md#relay-lifecycle-and-limits).
