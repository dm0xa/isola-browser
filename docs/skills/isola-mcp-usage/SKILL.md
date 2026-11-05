---
name: isola-mcp-usage
description: "How to drive isola over MCP: endpoint and handshake, the full tool catalog with exact arguments, recipes for browsing/inspection/evidence, and the REST route map when MCP is unavailable."
---

# Driving isola over MCP

isola is disposable browser sandboxes. `POST /mcp` (Streamable HTTP) exposes one
fixed tool set that boots a throwaway Chromium container, lets you navigate,
read and click, inspect everything the page loaded, and destroys the container on
close. Every analysis surface in the viewer exists as a tool, so a script sees
the same numbers as a human.

Use it for: rendering a page you can't fetch, screenshots/visual proof, clicking
through a flow, and network/OSINT evidence (hosts, headers, TLS, redirects,
trackers, DNS, robots/security.txt, request graphs). For plain HTML, a normal
HTTP client is cheaper.

## 1. Connect

Endpoint: `http://<host>:8080/mcp` — same origin as UI/API, no separate SSE URL.

Auth: every request needs `Authorization: Bearer <mcp token>`. MCP is opt-in;
server side it is `mcp.enabled` + `mcp.token` (env `ISOLA_MCP_ENABLED`,
`ISOLA_MCP_TOKEN`; tokens ≥ 32 chars or startup fails). Disabled → 404, missing
or wrong bearer → 401. `ISOLA_API_TOKEN` does not apply to MCP calls.

Client config (Cursor/IDE, SDKs handle the handshake):

```json
{ "mcpServers": { "isola": { "url": "http://localhost:8080/mcp",
  "headers": { "Authorization": "Bearer ${env:ISOLA_MCP_TOKEN}" } } } }
```

Speaking HTTP directly — three rules:

1. Stateful: first request is `initialize`; take `Mcp-Session-Id` from the
   response header and echo it on every later request.
2. Send `Content-Type: application/json` **and** `Accept: application/json,
   text/event-stream`; otherwise 400 before the JSON is parsed.
3. Responses are SSE: parse the `data: {...}` line. A notification (no `id`,
   e.g. `notifications/initialized`) returns `202` with an empty body.

```bash
TOKEN=...; H=(-H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream")
curl -si -X POST localhost:8080/mcp "${H[@]}" -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
  "params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cli","version":"1"}}}'
# -> Mcp-Session-Id: <sid>
curl -s -o /dev/null -X POST localhost:8080/mcp "${H[@]}" -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
curl -s -X POST localhost:8080/mcp "${H[@]}" -H "Mcp-Session-Id: $SID" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
       "params":{"name":"create_session","arguments":{"url":"https://example.com"}}}'
```

`tools/list` returns all tool input schemas — prefer it over hard-coding
arguments. Tool results are JSON text, except `click`/`type` ("ok") and
`close_session` ("terminated"). A failing tool returns `isError` with an
actionable message (bad ref, session gone, too many bytes).

## 2. The loop that always works

`create_session` → `inspect_session`/`snapshot` → act (`click`/`type`/
`navigate`) → `get_network`/`get_headers`/… → `close_session`.

Each session is one UUID; tools take only `session_id` (ownership is stored
server-side). A tool on a missing or non-running session fails with
"session not found" / "session is not running".

## 3. Tools

### Session lifecycle

| Tool | Arguments | Returns |
|---|---|---|
| `create_session` | `url`; `tor` (false), `tor_country` ("se" or "se,no", requires `tor`), `wait_ready` (true), `wait_document` (true) | `session_id`, `url`, `tor_enabled`, `tor_country`, `automation_ready`, `document_seen`, `inspect` |
| `navigate` | `session_id`, `url`; `wait_document` (true) | `inspect` + `document_seen`; retries while the control socket is still connecting (503) |
| `close_session` | `session_id` | `terminated`; destroys the container |

- `create_session` waits for CDP readiness and a main-document network entry by
  default. If the session never becomes usable it terminates it and returns the
  error, so a failed launch leaves no orphan.
