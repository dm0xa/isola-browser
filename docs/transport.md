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
- **One encoder per quality, not per viewer** — A share-link watcher attaches to the run that already exists.
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

Inside the container the capture never leaves the box: Electron writes damage rectangles on `/run/isola/frame.sock` (`ISOL` magic). The streamer composites them and feeds ffmpeg. That socket is not reachable from the host or the viewer. Streamer `:5900` is published to loopback only; the relay is the only client.

### Capture (`ISOL`)

The producer does not ship a full BGRA surface every tick. Each frame is a 32-byte header (`ISOL` magic, surface size and stride, dirty-rect count, and a `ts_ns` timestamp) plus the damaged rectangles and their pixels. `ts_ns` is wall-clock UTC nanoseconds (`CLOCK_REALTIME`), so a consumer can subtract its own clock to get a buffer delay; the field is carried for that reason and is not yet read by the streamer. A blinking caret is a few kilobytes; a 1440p full frame would be ~15 MB. A single rect that covers the whole surface is a full refresh and can seed the compositor from scratch.

The encode loop skips ticks while the surface is unchanged, but never for longer than `ISOLA_STATIC_KEEPALIVE_MS` (default 2 s, `0` disables). Idle pages stay cheap without looking like a stall. On the MSE path a quiet change also gets a 40 ms follow-up frame so the fMP4 muxer can close the fragment — otherwise a single edit would sit unflushed until keepalive.

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

### `metrics` (server → client)

A periodic snapshot of capture, encode, and renderer health. The bundled debug overlay renders it; custom clients can ignore it. Useful fields for a frozen picture: `changes_per_sec`, `damaged_pct`, `capture_empty_window`, `capture_fallback`, `renderer_stalled`, `cdp_probe_fail_streak`.

### Client → server (video socket)

| Message | Effect |
| :--- | :--- |
| `{"type":"ping","ts":…}` | Echoed as `{"type":"pong","ts":…}` for RTT. `ts` is passed through verbatim. |
| `{"type":"set_bitrate","kbps":N}` | Move this viewer onto an encoder run at `N` kbps (clamped to `base_bitrate`). Other viewers of the old run are reset with it. |
| `{"type":"request_keyframe"}` | Replace the current run so a late joiner or a dropped decoder gets an IDR. |

The relay forwards `ping` and `request_keyframe` even on view-only sockets. Pointer and key events belong on `channel=input` with `host=1`.

## Adaptive Quality

The viewer measures RTT (`ping`/`pong`), inbound bytes, and decode health, then sends `set_bitrate`. The streamer does not pick the rung itself — it starts a run at the requested kbps (and may reduce encode scale with the bitrate). Capture coordinates stay at `capture_width` / `capture_height` even when the encoded surface is smaller, so clicks do not drift when ABR steps down. Resolution-aware defaults apply when the session is created without an override. See [Configuration](configuration.md) for `session.resolution` and bitrate-related env.

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
- **Renderer stall** — detected both from CDP probe timeouts (early) and from the runner's capture verdict (authoritative). The runner can replace the renderer and reload; the UI can show *why* the picture froze instead of only a low fps number.
- **`metrics`** — those signals are exposed so a custom client can do the same.

If frames keep flowing but the page never changes, that is the page, not the transport.

## Building a Custom Viewer

1. `POST /api/sessions`, keep `id` and `session_token`.
2. Open `channel=isola` with `mode=webcodecs` if `VideoDecoder` exists.
3. Wait for `stream_init`, configure the decoder from `codec` / `width` / `height`.
4. Decode `0x02` (or `0x01`) packets; map clicks using `capture_width` / `capture_height`.
5. Open `channel=input&host=1` for JSON input. Do not send input on the video socket — the streamer will accept some types there as a fallback, but they head-of-line behind video.
6. On `stream_reset`, dispose the decoder and wait for the next `stream_init`.
7. If the decoder errors, send `request_keyframe` rather than reconnecting the whole session.

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
