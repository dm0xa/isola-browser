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
| `GET` | `/api/config` | none | Client-visible feature flags and settings (resolutions, transports, version, `create_auth_required` when API token is set, `tor_exit_country` when a Tor exit-country default is configured, `network_log_max_entries` when a capture window is configured). |
| `POST` | `/api/sessions` | optional bearer | Create a session. |
| `GET` | `/api/sessions/{id}` | session | Session details, including `cdp_url`. |
| `PATCH` | `/api/sessions/{id}` | session | Extend the session TTL. |
| `POST` | `/api/sessions/{id}/resize` | session | Change the capture resolution. |
| `POST` | `/api/sessions/{id}/audio` | session | Toggle audio capture. |
| `GET` | `/api/sessions/{id}/graph.png` | session | Render the captured network as a resource-graph PNG (`theme`, `direction`, `expand`, `type`, `failed`, `scale`). |
| `DELETE` | `/api/sessions/{id}` | session | Terminate the session. |
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
| `browser_theme` | string | `dark`, `light`, or `auto`. |
| `transport` | string | Ignored; always `isola`. |
| `audio_enabled` | bool | Capture Opus audio. |
| `bandwidth_kbps` | int | Encoder bitrate cap in kbps; `0` derives it from the resolution. |
| `frame_rate` | int | Capture framerate (default `30`). |
| `tor_enabled` | bool | Route the session through Tor. |
| `tor_country` | string | Pin Tor exit nodes to one or more ISO 3166-1 alpha-2 countries (comma-separated, e.g. `"se"` or `"se,no"`). Requires `tor_enabled`; overrides the server default (`tor.exit_country`). |
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
| `share_token`, `share_url` | Present when a share was created. |
| `status` | `provisioning`, `running`, `terminated`, or `error`. |
| `bandwidth_kbps`, `transport`, `expires_at`, `max_expires_at`, `remaining_min`, `extend_available` | Session state. |

A session is usable once `status` is `running`.

## Control a Session

```bash
TOKEN=<session_token>

# Extend the TTL (returns expiry, remaining minutes, viewer count)
curl -s -X PATCH http://localhost:8080/api/sessions/$ID -H "X-Session-Token: $TOKEN"

# Change resolution
curl -s -X POST http://localhost:8080/api/sessions/$ID/resize \
  -H "X-Session-Token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"resolution": "1920x1080"}'

# Toggle audio
curl -s -X POST http://localhost:8080/api/sessions/$ID/audio \
  -H "X-Session-Token: $TOKEN" -H "Content-Type: application/json" \
  -d '{"enabled": true}'

# Terminate
curl -s -X DELETE http://localhost:8080/api/sessions/$ID -H "X-Session-Token: $TOKEN"
```

## Resource graph image

`GET /api/sessions/{id}/graph.png` renders the session's captured network as a
PNG: the opened host, its resource-type groups, and the files behind them. The
image is produced by the streamer (which holds the capture) and is never cached,
so it always reflects the current window.

```bash
# Light theme, every host and group expanded, 3× raster
curl -s "http://localhost:8080/api/sessions/$ID/graph.png?theme=light&expand=all&scale=3" \
  -H "X-Session-Token: $TOKEN" -o graph.png

# Only the failing scripts and stylesheets, flowing top to bottom
curl -s "http://localhost:8080/api/sessions/$ID/graph.png?failed=1&type=script,stylesheet&direction=vertical" \
  -H "X-Session-Token: $TOKEN" -o failures.png
```

| Parameter | Values | Default | Meaning |
| :--- | :--- | :--- | :--- |
| `theme` | `dark`, `light` | `dark` | Image palette. |
| `direction` | `horizontal`, `vertical` | `horizontal` | Flow of the request chain. |
| `expand` | `origin`, `all`, `collapsed` | `origin` | `origin` shows the opened host's type groups, `all` opens every group to its files, `collapsed` draws hosts only. |
| `type` | comma list of `document`, `script`, `stylesheet`, `image`, `font`, `xhr`, `media`, `other` | all | Keep only these resource types. |
| `failed` | `0`, `1` | `0` | Draw only failed requests. |
| `scale` | `1`–`4` | `2` | Raster scale (1× is logical size). |
| `width`, `height` | 320–8000 | `1400`, `900` | Layout hints that decide where long sibling runs wrap. |
| `origin` | hostname | the opened page | Start the graph from another host. |
| `caption` | `0`, `1` | `1` | Draw the provenance line (origin, counts, theme, UTC time). |

Invalid options return `400` with the streamer's reason; a session whose
container is still starting returns `409` (`container_not_ready`). The same
image is available to MCP clients as the `resource_graph` tool.

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
