# Encoder runtime control

Today the streamer answers two different requests the same way: it kills ffmpeg
and starts a new one.

- a viewer asks for a keyframe (`{"type": "request_keyframe"}`) →
  `streamHub.requestKeyframe` spawns a replacement run;
- a viewer steps the ABR ladder (`{"type": "set_bitrate"}`) →
  `hub.attach(..., "bitrate_change")` spawns a run for the new rung.

Both are written that way because libx264 on a live pipe is assumed to accept
neither an IDR nor a new bitrate. This page is the spike that tested that
assumption: what the two CLI levers can and cannot do, what in-process control
would buy, and what the restart actually costs today.

**Status:** research only. No production code changed; `stream_hub.go` and
`ws.go` are untouched. The measurements come from
[`browsers/isola-streamer/encoder_restart_bench_test.go`](../browsers/isola-streamer/encoder_restart_bench_test.go)
and throwaway prototypes that live in `/tmp` (not committed). Reproduction
commands are at the end.

## What a restart costs a viewer

`spawnLocked` returns as soon as `ffmpeg` has been forked — before its first
IDR exists. Every viewer of the run is then moved to the new one with a
`stream_reset` control message plus a fresh fMP4 init segment (`ftyp`/`moov`),
and `streamSub.awaitingKey` drops fragments until the replacement writes its
first IDR. So one viewer's request costs every viewer of that run: a new
decoder configuration, and a video gap equal to *spawn → first IDR fragment*.

That gap is what the bench measures, not the `keyframeRestartGrace = 4 s`
tolerance (which only exists so a client's recovery timer cannot kill an
encoder that has not flushed yet).

## Measured: the restart stall

`ISOLA_ENCODER_BENCH=1 go test -run TestEncoderRestart -v .`, 720p, 25 fps,
6000 kbps, production argv from `startEncoder`.

| Environment | Samples | min | median | max |
| :--- | ---: | ---: | ---: | ---: |
| Debian bookworm container, ffmpeg 5.1.9 / libavcodec 59.37.100, `--cpus 3` | 16 | 62.1 ms | **68.2 ms** | 163.5 ms |
| macOS dev host, ffmpeg 9.0.2 / libavcodec 63.1.102, no cgroup quota | 12 | 46.5 ms | **49.7 ms** | 90.6 ms |

The `max` is always sample 0 — the first `ffmpeg` exec after a cold page cache.
Steady-state samples are tight: 62–70 ms in the container, 47–55 ms on the host.

Splitting the stall (`TestEncoderRestartBreakdown`, raw Annex-B so a NAL type 5
is unambiguous):

| Component | Debian 5.1.9 | macOS 9.0.2 |
| :--- | ---: | ---: |
| `fork` + exec, `startEncoder` returns | 0.22–0.33 ms | 1.5–2.1 ms |
| process + CLI + filtergraph + x264 init + first IDR encode (spawn → first stdout byte) | 54.8 / 57.4 / 59.7 ms | 41.5 / 45.2 / 60.3 ms |
| first byte → IDR visible on the pipe | 11–28 µs | 19–67 µs |
| first IDR visible as an fMP4 fragment (adds one frame + muxing) | 62–70 ms | 47–55 ms |
| encoder teardown (`kill` + wait) | 2.4–3.3 ms | 1.6–5.3 ms |
| first IDR fragment size | 60,508 B | 59,512 B |

Two things follow. Process startup, not encoding, is the stall: once ffmpeg
writes anything at all, the first picture is already an IDR. And the fMP4
fragment a viewer can actually use lands one frame later than the IDR itself,
because `-frag_duration 20 ms` closes on the next fed frame.

## Option matrix

| Option | Change bitrate live? | Force an IDR live? | New dependency | Measured on 5.1.9 | Measured on 9.0.2 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| `zmq` / `azmq` filter | **no** | **no** | libzmq (Debian build already has it) | filters present; dispatch measured through `sendcmd` (same `avfilter_graph_send_command`) → `Function not implemented` | filter not compiled into the dev build; dispatch measured through `sendcmd` |
| `sendcmd` filter | **no** | **no** | none (built in) | `Command reply ... ret:Function not implemented` | same |
| `-force_key_frames` | n/a | only at init, fixed times / pre-known frame numbers | none | IDR exactly on the injected expression; `f=<file>` rejected | same |
| in-process libavcodec / libx264 (cgo) | **yes**, 3.8–5.8× byte-rate change measured | **yes**, 3.0–4.3 ms at 720p | cgo, `libavcodec-dev` + `libx264-dev`, `CGO_ENABLED=1` | 3.5–4.3 ms forced IDR, 4.4–5.8× byte rate (libavcodec 59.37.100) | 3.0–3.8 ms forced IDR, 3.8–4.1× (libavcodec 63.1.102) |
| keep restarting (today) | new process per rung | new process per request | none | 68.2 ms median gap | 49.7 ms median gap |

