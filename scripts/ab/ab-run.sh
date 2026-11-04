#!/bin/bash
# One A/B arm: boot a streamer binary, drive it, score what the viewer got.
#
#   ./ab-run.sh <arm-name> <streamer-binary> <port> <w> <h> <kbps> <frames> \
#               [attackers] [attackMs]
#
# Env: AB_OUT (default /tmp/isola-ab/out) holds per-arm artifacts, SCROLL=1
# switches to the scrolling corpus (not frame-scorable — see driver.mjs).
set -u
ARM=$1; BIN=$2; PORT=$3; W=$4; H=$5; KBPS=$6; FRAMES=$7; ATK=${8:-0}; ATKMS=${9:-0}
HERE=$(cd "$(dirname "$0")" && pwd)
DIR=${AB_OUT:-/tmp/isola-ab/out}/$ARM
rm -rf "$DIR"; mkdir -p "$DIR"
SOCK=$DIR/frame.sock

PORT=$PORT ISOLA_FRAME_SOCK=$SOCK ISOLA_WIDTH=$W ISOLA_HEIGHT=$H ISOLA_FPS=25 \
  ISOLA_BITRATE_KBPS=$KBPS ISOLA_STATIC_KEEPALIVE_MS=2000 \
  /usr/bin/time -l -o "$DIR/time.txt" "$BIN" >"$DIR/streamer.log" 2>&1 &
TIME_PID=$!

for _ in $(seq 1 100); do
  [ -S "$SOCK" ] && grep -q "listening on" "$DIR/streamer.log" 2>/dev/null && break
  sleep 0.1
done
STREAMER_PID=$(pgrep -P "$TIME_PID" | head -1)
if [ -z "${STREAMER_PID:-}" ]; then echo "{\"arm\":\"$ARM\",\"error\":\"streamer did not start\"}"; exit 1; fi

node "$HERE/driver.mjs" --sock "$SOCK" --port "$PORT" --w "$W" --h "$H" \
  --frames "$FRAMES" --fps 25 --out "$DIR/out.h264" --refs "$DIR/refs" \
  --attackers "$ATK" --attack-ms "$ATKMS" --scroll "${SCROLL:-0}" \
  >"$DIR/metrics.json" 2>"$DIR/driver.err"

sleep 0.5
kill -TERM "$STREAMER_PID" 2>/dev/null
wait "$TIME_PID" 2>/dev/null

if [ -f "$DIR/refs/ref_0001.ppm" ]; then
  REF_IN=(-framerate 25 -i "$DIR/refs/ref_%04d.ppm")
else
  REF_IN=(-loop 1 -framerate 25 -i "$DIR/refs/ref_0000.ppm")
fi

score() {
  local filt=$1 out=$2 tag=$3
  ffmpeg -hide_banner -loglevel info -f h264 -i "$DIR/out.h264" "${REF_IN[@]}" \
    -lavfi "[0:v]format=yuv420p,setsar=1[a];[1:v]format=yuv420p,setsar=1[b];[a][b]${filt}=shortest=1" \
    -f null - 2>&1 | grep -i "$tag" | tail -1 >"$out"
}
score ssim "$DIR/ssim.txt" "SSIM"
score psnr "$DIR/psnr.txt" "PSNR"

SSIM=$(grep -o "All:[0-9.]*" "$DIR/ssim.txt" | head -1 | cut -d: -f2)
PSNR=$(grep -o "average:[0-9.a-z]*" "$DIR/psnr.txt" | head -1 | cut -d: -f2)
read -r REAL_S USER_S SYS_S < <(awk '/ real /{print $1, $3, $5}' "$DIR/time.txt" | tail -1)
FFMPEG_STARTS=$(grep -c "encoder: ffmpeg started" "$DIR/streamer.log")
FORCED=$(grep -c "forced keyframe" "$DIR/streamer.log")
REFUSED=$(grep -c "keyframe request refused\|keyframe request ignored" "$DIR/streamer.log")

python3 - "$ARM" "$DIR/metrics.json" "$SSIM" "$PSNR" "$USER_S" "$SYS_S" "$REAL_S" \
  "$FFMPEG_STARTS" "$FORCED" "$REFUSED" <<'EOF'
import json, sys
arm, mpath, ssim, psnr, user, syscpu, real, starts, forced, refused = sys.argv[1:11]
m = json.load(open(mpath))
cpu = float(user or 0) + float(syscpu or 0)
print(json.dumps({
  "arm": arm,
  "codec": m.get("codec"),
  "frames_fed": m.get("frames"),
  "packets": m.get("packets"),
  "idr_packets": m.get("idr"),
  "stream_bytes": m.get("bytes"),
  "first_packet_ms": m.get("firstPacketMs"),
  "ssim": float(ssim) if ssim else None,
  "psnr_db": None if not psnr or psnr == "inf" else float(psnr),
  "wall_s": float(real or 0),
  "cpu_s": round(cpu, 2),
  "stream_s": round(m.get("feedMs", 0) / 1000, 2),
  "cpu_per_stream_s": round(cpu / (m.get("feedMs", 1) / 1000), 2),
  "encoder_restarts": max(0, int(starts) - 1),
  "forced_keyframes": int(forced),
  "keyframe_refusals": int(refused),
}))
EOF
