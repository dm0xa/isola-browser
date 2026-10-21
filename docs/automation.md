# Automation

Isola exposes two supported automation surfaces:

- **MCP (Model Context Protocol)** — for AI agents that speak MCP. A small, fixed tool set drives isolated sessions. This is the simplest way to let an assistant browse.
- **The session WebSocket** — for custom viewers and controllers that want to send input and receive the stream themselves.

> **Raw CDP / BiDi is not exposed to external clients.** The browser DevTools protocol is used *inside* the session container (the streamer drives Chromium with it), but the API does not offer a passthrough CDP/BiDi WebSocket. If you need scripted browser control, use the [REST API](api.md) plus MCP below. Note that the create-session response still contains a `cdp_url` field for compatibility; it resolves to the input channel described here, not to raw CDP.

## MCP (Model Context Protocol)

The backend serves **Streamable HTTP** at `POST /mcp` on the same port as the UI and API. Enable it and set a token in [`backend/config.yaml`](../backend/config.yaml):

```yaml
mcp:
  enabled: true
  token:
    - "<openssl rand -hex 32>"
    - "<another>"
```

Or via environment: `ISOLA_MCP_ENABLED=true`, `ISOLA_MCP_TOKEN=...` (unprefixed `MCP_ENABLED` / `MCP_TOKEN` also work; comma-separate several tokens). Each token must be **at least 32 characters**; otherwise the backend refuses to start. Use `openssl rand -hex 32`. Any configured token is accepted.

`ISOLA_API_TOKEN` is independent: it gates `POST /api/sessions` for REST clients. MCP `create_session` does not need the API token because it invokes the handler in-process after MCP bearer auth on `/mcp`.

After changing MCP settings, restart the `isola` service (`docker compose up -d --force-recreate isola`) and reload MCP in Cursor.

Every request must send `Authorization: Bearer <token>`. Session ownership is kept server-side when a session is created; MCP clients pass only `session_id` to the tools that follow.

### Tools

| Tool              | Purpose                                                                                                                                |
| ----------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `create_session`  | Start a sandbox (`url`); optional `tor` routes all session traffic through Tor (optionally pinned to exit countries with `tor_country`); waits for CDP and a main-document network entry by default; returns `session_id`, `inspect`, `document_seen`. |
| `navigate`        | Go to a URL (ctrl-socket retries); returns JSON with `inspect` and `document_seen` when `wait_document` is true (default).             |
| `inspect_session` | Compact summary: page title, snapshot node count, document HTTP statuses.                                                              |
| `snapshot`        | Accessibility tree with `ref` values for click/type.                                                                                   |
| `screenshot`      | Visual capture via CDP (`format`: png/jpeg, optional `quality` for jpeg).                                                              |
| `click`           | Click by `ref` from the latest snapshot.                                                                                               |
| `type`            | Type text; optional `ref` to focus first.                                                                                              |
| `get_network`     | Network log; optional `type`, `host_contains`, `limit` filters.                                                                        |
| `resource_graph`  | Render the capture as a resource-graph PNG (hosts → resource types → files); `theme`, `direction`, `expand`, `types`, `failed_only`, `scale` (1–4). Returns the image plus a metadata block; bounded to 45 s and 8 MiB. |
| `get_links`       | Links on the active page (`href` + text, plus page url/title); optional `limit`, `host_contains`. Each link carries a snapshot `ref` when it can be matched, so `click` and `navigate` are interchangeable. |
| `close_session`   | Terminate the session and destroy the container.                                                                                       |

### Tool parameters (common)