## 1. Runtime bitrate over ffmpeg's command channel: impossible

The only in-band command channel ffmpeg has is the filtergraph: the `sendcmd`
filter (a command file) and `zmq`/`azmq` (a ZeroMQ socket). Both end in
`avfilter_graph_send_command`, whose target is a **filter instance** — not the
encoder. Debian's ffmpeg 5.1.9 build does ship `zmq` and `azmq` (the dev
machine's homebrew build does not), so the transport exists in the image; the
target does not. A ZeroMQ client connected to the filter bound in the container
but never received a command reply within 2 s, so the dispatch semantics below
were measured through `sendcmd`, which calls the same
`avfilter_graph_send_command` with the same target lookup — and returns
`ret:Function not implemented` for an encoder target on **both** 5.1.9 and
9.0.2.

Evidence, production argv shape (`-f rawvideo -pix_fmt bgra -s 1280x720 -r 25 -i
pipe:0` → libx264 → H.264), command issued at stream time 1.0 s targeting
`libx264 b 1500000`:

| Window (0.5 s) | 0.25 s | 0.75 s | 1.25 s | 1.75 s | 2.25 s | … | total (6 s) |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| control, no `sendcmd` | 1,163,264 B/s | 729,088 | 794,624 | 729,088 | 851,968 | … | 4,890,425 B |
| `sendcmd` → `libx264 b 1500000` | 1,163,264 B/s | 729,088 | 794,624 | 729,088 | 851,968 | … | 4,890,425 B |

Byte-for-byte identical, including every 0.5 s window: the command was parsed
and dispatched, and did nothing. `-loglevel debug` names the reason:

```
[Parsed_sendcmd_0] Processing command #0 target:libx264 command:b arg:1500000
[Parsed_sendcmd_0] Command reply for command #0: ret:Function not implemented
```

`ENOSYS` is `avfilter_graph_send_command` failing to find a filter called
`libx264`. The same file pointed at a real filter works, which rules out a
broken harness — target `scale@top w 960` replies `ret:Success` and the encoder
then fails with `Input picture width (1280) is greater than stride (960)`,
because the graph had already resized the frame mid-stream.

The AVOption layer agrees: querying the `libx264` encoder context with
`av_opt_find2` shows none of the rate-control options is a runtime option.

| Option | flags | `AV_OPT_FLAG_RUNTIME_PARAM` |
| :--- | :--- | :--- |
| `b` | `0x00000019` | no |
| `maxrate` | `0x00000019` | no |
| `bufsize` | `0x00000019` | no |
| `g` | `0x00000011` | no |
| `preset`, `x264-params` | `0x00000011` | no |

So even a custom ffmpeg that let a command address the encoder's AVClass would
have nothing to call: libx264's rate control is fixed at `x264_encoder_open`,
and the CLI never calls `x264_encoder_reconfig`. (`ffmpeg`'s *library* wrapper
does call it — see §3 — but only when `avctx->bit_rate` changes, which no CLI
input can cause.)

## 2. Runtime IDR: impossible

`-force_key_frames` is an output option evaluated by the CLI encode loop, and it
only accepts fixed timestamps or an expression over frame variables. It works —
an `expr:eq(n,75)` run emitted an IDR at 3.009 s instead of the 2 s GOP
boundary, and the following keyframe moved to 5.007 s — but it cannot be
reprogrammed while the process runs:

```
$ ffmpeg ... -force_key_frames f=/tmp/keytimes.txt -f h264 pipe:1
[vost#0:0/libx264] Invalid keyframe time: f=/tmp/enc-spike/keytimes.txt
Error processing forced keyframes
```

There is no file, fifo, signal or stdin form: rawvideo is the only thing ffmpeg
reads, and the command channel (`sendcmd` / `zmq`) reaches filters only. Two
further properties of the production argv close the remaining loopholes:
`-sc_threshold 0` with `scenecut=0` disables scene-cut IDRs entirely (a
deliberate bitrate choice), and `-intra-refresh` would make every frame a
recovery point rather than give an IDR on request.

## 3. In-process libavcodec: both levers work

Prototype (`/tmp`, cgo, no production code): 90 frames of incompressible
content at 6 Mbps through `avcodec_send_frame` / `avcodec_receive_packet`, an
IDR forced at frame 20, `bit_rate` changed at frame 45. It was built against
both libraries — libavcodec 63.1.102 + x264 r3222 on the dev host, and
libavcodec 59.37.100 + x264 0.164.3095 inside the Debian container — because
the image's ffmpeg is the older one.

720p:

| Library | `avcodec_open2` | first IDR packet | forced-IDR frame (encode time) |
| :--- | ---: | ---: | ---: |
| libavcodec 59.37.100, libavcodec wrapper | 1.10 ms | 17.3 ms | 4.25 ms |
| libavcodec 59.37.100, `x264_encoder_encode` direct | 0.70 ms | 11.9 ms | 3.54 ms |
| libavcodec 63.1.102, libavcodec wrapper | 0.58–0.78 ms | 6.7 ms | 3.8 ms |
| libavcodec 63.1.102, `x264_encoder_encode` direct | 0.41–0.57 ms | 8.6 ms | 3.0 ms |

(360p: 1.8–2.8 ms forced IDR / 6.8–9.4 ms first packet. 1080p: 6.5–7.3 ms forced
IDR / 14.5–20.2 ms first packet — the first packet is slower than the forced one
because it includes the encoder's own initialisation and VBV priming.)

The forced IDR is real, not a guess: the packet flags show keyframes at frames
0, 20 (forced mid-GOP) and 70 (the 50-frame GOP boundary) on both libraries, and
`x264_encoder_open` takes 0.41–0.70 ms — two orders of magnitude less than the
57 ms ffmpeg CLI startup the restart pays.

Runtime bitrate works too. At 720p, 6000 → 1500 kbit/s at frame 45:

| Path | bytes/s before the change | bytes/s after (settled) | ratio |
| :--- | ---: | ---: | ---: |
| libavcodec 59, `avctx->bit_rate` / `rc_max_rate` | 651,950 (26,078 B/frame) | 111,775 (4,471 B/frame) | 5.83× |
| libavcodec 59, `x264_encoder_reconfig` | 639,225 (25,569 B/frame) | 146,500 (5,860 B/frame) | 4.36× |
| libavcodec 63, `avctx->bit_rate` / `rc_max_rate` | 583,525 (23,341 B/frame) | 141,275 (5,651 B/frame) | 4.13× |
| libavcodec 63, `x264_encoder_reconfig` | 600,850 (24,034 B/frame) | 159,025 (6,361 B/frame) | 3.78× |

"After (settled)" is the mean over frames 76–88, i.e. after the 1 s VBV has
drained towards the new target; the first two seconds after the change average
2.0–2.9 Mbit/s. `x264_encoder_reconfig` returned 0 in every case. The
`avctx->bit_rate` path is not a prototype trick: `libavcodec/libx264.c` compares
`x4->params.rc.i_bitrate` with `ctx->bit_rate / 1000` inside `X264_frame` and
calls `x264_encoder_reconfig`, and does the same for `rc_buffer_size` /
`rc_max_rate`; that code has been there since ffmpeg 2.x, which is why it
behaves identically on 59 and 63.

**The exact API calls for this path:**

```c
// setup, once per run
codec = avcodec_find_encoder_by_name("libx264");     // AV_CODEC_ID_H264
enc   = avcodec_alloc_context3(codec);               // width/height/pix_fmt/time_base/gop_size/bit_rate
av_opt_set(enc->priv_data, "preset", "superfast", 0); // + "tune"/"profile"/"x264-params" as today
avcodec_open2(enc, codec, NULL);

// per frame
avcodec_send_frame(enc, frame);
avcodec_receive_packet(enc, pkt);                     // pkt->flags & AV_PKT_FLAG_KEY

// keyframe request, next frame
frame->pict_type = AV_PICTURE_TYPE_I;
frame->flags    |= AV_FRAME_FLAG_KEY;

// ABR step, immediate
enc->bit_rate     = kbps * 1000;
enc->rc_max_rate  = kbps * 1000;
enc->rc_buffer_size = kbps * 1000;
```

What it does **not** remove: an IDR still has to be encoded (3.0–3.8 ms at
720p on incompressible content) and still has to reach a frame boundary — at
25 fps the next fed frame is up to 40 ms away unless the pump injects an extra
frame for it, which the stream pump already knows how to do for keepalives.
Best case therefore ~4 ms, worst case one frame interval — against 68 ms plus a
viewer decoder reset today. The larger win is semantic: a keyframe request or an
ABR step would no longer reset every other viewer of the run, and would not need
`keyframeRestartGrace` / `lateJoinRestartCooldown` to damp a restart storm. Note
also that only the first ABR down-step (`100% → 70%`, scale 1.0) is
bitrate-only; the deeper rungs change `scale`, which is an argv/geometry change
today and would remain one.

## 4. Keep restarting: what it costs, and the honest alternative

The measured cost is 68 ms of video and a decoder reset per event, on a path
that is already rate-limited (4 s grace, 5 s cooldown). In-process control would
replace that with ~4 ms and no reset — but it costs:

- **cgo.** `CGO_ENABLED=1` for the streamer. Today `browsers/browser-node-electron/Dockerfile`
  builds it as `CGO_ENABLED=0` on `golang:1.26.10-alpine`; with cgo the builder
  must match the runtime's glibc, i.e. `golang:1.26-bookworm`, plus
  `libavcodec-dev`/`libx264-dev`/`pkg-config` in the build stage (discarded by
  multi-stage, so the shipped image does not grow from them).
- **Dynamic linkage.** The binary stops being static: it needs
  `libavcodec.so.59`, `libavutil.so.59`, `libx264.so.164` at runtime. Those are
  already in the image — `ffmpeg` is installed from apt and drags in
  `libavcodec.so.59.37.100` (12.7 MB) and `libx264.so.164` (1.4 MB) — so the
  image delta is ~0, but the build is no longer architecture-agnostic:
  `publish-browser-image.yml` builds `linux/amd64,linux/arm64` through buildx,
  and cgo means compiling under emulation (or a cross toolchain) for the
  non-native leg.
- **A muxer.** `startEncoder` currently delegates fMP4 fragmenting to ffmpeg
  (`-movflags frag_keyframe+empty_moov+default_base_moof -frag_duration 20000`)
  and pixel-format conversion to libavfilter. In-process means either binding
  `libavformat`/`libswscale` too (more cgo surface) or writing the fragmenter in
  Go — the streamer already parses fMP4 (`fmp4_reader.go`), it does not write it.
- **Isolation.** ffmpeg is a subprocess today; a codec crash kills the encoder
  and the hub respawns it. In-process, the same fault kills the streamer and the
  session.
- **Licensing.** libx264 is GPLv2-or-later. The image already ships GPL ffmpeg,
  but linking it into the Go binary changes the licence status of the binary the
  project distributes (currently MIT alongside an external GPL tool).

## Recommendation

Keep the restart for now, and treat in-process encoding as a separate,
larger project rather than a follow-up to this spike. Both CLI levers are
provably unreachable — a command addressed to the encoder returns `ENOSYS`
(`Function not implemented`) on both ffmpeg 5.1.9 and 9.0.2, no rate-control
AVOption is marked runtime-settable, and `-force_key_frames` cannot be
reprogrammed after start — so the choice is 68 ms of gap per restart (measured,
already rate-limited by the 4 s grace and 5 s cooldown) versus cgo, a builder
base change, dynamic linkage, a new fragmenter, lost crash isolation and a
licence change. If the reset-every-viewer behaviour becomes a product problem —
it is the part a restart cannot fix — the path is in-process libavcodec with
`avcodec_send_frame`/`avcodec_receive_packet`, `frame->pict_type =
AV_PICTURE_TYPE_I` plus `AV_FRAME_FLAG_KEY` for the IDR and `enc->bit_rate` /
`rc_max_rate` / `rc_buffer_size` for the ABR step (or `x264_encoder_reconfig`
directly), which this spike measured at 3.0–4.3 ms per forced IDR and a 3.8–5.8×
byte-rate change at 720p on the image's own libavcodec 59.

## Reproduction

```bash
cd browsers/isola-streamer

# restart stall (fMP4, what a viewer waits for) and the spawn→byte→IDR split
ISOLA_ENCODER_BENCH=1 ISOLA_ENCODER_BENCH_RUNS=16 \
  go test -run TestEncoderRestart -v -count=1 .

# the same binary inside the production image's Debian ffmpeg 5.1
# (on colima only $HOME is mounted, so keep the binary under your home dir)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o /tmp/bench.test .
docker run --rm --cpus 3 -v /tmp:/t debian:bookworm-slim sh -c \
  'apt-get update -qq && apt-get install -y -qq ffmpeg >/dev/null && \
   ISOLA_ENCODER_BENCH=1 ISOLA_ENCODER_BENCH_RUNS=16 /t/bench.test -test.run TestEncoderRestart -test.v'
```

The option-1/2 prototypes (`/tmp/enc-spike/ff.py`, `exp1_sendcmd.py`,
`exp2_idr.py`) drive the production argv shape over rawvideo stdin and print
bytes/s windows plus IDR timestamps; the option-3 prototype
(`/tmp/enc-spike/inproc/main.go`, `W=1280 H=720 go run .`) prints
`avcodec_open2`, forced-IDR and `x264_encoder_reconfig` timings and per-frame
packet sizes. Against the image's libavcodec:

```bash
docker run --rm -v /tmp/enc-spike:/t debian:bookworm-slim sh -c \
  'apt-get update -qq && apt-get install -y -qq golang-go libavcodec-dev \
   libavutil-dev libx264-dev pkg-config gcc >/dev/null && \
   cd /t/inproc && CGO_ENABLED=1 W=1280 H=720 go run .'
```

Bookworm's `golang-go` is 1.19, so the copy that ran there also rewrites the
`range int` loops (`for i := range n` → `for i := 0; i < n; i++`); everything
else is identical.

Both prototype trees are throwaway and deliberately not committed.
