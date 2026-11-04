#!/bin/bash
# One multi-viewer arm: boot a streamer binary, attach N viewers on N ABR rungs,
# feed the corpus, and report CPU plus where each viewer actually landed.
#
#   ./multi-rung.sh <arm> <streamer-binary> <port> <w> <h> <base-kbps> <frames> <rungs>
#
# <rungs> is a comma-separated list of kbps targets, one per viewer; the first
# value is also the bitrate the session boots with (so the base is the ceiling
# of the ladder). Example: ./multi-rung.sh cap2 /tmp/rs-new 17020 1280 720 4000 200 4000,2800,1800
#
# Env: AB_OUT (default /tmp/isola-ab/out) holds per-arm artifacts.
set -u
ARM=$1; BIN=$2; PORT=$3; W=$4; H=$5; BASE=$6; FRAMES=$7; RUNGS=${8:-}
HERE=$(cd "$(dirname "$0")" && pwd)
DIR=${AB_OUT:-/tmp/isola-ab/out}/$ARM
rm -rf "$DIR"; mkdir -p "$DIR"
SOCK=$DIR/frame.sock

PORT=$PORT ISOLA_FRAME_SOCK=$SOCK ISOLA_WIDTH=$W ISOLA_HEIGHT=$H ISOLA_FPS=25 \
  ISOLA_BITRATE_KBPS=$BASE ISOLA_STATIC_KEEPALIVE_MS=2000 \
  /usr/bin/time -l -o "$DIR/time.txt" "$BIN" >"$DIR/streamer.log" 2>&1 &
TIME_PID=$!

for _ in $(seq 1 100); do
  [ -S "$SOCK" ] && grep -q "listening on" "$DIR/streamer.log" 2>/dev/null && break
  sleep 0.1
done
STREAMER_PID=$(pgrep -P "$TIME_PID" | head -1)
if [ -z "${STREAMER_PID:-}" ]; then echo "{\"arm\":\"$ARM\",\"error\":\"streamer did not start\"}"; exit 1; fi

node "$HERE/multi_rung.mjs" --sock "$SOCK" --port "$PORT" --w "$W" --h "$H" \
  --base "$BASE" --frames "$FRAMES" --fps 25 --rungs "$RUNGS" --scroll "${SCROLL:-0}" \
  >"$DIR/metrics.json" 2>"$DIR/driver.err"

sleep 0.5
kill -TERM "$STREAMER_PID" 2>/dev/null
wait "$TIME_PID" 2>/dev/null

read -r REAL_S USER_S SYS_S < <(awk '/ real /{print $1, $3, $5}' "$DIR/time.txt" | tail -1)
FFMPEG_STARTS=$(grep -c "encoder: ffmpeg started" "$DIR/streamer.log")
PEAK_RUNS=$(grep -o "encoder runs=[0-9]*" "$DIR/streamer.log" | grep -o "[0-9]*" | sort -n | tail -1)

python3 - "$ARM" "$DIR/metrics.json" "$USER_S" "$SYS_S" "$REAL_S" \
  "$FFMPEG_STARTS" "${PEAK_RUNS:-0}" <<'EOF'
import json, sys
arm, mpath, user, syscpu, real, starts, peak = sys.argv[1:8]
m = json.load(open(mpath))
cpu = float(user or 0) + float(syscpu or 0)
stream_s = m.get("feedMs", 1) / 1000
print(json.dumps({
  "arm": arm,
  "cpu_s": round(cpu, 2),
  "wall_s": float(real or 0),
  "stream_s": round(stream_s, 2),
  "cpu_per_stream_s": round(cpu / stream_s, 2),
  "ffmpeg_starts": int(starts),
  "peak_runs_reported": int(peak or 0),
  "viewers": [
    {
      "idx": v["idx"],
      "want_kbps": v["want_kbps"],
      "landed_kbps": (v["inits"][-1]["bitrate_kbps"] if v["inits"] else None),
      "landed_wh": (f'{v["inits"][-1]["width"]}x{v["inits"][-1]["height"]}' if v["inits"] else None),
      "inits": [i["bitrate_kbps"] for i in v["inits"]],
      "resets": v["resets"],
      "packets": v["packets"],
      "bytes": v["bytes"],
    }
    for v in m.get("viewers", [])
  ],
}))
EOF