| Tool             | Notable arguments                                                                                                                                                 |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `create_session` | `url`; `tor` (default false) routes the session through Tor — the browser has no unproxied egress, so startup waits for a live circuit; `tor_country` (e.g. `se` or `se,no`) pins Tor exits to those ISO 3166-1 alpha-2 countries and requires `tor`; `wait_ready` (default true); `wait_document` (default true). Returns `session_id`, `tor_enabled`, `tor_country` (the effective pin), `automation_ready`, `document_seen`, and `inspect` when checks succeed. |
| `navigate`       | `session_id`, `url`; `wait_document` (default true). Retries while the control socket is not ready (HTTP 503).                                                    |
| `get_network`    | `type` (e.g. `Document`), `host_contains`, `limit`.                                                                                                               |
| `resource_graph` | `theme` `dark` (default) or `light`; `direction` `horizontal`/`vertical`; `expand` `origin` (default) / `all` / `collapsed`; `types` comma list (`document`, `script`, `stylesheet`, `image`, `font`, `xhr`, `media`, `other`); `failed_only`; `scale` 1–4 (default 2). Returns an image block with the PNG and a text block carrying `format`, `bytes`, `theme`, `width`, `height`. Invalid values are rejected before the renderer is called; the call times out after 45 s and refuses a PNG over 8 MiB (retry with `scale=1` or `expand=collapsed`). |
| `get_links`      | `limit` (default 200, max 1000), `host_contains`. Returns `{url, title, count, links:[{href, text, ref}]}`; `href` values are absolute. `ref` is present when the link maps to a snapshot node (refresh happens automatically), so you can `click` the `ref` or `navigate` to the `href`. |
| `screenshot`     | `format` `png` or `jpeg`; `quality` 1–100 for JPEG (default 85). Response includes `data_base64`, `mime`, `width`, `height`.                                      |
| `click` / `type` | `ref` from the **latest** `snapshot` (call `snapshot` again after navigation).                                                                                    |

