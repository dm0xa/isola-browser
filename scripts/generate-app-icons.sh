#!/usr/bin/env bash
# Regenerates frontend/public/icons from frontend/public/favicon.svg.
#
#   CHROME=/path/to/chrome scripts/generate-app-icons.sh
#
# Why Chromium and not ImageMagick: ImageMagick's built-in MSVG renderer drops
# the favicon's `fill="none" stroke=...` neon ring, so the icons would silently
# lose the cyan layer. Headless Chromium renders the SVG exactly as the app does.
#
# Two purposes come out of one artwork. `any` is the plain launcher icon;
# `maskable` is the same artwork scaled down so it survives the launcher's
# circular crop (safe zone = the inner 80% circle, i.e. every non-background
# pixel within 0.4 * size of the centre).
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
src="$repo_root/frontend/public/favicon.svg"
out="$repo_root/frontend/public/icons"
bg="#030712" # gray-950, the app's dark surface (manifest background_color)

if [[ -z "${CHROME:-}" ]]; then
  echo "CHROME must point at a Chromium/Chrome binary" >&2
  exit 1
fi
if ! command -v magick >/dev/null 2>&1; then
  echo "ImageMagick (magick) is required" >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 2048 so downscaling to 512 keeps the 10px strokes smooth.
"$CHROME" --headless --disable-gpu --hide-scrollbars \
  --default-background-color=00000000 --window-size=2048,2048 \
  --screenshot="$tmp/logo.png" "file://$src" >/dev/null 2>&1
magick "$tmp/logo.png" -trim +repage "$tmp/art.png"

mkdir -p "$out"
# 360/512 = 70% for `any`; 268/512 = 52% keeps the artwork inside the maskable
# safe circle (half-diagonal ~180px < 204.8px).
magick -size 512x512 "xc:$bg" \( "$tmp/art.png" -resize 360x360 \) -gravity center -composite "$out/icon-512.png"
magick -size 512x512 "xc:$bg" \( "$tmp/art.png" -resize 268x268 \) -gravity center -composite "$out/maskable-512.png"
magick "$out/icon-512.png" -resize 192x192 "$out/icon-192.png"
magick "$out/icon-512.png" -resize 180x180 "$out/apple-touch-icon.png"

echo "wrote $out/{icon-192,icon-512,maskable-512,apple-touch-icon}.png"
