# Isola transport

Isola is a purpose-built remote-browser wire: **offscreen Chromium paint → H.264 → WebSocket → WebCodecs canvas**. There is no VNC, no WebRTC, no X11 desktop, and no peer-to-peer session. One container encodes; Isola relays; the viewer decodes in the page that already talks to the API.

That shape is the product. A disposable sandbox wants a picture, a click, and a teardown — not a desktop protocol and not a conferencing stack. The rest of this page is the how.

Input JSON is listed in [Automation → Session WebSocket](automation.md#session-websocket-custom-clients). Frozen-picture recovery is in [Troubleshooting](troubleshooting.md#the-stream-is-blank-or-frozen) and the [README](../README.md#frozen-pictures-heavy-pages-capture-stalls).

## Why This Wire

Remote browsers usually inherit someone else's protocol:

| Stack | What you inherit | Why Isola does not |
| :--- | :--- | :--- |
| **VNC / RFB** | A framebuffer of a real X session, x11vnc, extra hops, DES-era auth, no hardware encode in the viewer. | There is no desktop. Electron paints offscreen; Isola never owns an X display. |
| **WebRTC** | ICE, STUN/TURN, DTLS-SRTP, SDP, a peer-connection lifetime, and a media stack sized for calls. | The viewer and the encoder already share a server. NAT traversal is solving a problem Isola does not have. One container fans one bitstream out to every watcher; a WebRTC peer per viewer would encode N times. |

Isola keeps the parts that matter for a **hosted Chromium**: damage-rect capture, one H.264 encode, a tiny binary packet, and JSON input on a second socket so a video write cannot stall a click.

What that buys in practice:

- **Same origin as the UI and the API** — Video is `/ws/{sessionId}` on port 8080. No extra media port, no TURN credential, no VNC password.
- **One encoder per quality, not per viewer** — A share-link watcher attaches to the run that already exists, and a session runs at most two runs, so N viewers on N ABR rungs cost two encoders rather than N.
- **Decode in the browser that already has WebCodecs** — Annex-B access units go to `VideoDecoder` → canvas. MSE/fMP4 is only the fallback.
- **Clicks land in the frame that painted them** — including cross-origin embeds (a video embed inside another page), because input is CDP on the iframe target — not a synthetic X click on a compositor.

## Path

```
Electron offscreen paint
        │  ISOL frames on a Unix socket
        ▼
isola-streamer  ── ffmpeg H.264 ──► shared encoder run
        │
        │  WebSocket :5900 (loopback)
        ▼
Isola process (relay)
        │
        │  /ws/{sessionId}
        ▼
viewer  ── WebCodecs (Annex-B) or MSE (fMP4)
```

One container serves one session. `isola-streamer` keeps **one ffmpeg process per (bitrate, scale, decode mode)** and fans that bitstream out to every viewer on that run. A share-link watcher therefore does not start a second encoder.

At most **two** encoder runs are live per session (`maxConcurrentRuns` in `stream_hub.go`). The ABR ladder gives one viewer four configurations, so three viewers stepping it differently used to mean three libx264 processes in a three-CPU container — three times the encode cost plus capture, compose and mux, which starves the frame pump and turns an adaptive step into a stall. Two runs is what a host and one share watcher need on different rungs; a third configuration shares a live run instead of starting a third encoder. Runs are never shared across decode modes or capture geometries: a viewer whose container format or geometry matches no live run gets its own encoder regardless of the cap, because the alternative is a picture it cannot decode.

Inside the container the capture never leaves the box: Electron writes damage rectangles on `/run/isola/frame.sock` (`ISOL` magic). The streamer composites them and feeds ffmpeg. That socket is not reachable from the host or the viewer. Streamer `:5900` is published to loopback only; the relay is the only client.

### Capture (`ISOL`)

The producer does not ship a full BGRA surface every tick. Each frame is a 32-byte header (`ISOL` magic, surface size and stride, dirty-rect count, and a `ts_ns` timestamp) plus the damaged rectangles and their pixels. `ts_ns` is wall-clock UTC nanoseconds (`CLOCK_REALTIME`), so a consumer can subtract its own clock to get a buffer delay; the streamer parses it into the frame's timestamp, but nothing downstream uses it — the encoder is fed `-f rawvideo`, which carries no PTS, so ffmpeg synthesises CFR from the capture rate. A blinking caret is a few kilobytes; a 1440p full frame would be ~15 MB. A single rect that covers the whole surface is a full refresh and can seed the compositor from scratch.

The encode loop skips ticks while the surface is unchanged, but never for longer than `ISOLA_STATIC_KEEPALIVE_MS` (default 2 s, `0` disables). Idle pages stay cheap without looking like a stall. On the MSE path a quiet change also gets a 40 ms follow-up frame so the fMP4 muxer can close the fragment — otherwise a single edit would sit unflushed until keepalive.

Only the active tab of an attached viewer is composited at the capture rate. An inactive tab — and every tab while no viewer is attached — runs at `ISOLA_BACKGROUND_FPS` (default `1`; `<= 0` keeps the capture rate). An offscreen window is composited whether or not anyone can see it, and with no GPU in the sandbox a background tab drawing a WebGL page at capture rate is real CPU competing with the tab that is actually on screen.

Loading a document also schedules post-load `capturePage()` readbacks, because offscreen paint misses GPU-layer hero and background images. Each readback is a full-surface one, so once a document has put a full frame on the wire only the late offset runs (`ISOLA_LOAD_CAPTURE_DELAYS_MS`, default `1200,5000`).

### Encode

ffmpeg reads the composited BGRA, converts, and emits H.264 **High** profile (`avc1.6400LL`; level follows capture size — 3.0 at 720p, up through 5.1 at 4K). High is ~15% more efficient than Constrained Baseline at the same bitrate, which is the quality lever on text-heavy pages.

Hardware encode is used when the container can see it (QSV / VAAPI on `/dev/dri`, NVENC when the NVIDIA encoder is present); otherwise libx264. Bitrate scales with pixel area (6 Mbps at 720p, 2 Mbps floor) so a large capture keeps bits-per-pixel instead of flattening under a fixed cap.

Fan-out never blocks on one slow socket. Each viewer has a bounded mailbox; a stalled watcher drops and waits for the next IDR instead of stalling the shared run. A newly attached subscription (or one that overflowed) discards delta frames until a keyframe — a late joiner must not decode against a reference it never received. libx264 on a live pipe cannot emit one IDR in place, so `request_keyframe` replaces the run.

Control and video share that mailbox: `stream_reset` cannot overtake the last frame of the old stream, and cannot trail the first frame of the new one.

## Two Sockets

`GET /ws/{sessionId}` upgrades to WebSocket. Direction is a query parameter, not a second port:

| Socket | Query | Carries |
| :--- | :--- | :--- |
| Video | `channel=isola` (default) | Binary media + JSON control (`stream_init`, metrics, pong). |
| Input | `channel=input` | JSON mouse, keys, clipboard, tabs. |

They are separate so a large video write cannot stall a click. Auth and the `host=1` flag are in [API → WebSocket](api.md#websocket).

### Relay lifecycle and limits

The Go relay is the only hop between a viewer and a container, and it owns the
teardown of both sockets. The rules below are enforced in
`backend/pkg/wsproxy/duplex.go`:

| Guard | Value | Why |
| :--- | :--- | :--- |
| Write deadline, both directions | 30 s per message | A viewer or container that stops reading must not pin a relay goroutine (and with it the other socket) forever. The streamer already applies 10 s per write. |
| Container read deadline | 90 s between messages | A container that stops producing frames is torn down rather than holding the viewer open. |
| Client read limit | 12 MiB per message | Input and control are small JSON; an unbounded client message is a DoS. |
| Container read limit | 32 MiB per message | The streamer sends one message per packet, so the largest legitimate message is a single keyframe (well under 1 MiB at 1080p). 32 MiB is an order of magnitude of headroom — a tighter limit would break video, a looser one would bound nothing. |
| Container control (TEXT) limit | 1 MiB per message | Control/status JSON does not scale with the picture, so it is bounded separately and tightly even though video cannot be. |
| Client read deadline | none | An idle viewer legitimately sends nothing for minutes. Liveness is the 30 s ping, whose control write has its own 10 s deadline. |

Closing is symmetric: whichever of the three goroutines exits first (viewer
gone, container gone, keepalive write failed) closes **both** sockets and the
`done` channel exactly once, so `wg.Wait` cannot return with a socket still
open and a dead viewer cannot leave a container subscription alive. Teardown is
safe mid-frame — closing a socket under an open writer makes the in-flight
flush return an error rather than panic.

### Analysis side channel (server → client)

The input socket also carries the observation stream, so a viewer renders
traffic without a second connection. Unknown `type` values must be ignored —
the set grows over time.

| `type` | Payload |
| :--- | :--- |
| `net_init` | `{ entries: [...] }` — replay of the capture window when a viewer attaches. |
| `net` | `{ e: {...} }` — one request snapshot; merge by `e.id` (the streamer publishes the same row several times as it progresses). |
| `headers` | `{ e: { id, url, s, tb, hd, pst, tls } }` — document response headers, the posture grade and the certificate summary. `tls.security_state` is CDP's verdict (`secure`, `neutral`, `insecure`, …), and `pst.tls_state`/`pst.note` report it with the grade it capped. Sent separately from `net` so subresources stay small. |
| `console` | `{ l, t, tb, ts }` — one console line. |
| `cookie` | `{ d, n, v, h, s, fp? }` — one parsed `Set-Cookie`; `fp` is the product-family label when the name matches a known fingerprint. |
| `tabs` | `{ tabs: [...] }` — the in-container tab strip. |

Analysis is descriptive: raw CDP, response bodies and `Set-Cookie` header maps
are never forwarded (the cookies tab owns cookie data).

```
ws://localhost:8080/ws/<SESSION_ID>?channel=isola&mode=webcodecs&session_token=<TOKEN>
ws://localhost:8080/ws/<SESSION_ID>?channel=input&host=1&session_token=<TOKEN>
```

`mode=webcodecs` is negotiated on the **handshake URL**, not in a first message. The bundled viewer sets it when `VideoDecoder` exists; otherwise the streamer serves fMP4 for MSE. Negotiating in-band raced slow links and poisoned the session.

## Binary Packets (`channel=isola`)

Every media frame is a binary WebSocket message: a 10-byte little-endian header, then the payload.

| Offset | Size | Field |
| :--- | :--- | :--- |
| 0 | 1 | Marker: `0x01` fMP4 segment, `0x02` Annex-B access unit, `0x03` Opus packet |
| 1 | 1 | Flags: bit 0 set = keyframe (video only) |
| 2–5 | 4 | Sequence number (uint32 LE) |
| 6–9 | 4 | Payload length (uint32 LE) |
| 10+ | *n* | Payload |

Set `binaryType = 'arraybuffer'` on the socket. Ignore unknown markers.

**WebCodecs (`0x02`).** Payload is one H.264 access unit in Annex-B (`avc1.6400LL` / High profile in current builds). SPS/PPS ride in-band before each IDR. Feed `EncodedVideoChunk` with `type: 'key'` when flag bit 0 is set. Draw onto a canvas.

`VideoDecoder.decode()` is fire-and-forget, so the queue is the presentation delay: after a burst or a throttled tab every access unit is still decoded in order and the viewer replays the past. The bundled viewer therefore bounds that queue (250 ms of stream time, `MAX_DECODE_BACKLOG_MS`): when it is exceeded it abandons the decoder queue, asks for a keyframe, and resumes on the next IDR — the current picture — refusing deltas in between. Its state is published at 1 Hz in the viewer's metrics (`decodeBacklogMs`, `decodeDroppedFrames`, `decodeCatchUps` in `IsolaStreamMetrics`).

Two lifecycle rules follow from WebCodecs itself. `VideoDecoder.reset()` leaves the decoder **unconfigured**, not "configured": a client that resets on `stream_reset` must `configure()` again or every later `decode()` throws. And `stream_reset` + `stream_init` with an unchanged codec string needs neither a new decoder nor a new canvas — the canvas is resized in place, so an ABR step costs no decoder allocation and does not blank the picture.

**MSE (`0x01`).** Payload is an fMP4 fragment. The first segment after `stream_init` is an init segment; later ones are media. Use this only when WebCodecs is unavailable.

**Audio (`0x03`).** Raw Opus packets (20 ms), forwarded only while the session has audio enabled (`POST /api/sessions/{id}/audio`). Decode with `AudioDecoder` into an `AudioContext`. Audio shares the video socket; it is not a third channel.

## JSON on the Video Socket

Text frames are JSON objects with a `type` field. Unknown types are ignored.

### `stream_init` (server → client)

First message after attach, and again after every encoder restart:

```json
{
  "type": "stream_init",
  "codec": "avc1.64001E",
  "width": 1280,
  "height": 720,
  "capture_width": 1280,
  "capture_height": 720,
  "encode_fps": 25,
  "bitrate_kbps": 6000,
  "base_bitrate": 6000,
  "mode": "webcodecs"
}
```

`width` / `height` are the encoded surface (ABR may scale them down). `capture_*` is the compositor size — map pointer events into that space, not the encoded size. `mode` is present only for Annex-B clients. `base_bitrate` is the session ceiling; ABR must not ask above it. `codec` is `avc1.6400` plus the H.264 level byte for the capture (for example `1E` at 720p, `28` at 1080p).

### `stream_reset` (server → client)

Sent immediately before a new `stream_init` when the encoder run is replaced (bitrate change, keyframe request, resize). Tear down `VideoDecoder` / `SourceBuffer` and wait for the following init.

A reset always has an init behind it. The matching local control surface is `POST /resize?width=&height=` on the streamer port (the backend calls it for `POST /api/sessions/{id}/resize`); like `/navigate` it refuses any method but `POST` with `405`, so a stray `GET` — a prefetch, a link preview, an `<img>` tag — cannot re-key every viewer's encoder.

### `stream_error` (server → client)

The last message on the socket when the streamer could not start an encoder for this viewer at all — ffmpeg missing from the image, a process or memory limit, a container out of slots:

```json
{ "type": "stream_error", "reason": "encoder_error", "retryable": true }
```

It is **not** a `stream_reset`: no `stream_init` follows, no video will arrive on this socket, and the streamer closes it immediately with application close code `4001`. The distinction is the whole point — a client that treats every reset as "wait for the next init" waits forever, and a bare TCP close is indistinguishable from a network drop.

What a client should do: stop decoding, surface an error, and reconnect the **socket** with backoff (a few attempts seconds apart, never a tight loop). `retryable: true` means the failure was the encoder, not the session, so a reconnect can succeed once transient pressure clears; if every attempt fails the session is unusable and needs replacing. Behind the relay the close code does not survive the hop — the relay copies messages, not close frames — so branch on `stream_error`, not on the close code.

### `metrics` (server → client)

A periodic snapshot of capture, encode, and renderer health. The bundled debug overlay renders it; custom clients can ignore it. Useful fields for a frozen picture: `changes_per_sec`, `damaged_pct`, `capture_empty_window`, `capture_fallback`, `renderer_stalled`, `cdp_probe_fail_streak`.

Three cumulative, per-viewer counters say what this socket could not use — each viewer is metered on its own, so a counter that rises points at that one viewer, never at the encoder:

| Field | Meaning | What to read into it |
| :--- | :--- | :--- |
| `drop_overflow` | Packets shed because this viewer's mailbox was full. | The socket is not draining at the encode rate: the viewer is too slow, or the path to it is. Everything until the next IDR is undecodable for it. |
| `drop_awaiting_key` | Packets shed because the viewer had no picture yet and they were deltas whose references it never received (the poster wait after joining, or after a resync). | Expected for the first GOP of any late join, and it stops on its own when the first IDR lands. A number that keeps climbing means no IDR is arriving — a different problem. |
| `dropped` | `drop_overflow + drop_awaiting_key`. | The compatible total, kept so a single-number reader keeps its meaning. |

A rising `dropped` with a healthy `encode_fps` means this viewer is falling behind — but check which half moved before blaming the network: `drop_awaiting_key` on a viewer that joined 100 ms ago is the poster, `drop_overflow` on a long-lived one is the socket. Older streamers omit all three, and clients must tolerate that. Fields may be added; ignore the ones you do not know.

### Packet sequence and loss

Each binary packet carries a per-run `seq` (`uint32` LE at header offset 2) that restarts at 0 with every encoder run. Over TCP a gap is therefore not network loss: it is the streamer shedding packets for this viewer, and everything until the next IDR is undecodable. The client counts `seq > last + 1` as missing, treats a backward/duplicate `seq` as a run change (baseline reset, nothing counted), and ignores all of it across `stream_init`/`stream_reset`. Progressive missing counts are not worth a restart — one or two dropped delta frames heal on the next P-frame — but a burst of 5 missing packets is: the viewer then sends `request_keyframe` through its normal keyframe-recovery budget (one request per burst), exactly as it does after a decode error.

### Client → server (video socket)

| Message | Effect |
| :--- | :--- |
| `{"type":"ping","ts":…}` | Echoed as `{"type":"pong","ts":…}` for RTT. `ts` is passed through verbatim. |
| `{"type":"set_bitrate","kbps":N}` | Move **this** viewer onto an encoder run at `N` kbps (clamped to `base_bitrate`). Other viewers keep the run they are on; the previous run stops when its last viewer leaves it. When two runs are already live the viewer may land on a different rung than it asked for — the most expensive live run at or below `N`, else the cheapest one — and learns the real rung from the `stream_init` and metrics that follow (`bitrate_kbps`, encoded `width`/`height`), never from its own request. |
| `{"type":"request_keyframe"}` | Replace the current run so a late joiner or a dropped decoder gets an IDR. Budgeted per connection: while the viewer still has no picture it may retry the restart three times, spaced by the hub-wide 5 s cooldown; once it has a picture it gets one forced restart per 30 s, four per connection. A refused request is ignored silently. Every viewer of that run is reset with it. |

The relay forwards `ping` and `request_keyframe` even on view-only sockets. Pointer and key events belong on `channel=input` with `host=1`. A view-only connection may forward one `request_keyframe` per 30 s — the relay drops the rest and logs them — because each request restarts the shared encoder for the session owner too. `ping` is not limited.

## Adaptive Quality

The viewer measures RTT (`ping`/`pong`), inbound bytes, and decode health, then sends `set_bitrate`. The streamer does not pick the rung itself — it starts a run at the requested kbps (and may reduce encode scale with the bitrate), subject to the two-run cap above. A viewer that lands on another rung is not left guessing: the `stream_reset` + `stream_init` pair and every metrics tick carry the run it actually joined, so the controller's `targetKbps` is the real encoder target rather than the request. A viewer alone in its session always gets exactly the rung it asked for, because the run it steps off is its own and is stopped as part of the step. Capture coordinates stay at `capture_width` / `capture_height` even when the encoded surface is smaller, so clicks do not drift when ABR steps down. Resolution-aware defaults apply when the session is created without an override. See [Configuration](configuration.md) for `session.resolution` and bitrate-related env.

## Input That Matches the Picture

Offscreen Chromium has no OS cursor and no X hit-testing. Isola drives the page the same way a debugger would: CDP `Input.*` on the target that owns the pixels.

- **Cross-origin iframes** — A click on the page target stops at the parent `<iframe>`. Paint still composites the child (embedded players, ads); hit-testing does not. When the pointer is over an OOPIF, Isola translates into that frame's viewport and dispatches on the iframe CDP target. Same-origin frames are walked in-page; the walk stops at the first cross-origin frame `/json/list` exposes as its own target.
- **Tabs** — The Electron runner owns the tab strip (`tab_new`, `tab_switch`, `tab_close`, `tab_back`) on `/run/isola/ctrl.sock`. The streamer relays those commands and points CDP at the active page. Capture attaches to every page target, so each network/console entry carries the id of the tab it came from; a target is matched to a tab by URL (exact, then prefix, with the title breaking ties), and an unmatched target falls back to the active tab. Traffic captured before the runner has reported its first strip — the launch-page load — is folded into whichever tab is active once it does, and re-published so attached viewers correct it in place. Console lines carry a streamer-assigned id, so a re-published line is folded into the existing row rather than appended a second time.
- **Cursor** — The viewer synthesizes arrow / I-beam / hand from the page's computed style under the pointer, so the picture is not a naked bitmap.
- **Clipboard** — Copy/cut inside the sandbox is read back over CDP and pushed to the host; paste goes the other way.

## Staying a Live Picture

A remote browser fails in ways a webcam does not: the renderer wedges, paint returns empty buffers, a heavy page stops mutating while the encoder is still healthy.

Isola treats those as first-class:

- **Empty captures** — a rolling window. After paint recovers, the window is pruned even if no new empties arrive, so the viewer badge does not stick.
- **Blank commits** — a navigation paints a blank document before the new page has anything on it. A blank takes several forms: an empty image, a whole surface of one colour, or — the one that used to get through — a blank *patch* of the viewport blitted onto a surface still holding the previous page, which is a half-white frame no browser ever presents. All three are withheld for up to `BLANK_HOLD_MS` after a navigation so the viewer keeps the last real picture; the hold is bounded, so a page that really is one flat colour still appears.
- **Renderer stall** — detected both from CDP probe timeouts (early) and from the runner's capture verdict (authoritative). The runner can replace the renderer and reload; the UI can show *why* the picture froze instead of only a low fps number.
- **`metrics`** — those signals are exposed so a custom client can do the same.

If frames keep flowing but the page never changes, that is the page, not the transport.

## Frame Rate

The session has two rates, and both matter:

- **capture rate** — `ISOLA_FPS` (default 25). The streamer encodes at this rate and reports it as `capture_fps` in `metrics`.
- **compositor rate** — what the runner's offscreen tab actually renders at. It is the capture rate *only for the active tab of an attached viewer*; every other tab, and every tab with no viewer attached, renders at `BACKGROUND_FPS` (1). Composing a page nobody watches at 25 fps would burn CPU for frames that are never encoded.

The runner learns about viewers from the streamer's `{"cmd":"capture","on":true|false}` on `/run/isola/ctrl.sock`. Two rules keep the two rates consistent:

- A `capture` command whose value equals the runner's own state is a **re-assertion, not a no-op** — the streamer re-sends the current state whenever its ctrl socket reconnects, so the command must re-apply the rate. Ignoring it (the pre-1.5.3 behaviour) could leave a session at 1-2 fps with `activity_mode: active` in every metric while scrolling and typing looked frozen.
- While a viewer is attached, the runner re-applies the active tab's rate every 5 s, so a rate lost through any path that is not a capture transition (a late resize, a re-attached tab) repairs itself.

Measure the *compositor* rate when diagnosing a frozen viewer, not the frame counter: `requestAnimationFrame` over one second inside the page. With a viewer attached it should match `capture_fps`; with none attached, 1-2 fps is correct behaviour, not a bug.

The regression is covered end to end by [`browsers/isola-streamer/fps_e2e_test.go`](../browsers/isola-streamer/fps_e2e_test.go), which boots the sandbox image against a real site (`https://github.com/` by default), attaches a real viewer and asserts the rate on attach, reconnect, capture re-send, live resize, and detach:

```bash
ISOLA_FPS_E2E=1 go test -run TestViewerFrameRateOnRealSite -v ./browsers/isola-streamer
```

## Building a Custom Viewer

1. `POST /api/sessions`, keep `id` and `session_token`.
2. Open `channel=isola` with `mode=webcodecs` if `VideoDecoder` exists.
3. Wait for `stream_init`, configure the decoder from `codec` / `width` / `height`.
4. Decode `0x02` (or `0x01`) packets; map clicks using `capture_width` / `capture_height`.
5. Open `channel=input&host=1` for JSON input. Do not send input on the video socket — the streamer will accept some types there as a fallback, but they head-of-line behind video.
6. On `stream_reset`, dispose the decoder and wait for the next `stream_init`. If `stream_error` arrives instead, no init will follow and the socket is closing — surface it and reconnect with backoff (the section above is the contract).
7. If the decoder errors, send `request_keyframe` rather than reconnecting the whole session.
8. Closing the socket ends the viewer: the streamer releases the subscription whether the close handshake completed or not, and stops the encoder run once its last viewer is gone. Abandoning a socket without closing it costs a dead subscription until the server's 10 s write deadline fires, so do close it.

The origin check on the upgrade requires a browser `Origin` that matches the request host (localhost origins only when the request itself is local). A non-browser client that strips `Origin` is refused — see [Security](security.md).

## Related Code

| Piece | Where |
| :--- | :--- |
| Packet packing | [`browsers/isola-streamer/protocol.go`](../browsers/isola-streamer/protocol.go) |
| Encoder fan-out | [`browsers/isola-streamer/stream_hub.go`](../browsers/isola-streamer/stream_hub.go) |
| H.264 / HW probe | [`browsers/isola-streamer/encoder.go`](../browsers/isola-streamer/encoder.go) |
| Viewer session loop | [`browsers/isola-streamer/ws.go`](../browsers/isola-streamer/ws.go) |
| OOPIF pointer routing | [`browsers/isola-streamer/iframe_input.go`](../browsers/isola-streamer/iframe_input.go) |
| Capture UDS (`ISOL`) | [`browsers/electron-runner/src/isol_protocol.js`](../browsers/electron-runner/src/isol_protocol.js) |
| Browser client | [`frontend/src/hooks/useIsolaStream.ts`](../frontend/src/hooks/useIsolaStream.ts) |
| Relay | [`backend/pkg/wsproxy/`](../backend/pkg/wsproxy/) |