A `tor` session always cold-starts — the SOCKS proxy is wired at container creation, so it is never served from the [warm pool](configuration.md#warm_pool). Pinning an exit country (`tor_country`) is a strict guarantee: if no reachable exit exists in the country, the session fails to start rather than egressing elsewhere.

Network data comes from the same capture as the in-viewer Network panel, but `get_network` returns the streamer's full in-memory window with fields the UI omits or clips. The window holds the most recent `session.network_log_max_entries` requests (default 1000; see [Configuration → `session`](configuration.md#session)).

### Snapshot, screenshot and graph

- `snapshot` — accessibility tree with stable `ref` ids for automation. Geometry fields may be zero when the AX backend does not expose layout; roles and names are still useful for choosing targets.
- `screenshot` — full visual frame of the active tab (CDP capture). Prefer this when you need to see captchas, layout, or pages that expose little in the AX tree. Payloads are large; use JPEG with a lower `quality` when bandwidth matters.
- `resource_graph` — the captured *requests* rather than the pixels: one PNG of the initiator → host → resource chain, drawn from the same window `get_network` reads. Reach for it when you want to show what a page loaded — a phishing kit's exfiltration host, a page that never rendered, a wall of third-party scripts — or to attach evidence to a report. `expand=origin` (default) is the readable summary; `expand=all` opens every group to its files and can be very wide; `failed_only` keeps just the broken requests. The image is rendered by the streamer, so it works even when the tab shows nothing. A render is bounded to 45 seconds and an 8 MiB payload, so a very large capture should use `scale=1` or `expand=collapsed`.

### Client configuration

```json
{
  "mcpServers": {
    "isola": {
      "url": "http://localhost:8080/mcp",
      "headers": { "Authorization": "Bearer ${env:ISOLA_MCP_TOKEN}" }
    }
  }
}
```

### Example: a session, tool by tool

One tool call is one round trip and each result is JSON text (abbreviated below). The order that works on a page you have never seen: open it, look at what is there, act, then read what the page did.

```text
create_session  {"url":"https://example.com"}
  → {"session_id":"9f1c2b3d-...","url":"https://example.com","automation_ready":true,
     "document_seen":true,
     "inspect":{"page_title":"Example Domain","nodes":41,
                "network":{"entry_count":4,
                           "documents":[{"url":"https://example.com/","status":200,"method":"GET"}]}}}

snapshot        {"session_id":"9f1c2b3d-..."}
  → {"nodes":[{"ref":"e1","role":"RootWebArea","name":"Example Domain"},
              {"ref":"e2","role":"link","name":"More information...","x":184,"y":206,"w":129,"h":20}]}

click           {"session_id":"9f1c2b3d-...","ref":"e2"}
  → ok

type            {"session_id":"9f1c2b3d-...","ref":"e3","text":"isola@example.com"}
  → ok

get_links       {"session_id":"9f1c2b3d-...","limit":20}
  → {"url":"https://example.com/","title":"Example Domain","count":1,
     "links":[{"href":"https://www.iana.org/domains/example","text":"More information...","ref":"e2"}]}

get_network     {"session_id":"9f1c2b3d-...","type":"Document","limit":5}
  → {"entries":[{"url":"https://example.com/","method":"GET","status":200,"type":"Document"}]}

screenshot      {"session_id":"9f1c2b3d-...","format":"jpeg","quality":70}
  → {"format":"jpeg","mime":"image/jpeg","width":1280,"height":720,"data_base64":"/9j/4AAQSk..."}

resource_graph  {"session_id":"9f1c2b3d-...","theme":"light","expand":"all"}
  → {"bytes":184213,"format":"png","height":986,"theme":"light","width":2412}
    + an image/png content block with the graph itself

close_session   {"session_id":"9f1c2b3d-..."}
  → terminated
```

Two things worth knowing:

- **`ref` values are renumbered on every `snapshot`.** After `navigate`, or a click that replaces the page, call `snapshot` again before the next `click` / `type`; an `e2` from an older snapshot points at nothing useful.
- **A filter narrows the fields, not just the rows.** Unfiltered, `get_network` returns the streamer's full window with `id`, `mime`, `ip`, `protocol`, `connection_id`, `bytes`, `sha1`, `failed`, `error` and `tab`; with `type`, `host_contains` or `limit` set, entries come back as only `url`, `method`, `status`, `type`. Drop the filters when you need `connection_id` (the agent-to-agent shape a phishing page often hides) or the source `ip`.

### Calling it without an SDK

Any MCP client works — Cursor, another IDE, an SDK, which all handle the handshake below for you. Three things matter if you speak to `POST /mcp` yourself:

1. **It is a stateful Streamable-HTTP endpoint.** The first request must be `initialize`; that response carries an `Mcp-Session-Id` header which every later request must echo. The endpoint is the same URL throughout — there is no separate SSE URL to discover.
2. **Headers are mandatory.** `Content-Type: application/json` and an `Accept` containing **both** `application/json` and `text/event-stream`, or the server answers `400` before it looks at the JSON.
3. **Replies arrive as SSE.** A response is a `data: {...}` line, not a bare JSON body; a notification (no `id`) gets `202 Accepted` with an empty body.

```bash
TOKEN=<mcp token>

# 1. initialize — read Mcp-Session-Id from the response headers (-D -)
curl -si -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize",
       "params":{"protocolVersion":"2025-06-18","capabilities":{},
                 "clientInfo":{"name":"curl","version":"1"}}}'

# 2. confirm the handshake (202, no body)
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# 3. call a tool — the payload is in the data: line of the SSE reply
curl -s -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -H "Mcp-Session-Id: $SESSION" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
       "params":{"name":"create_session","arguments":{"url":"https://example.com"}}}'
```

`tools/list` returns the same eleven tools with their input schemas, so a client can read the arguments from there rather than hard-coding them. SDKs also send `Mcp-Protocol-Version` with the negotiated revision; the server only insists on that header for the newest protocol revisions.

A working client is about 30 lines — Node 18+, no dependencies (save as `isola-mcp.mjs`; top-level `await` needs an ES module):

```js
const URL = 'http://localhost:8080/mcp';
const TOKEN = process.env.ISOLA_MCP_TOKEN;
let sessionId = '';
let seq = 0;

// One POST per message. Responses are SSE; a notification (no `id`) gets 202
// and no body, so it returns null.
async function post(message) {
  const res = await fetch(URL, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${TOKEN}`,
      'Content-Type': 'application/json',
      Accept: 'application/json, text/event-stream',
      ...(sessionId ? { 'Mcp-Session-Id': sessionId } : {}),
    },
    body: JSON.stringify(message),
  });
  if (!res.ok) throw new Error(`${message.method}: ${res.status} ${await res.text()}`);
  // initialize is the one response that carries the session id every later call must echo.
  sessionId ||= res.headers.get('mcp-session-id') ?? '';
  const sse = await res.text();
  const data = sse.split('\n').find((l) => l.startsWith('data:'));
  return data ? JSON.parse(data.slice(5)) : null;
}

const rpc = (method, params) => post({ jsonrpc: '2.0', id: ++seq, method, params });
// Notifications carry no `id` — with one, the server rejects them as an invalid request.
const notify = (method, params) => post({ jsonrpc: '2.0', method, params });