- `tor: true` forces a cold start (never warm-pooled), waits for a live circuit,
  and gives the browser **no** unproxied egress. `tor_country` is strict: no
  reachable exit in that country = launch fails rather than leaking.
- Always call `close_session`. A crashed client still has its session expire on
  TTL, but the container burns resources until then.

### Reading the page

| Tool | Arguments | Returns |
|---|---|---|
| `inspect_session` | `session_id` | compact `{page_title, nodes, network:{entry_count, documents:[{url,status,method}]}}`, plus best-effort `posture:{score,total,grade}` (newest document) and `trackers:{third_party_hosts,beacons,trackers}` when the streamer supports them |
| `snapshot` | `session_id` | accessibility tree nodes `{ref, role, name, x, y, w, h}` (geometry may be 0) |
| `get_links` | `session_id`; `limit` (200, max 1000), `host_contains` (matches the link URL) | `{url, title, count, links:[{href, text, ref}]}` — `href` absolute, `ref` present when matchable |
| `screenshot` | `session_id`; `format` `png` (default) or `jpeg`, `quality` 1–100 (85) | image block + `{format, mime, width, height, bytes}` |

- `snapshot` is the automation view; `screenshot` is the human view. Use
  `screenshot` for captchas, layout, canvas-only pages; use `snapshot` to pick
  click targets.
- `ref` values are **renumbered by every snapshot**. After `navigate` or a click
  that replaces the page, snapshot again before the next `click`/`type`.
- `get_links` is the crawl primitive: either `click` its `ref` or `navigate` to
  its `href`.

### Acting

| Tool | Arguments |
|---|---|
| `click` | `session_id`, `ref` (from the latest snapshot) |
| `type` | `session_id`, `text`; optional `ref` to focus first |

- `type` inserts text only (`Input.insertText`); no key events or Enter are
  exposed. To submit, snapshot after typing and `click` the submit button's ref.
- Both `click` and `type` resolve the ref to its accessibility **centre** and
  click there. A ref whose geometry is 0,0 (some AX backends omit layout) will
  mis-target — confirm with `screenshot` and prefer `navigate`/`get_links`.

### Network and evidence

All of these read the same capture as the viewer's Network panel, but with
unclipped fields. The window holds the most recent
`session.network_log_max_entries` requests (default 1000).

| Tool | Arguments | What it gives you |
|---|---|---|
| `get_network` | `session_id`; `type` (e.g. `Document`, `Script`), `host_contains` (hostname substring only), `url_contains` (anywhere in URL), `limit` | every captured request: `url`, `method`, `status`, `type`, `ip`, `protocol`, `connection_id`, `bytes`, `sha1`, `failed`, `error`, `tab`, `origin_host`, `headers`, `tls`, `redirects`, `posture`, `third_party`, `tracker` |
| `get_headers` | `session_id`; `host_contains`, `url_contains`, `type`, `documents_only` (true), `limit` (50), `include_tls` (true) | response headers + TLS cert summary per request |
| `get_posture` | `session_id`; `host` (URL substring filter) | per-document security-header audit: HSTS, CSP, frame protection, nosniff, Referrer-Policy, Permissions-Policy, COOP/COEP, 0–7 score, A–F grade; `tls_state`/`note` drive the transport verdict (untrusted/plaintext caps the grade at C) |
| `get_redirects` | `session_id`; `host_contains`, `url_contains`, `documents_only` (true), `limit` (20) | redirect chains with each hop's status |
| `get_trackers` | `session_id`; `third_party_only`, `include_beacons` (true), `include_cookies` (true), `limit` (100) | per-host counts/bytes, known tracker families, beacon count, cookie-family fingerprints |
| `host_intel` | `session_id`; `host` (omit = most recent document host), `include_cname` (true) | passive DNS: IPs, PTR, ASN/org/country, CNAME chain. Contacts no provider and never the target |
| `probe_host` | `session_id`; `host` (omit = document host), `checks` `robots`,`securitytxt`,`sitemap`,`wellknown`,`favicon` (default first three) | active recon from inside the sandbox, one bounded fetch per check; favicon returns mmh3 + SHA-1. Requires `intel.enabled` (`ISOLA_INTEL_ENABLED`) |
| `resource_graph` | `session_id`; `theme` `dark`/`light`, `direction` `horizontal`/`vertical`, `expand` `origin`/`all`/`collapsed`, `types` comma list (`document,script,stylesheet,image,font,xhr,media,other`), `failed_only`, `scale` 1–4 (2) | PNG of the capture tree: request → pages (visit order) → hosts → resource types → files, plus a metadata text block |