// Tools answer with text; most of it is JSON, but click/type return plain "ok"
// and close_session returns "terminated".
async function call(name, args = {}) {
  const msg = await rpc('tools/call', { name, arguments: args });
  if (msg?.error) throw new Error(`${name}: ${JSON.stringify(msg.error)}`);
  const text = msg.result.content.map((c) => c.text).join('\n');
  try { return JSON.parse(text); } catch { return text; }
}

await rpc('initialize', {
  protocolVersion: '2025-06-18',
  capabilities: {},
  clientInfo: { name: 'example', version: '1' },
});
await notify('notifications/initialized');

const { session_id } = await call('create_session', { url: 'https://example.com' });
console.log(await call('snapshot', { session_id }));
await call('close_session', { session_id });
```

`close_session` is the one tool a client must not skip: it destroys the container. If a crash loses the `session_id`, the session still expires on its own TTL and the store's reaper destroys the container.

## Session WebSocket (Custom Clients)

`GET /ws/{sessionId}` upgraded to WebSocket carries both directions. Open one socket with `channel=isola` for video/audio and a second with `channel=input` for input — they are separate so input is never blocked behind video. Both need a token (see [API → WebSocket](api.md#websocket)). The video socket's binary packets and `stream_init` / `stream_reset` are documented in [Transport](transport.md).

The input socket speaks Isola's JSON input protocol. The bundled viewer is built on exactly these messages, so the protocol is stable enough to build on:

### Input events

| Message                                                                     | Effect                                               |
| --------------------------------------------------------------------------- | ---------------------------------------------------- |
| `{"type":"mousemove","x":..,"y":..,"buttons":..}`                           | Move the pointer (`buttons` bitmask: L=1, R=2, M=4). |
| `{"type":"mousedown"|"mouseup","x":..,"y":..,"button":"left|right|middle"}` | Press/release a button.                              |
| `{"type":"mousewheel","deltaX":..,"deltaY":..}`                             | Scroll.                                              |
| `{"type":"type","text":"..."}`                                              | Type text.                                           |
| `{"type":"chord","combo":"ctrl+a"}`                                         | Send a key chord.                                    |
| `{"type":"clipboard_set"|"clipboard_sync"}`                                 | Set/sync the clipboard.                              |

### Control and tab messages

Some messages get a reply on the same socket:

| Message                                                                                            | Reply                               |
| -------------------------------------------------------------------------------------------------- | ----------------------------------- |
| `{"type":"get_selection"}`                                                                         | `{"type":"selection","text":"..."}` |
| `{"type":"select_all"}`                                                                            | —                                   |
| `{"type":"select_range","x1":..,"y1":..,"x2":..,"y2":..}`                                          | —                                   |
| `{"type":"tab_new"|"tab_switch"|"tab_close"|"tab_navigate"|"tab_back"|"tab_reload"|"tab_recover"|"get_tabs"}` | Tab state                           |

### Server pushes

The socket is not only a request channel — the sandbox reports state on it as it happens. Unknown message types are ignored, so a client can safely skip pushes it does not understand.

| Message | Meaning |
| :--- | :--- |
| `{"type":"keepalive"}` | Liveness, sent periodically. |
| `{"type":"selection","text":"..."}` | Reply to `get_selection`. |
| `{"type":"tabs","tabs":[{"id":1,"url":"...","title":"...","active":true}]}` | Tab strip state — after any tab command and once on attach. |
| `{"type":"net_init","entries":[...]}` | Replay of everything captured before this client attached. |
| `{"type":"net","e":{...}}` | One network entry as it completes: `id`, `url`, `m`, `s`, `t`, `ip`, `b` (bytes), `f` (failed), `e` (error), `tb` (browser tab), `ts`, `h` (SHA-1 of the body), `ut` (the URL was clipped to the wire budget). |
| `{"type":"console","l":"error","t":"...","tb":1,"ts":...}` | Console line with its level and tab (text capped at 500 chars). `i` is a stable line id that survives a tab-attribution re-publish. |
| `{"type":"cookie","n":"name","v":"...","d":"domain","p":"/","ss":"Lax","exp":"...","h":true,"s":true}` | A cookie was set or changed. |
| `{"type":"cursor","c":"text","u":"https://..."}` | Computed cursor shape under the pointer, plus the hovered URL. |
| `{"type":"renderer_stall","stalled":true,"ms":...,"recoveries":...}` | The sandbox replaced a wedged renderer, or recovered from one. |
| `{"type":"clipboard_sync","text":"..."}` | The sandbox clipboard changed (a copy happened inside the page). |

The `net` / `net_init` / `console` / `cookie` pushes exist only for a session created with `network_monitor`, and on a long-lived session they cover the most recent `session.network_log_max_entries` requests (default 1000) and 300 console lines.

### Example: headless controller (input only)

Driving a session without decoding a single frame — navigate, click, type, read the result. This uses the [`ws`](https://www.npmjs.com/package/ws) package (`npm i ws`).

Non-browser clients must set `Origin` to the host serving Isola: the relay refuses a handshake with a missing or foreign origin, which is what stops a random page from hijacking a local session ([Security](security.md)).

```js
import WebSocket from 'ws';