Gotchas that cost real calls:

- `host_contains` is **hostname only** on `get_network`/`get_headers`/
  `get_redirects`; a path, query or scheme silently matches nothing there. Put
  the path in `url_contains`. `get_links` is the exception.
- A filter narrows rows, not fields: a `limit`-ed `get_network` entry is still
  complete, so `headers`/`tls`/`posture`/`connection_id` are safe to read.
- `get_network` and `resource_graph` refuse payloads over 8 MiB with an
  actionable error; `resource_graph` also has a 45 s deadline. Retry with
  `limit`, `expand=collapsed`, `types=`, or `scale=1` — never assume truncation.
- Host→host traffic is not a causal chain: `origin_host` is the main-frame
  document host that loaded an entry (iframes inherit it), and pages keep visit
  order. Don't claim "page X loaded host Y" beyond those fields.

## 4. Recipes

**Triage an unknown page**

```
create_session {url}
inspect_session {}
get_links {limit: 40}
get_network {type: "Document", limit: 10}      # did it load, and with what status?
screenshot {format: "jpeg", quality: 70}       # confirm what actually rendered
close_session {}
```

**Who is behind it / is it sketchy**

```
get_network {limit: 200}                    # third_party, tracker, connection_id, ip
get_trackers {third_party_only: true}
get_redirects {documents_only: true}
host_intel {}                               # most recent document host
get_posture {}                              # header grade + TLS verdict
probe_host {checks: "robots,securitytxt,sitemap"}
```

**Evidence for a report**

`resource_graph {theme: "light", expand: "origin"}` for the request tree, plus
`screenshot` for the pixels, plus `get_network` rows you can cite verbatim
(`ip`, `sha1`, `status`, `origin_host`).

**Click through a flow**

`snapshot` → `click {ref}` → `snapshot` (new page = new refs) →
`type {ref, text}` → `snapshot` → `click` the submit ref → `get_network` to see
what the interaction actually sent.

**Anonymous / geo-pinned research**

`create_session {url, tor: true, tor_country: "se"}` — cold start, slower, and
the failure mode is a launch error, not a silent direct connection.

## 5. REST routes (when MCP is not enabled)

Same origin, JSON, under `/api`. `GET /api/openapi.json` is the field-level
source of truth.

| Method | Path | Auth |
|---|---|---|
| `GET` | `/api/config` | none — feature flags, resolutions, `create_auth_required`, `max_sessions`, `intel_enabled` |
| `POST` | `/api/sessions` | optional bearer (`ISOLA_API_TOKEN`) |
| `GET`/`PATCH`/`DELETE` | `/api/sessions/{id}` | `X-Session-Token` — details / extend TTL / terminate |
| `POST` | `/api/sessions/{id}/resize` | `X-Session-Token` — change capture resolution |
| `POST` | `/api/sessions/{id}/audio` | `X-Session-Token` — toggle audio |
| `GET` | `/api/sessions/{id}/graph.png` | `X-Session-Token` — resource graph PNG |
| `GET` | `/api/sessions/{id}/intel` | `X-Session-Token` — passive DNS + active probes |
| `POST`/`GET`/`DELETE` | `/api/sessions/{id}/shares[/{token}]` | `X-Session-Token` — view-only share links |
| `GET` | `/api/share/{token}` | none |
| `WS` | `/ws/{sessionId}` | `session_token` query (handshake only) |

`POST /api/sessions` returns `id` + `session_token`; keep the token for every
later call. `429 max_sessions_reached` means the instance cap (`ISOLA_MAX_SESSIONS`)
is hit — honour `Retry-After`. Never put `session_token` in an HTTP URL; only the
WebSocket handshake accepts the query form. There is no raw CDP/BiDi passthrough
(`cdp_url` resolves to isola's input channel, not DevTools).