const BASE = 'http://localhost:8080';

// A fixed resolution makes click coordinates predictable without a video socket.
const session = await (await fetch(`${BASE}/api/sessions`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },   // add Authorization: Bearer <api token> if ISOLA_API_TOKEN is set
  body: JSON.stringify({ url: 'about:blank', resolution: '1280x720' }),
})).json();
const { id, session_token } = session;

const ws = new WebSocket(
  `ws://localhost:8080/ws/${id}?` + new URLSearchParams({
    channel: 'input', host: '1', session_token,
  }),
  { headers: { Origin: BASE } },
);

const send = (msg) => ws.send(JSON.stringify(msg));
const waitFor = (type, ms = 15000) => new Promise((resolve, reject) => {
  const timer = setTimeout(() => reject(new Error(`timeout waiting for ${type}`)), ms);
  const onMessage = (raw) => {
    const msg = JSON.parse(raw);
    if (msg.type !== type) return;
    clearTimeout(timer);
    ws.off('message', onMessage);
    resolve(msg);
  };
  ws.on('message', onMessage);
});

await new Promise((resolve) => ws.once('open', resolve));

// tab_navigate drives the active tab and has no reply — wait for the `tabs`
// push that shows the new URL (the attach-time push can arrive first).
const waitForTab = async (needle) => {
  for (;;) {
    const { tabs } = await waitFor('tabs');
    const hit = tabs.find((t) => t.url.includes(needle));
    if (hit) return hit;
  }
};

send({ type: 'tab_navigate', url: 'https://example.com' });
console.log(await waitForTab('example.com'));

// A click is a press and a release, in capture coordinates (0,0 = top left).
send({ type: 'mousemove', x: 186, y: 216, buttons: 0 });
send({ type: 'mousedown', x: 186, y: 216, button: 'left' });
send({ type: 'mouseup', x: 186, y: 216, button: 'left' });

send({ type: 'type', text: 'hello@example.com' });
send({ type: 'chord', combo: 'ctrl+a' });
send({ type: 'get_selection' });
console.log((await waitFor('selection')).text);

ws.close();
```

Coordinates are in **capture** space: with `resolution: '1280x720'` a click at `x:186` is 186 px from the left of the *page*, whatever size the window shows it at. A client that also opens the video socket should use `capture_width` / `capture_height` from `stream_init` instead, since ABR can shrink the encoded surface without changing the capture.

### Example: minimal custom viewer (browser)

The bundled viewer is a few thousand lines because it also does ABR, stall recovery, cursor shaping and the tab strip. A viewer that paints and clicks is ~50. The packet layout, `stream_init` fields and the markers are in [Transport](transport.md#binary-packets-channelisola).

```js
const ID = '<session id>', TOKEN = '<session token>';

const socket = (channel, extra = {}) => new WebSocket(
  `ws://${location.host}/ws/${ID}?` + new URLSearchParams({
    channel, host: '1', session_token: TOKEN, ...extra,
  }),
);

const canvas = document.querySelector('canvas');
const ctx = canvas.getContext('2d');
let capture = { w: 1280, h: 720 };
let decoder, ts = 0;

// Video: ask for Annex-B on the handshake — `mode=webcodecs` is a query
// parameter, not a first message.
const video = socket('isola', { mode: 'webcodecs' });
video.binaryType = 'arraybuffer';
video.onmessage = ({ data }) => {
  if (typeof data === 'string') {
    const msg = JSON.parse(data);
    if (msg.type === 'stream_init') {
      capture = { w: msg.capture_width, h: msg.capture_height };
      canvas.width = msg.width;                 // encoded surface — may be scaled by ABR
      canvas.height = msg.height;
      decoder?.close();
      decoder = new VideoDecoder({
        output: (frame) => { ctx.drawImage(frame, 0, 0, canvas.width, canvas.height); frame.close(); },
        error: () => { decoder = undefined; },  // next stream_init rebuilds it
      });
      decoder.configure({ codec: msg.codec, optimizeForLatency: true });
    } else if (msg.type === 'stream_reset') {
      decoder?.close();
      decoder = undefined;                      // wait for the following stream_init
    }
    return;
  }
  const view = new DataView(data);
  if (new Uint8Array(data, 0, 1)[0] !== 0x02) return;   // 0x01 fMP4, 0x03 Opus audio
  if (!decoder) return;
  decoder.decode(new EncodedVideoChunk({
    type: (new Uint8Array(data, 1, 1)[0] & 1) ? 'key' : 'delta',   // header flag bit 0
    timestamp: ts += 33333,                     // µs; only has to increase
    data: new Uint8Array(data, 10, view.getUint32(6, true)),
  }));
};

// Input: its own socket, in capture coordinates.
const input = socket('input');
const send = (msg) => input.readyState === WebSocket.OPEN && input.send(JSON.stringify(msg));
const toPage = (e) => {
  const r = canvas.getBoundingClientRect();
  return {
    x: Math.round((e.clientX - r.left) * capture.w / r.width),
    y: Math.round((e.clientY - r.top) * capture.h / r.height),
  };
};

canvas.addEventListener('mousemove', (e) => send({ type: 'mousemove', ...toPage(e), buttons: e.buttons }));
canvas.addEventListener('mousedown', (e) => send({ type: 'mousedown', ...toPage(e), button: 'left' }));
canvas.addEventListener('mouseup', (e) => send({ type: 'mouseup', ...toPage(e), button: 'left' }));
canvas.addEventListener('wheel', (e) => send({ type: 'mousewheel', deltaX: e.deltaX, deltaY: e.deltaY, ...toPage(e) }), { passive: true });
canvas.addEventListener('keydown', (e) => {
  const mods = [
    e.ctrlKey || e.metaKey ? 'ctrl' : '',
    e.shiftKey ? 'shift' : '',
    e.altKey ? 'alt' : '',
  ].filter(Boolean);
  // Single printable characters ride `type` (so IME and autocorrect stay out of it);
  // everything else is a chord: 'ctrl+a', 'shift+Tab', 'Enter', 'ArrowLeft'.
  if (e.key.length === 1 && mods.length === 0) send({ type: 'type', text: e.key });
  else send({ type: 'chord', combo: [...mods, e.key].join('+') });
});
canvas.tabIndex = 0;   // focusable, or keydown never fires
```

Notes on the edges:

- **Audio is not a third channel.** Turn it on with `POST /api/sessions/{id}/audio` (`{"enabled":true}`); Opus packets then arrive on the video socket as marker `0x03`.
- **A stalled decoder is fixed in place.** Send `{"type":"request_keyframe"}` on the video socket instead of reconnecting the session; the streamer replaces the encoder run and you get `stream_reset` + a fresh `stream_init`.
- **The surface can change under you.** `POST /api/sessions/{id}/resize` (and a session restart) changes the capture; an ABR step changes the encoded `width` / `height`. Re-read both pairs on every `stream_init` rather than caching them once.
- **`host=1` marks a socket as the host connection** — the bundled viewer sends it on both, and it keeps that connection out of the session's viewer count. Permission, however, comes from the token: a view-only share token gets `403` on `channel=input` / `host=1`, and on the video socket the relay forwards only `ping` and `request_keyframe`, so a viewer cannot even ask for a bitrate change.
- **Input belongs on the input socket.** The video socket accepts some input types as a fallback, but a click sent there waits behind video frames.

The reference implementation of all of this is [`frontend/src/hooks/useIsolaStream.ts`](../frontend/src/hooks/useIsolaStream.ts), and the relay that forwards both channels is [`backend/pkg/wsproxy/proxy.go`](../backend/pkg/wsproxy/proxy.go).
