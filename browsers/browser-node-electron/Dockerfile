# Multi-stage build for Electron offscreen browser container (isola transport).
#
# Stage 1: go-builder — cross-compile isola-streamer static binary
# Stage 2: node-builder — install Electron + electron-runner deps
# Stage 3: runtime — Debian slim + Electron runtime deps + ffmpeg + tor

FROM golang:1.26-alpine AS go-builder

# ca-certificates: only needed when modules are downloaded (i.e. no vendor/).
RUN apk add --no-cache ca-certificates && update-ca-certificates

WORKDIR /src
COPY isola-streamer ./isola-streamer
# No -mod=vendor: Go uses a vendor/ tree when present (offline) and downloads
# modules otherwise (clean clone, where vendor/ is absent).
RUN cd isola-streamer && CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/isola-streamer .

FROM node:22-bookworm-slim AS node-builder

WORKDIR /app
# Electron needs a build context to resolve prebuilt binaries
COPY electron-runner/package.json ./
RUN npm install --production

# Electron 44 no longer downloads the platform binary from a package
# `postinstall` hook — its manifest ships no `scripts` at all. The fetch is now
# lazy (on the first `require('electron')`, or explicitly via the exported
# `install-electron` bin). Do it eagerly here: the prune below needs dist/ to
# exist, and the runtime image has no Node.js to trigger the lazy path, so
# without this the sandbox has no Electron binary at all.
RUN node node_modules/electron/install.js

COPY electron-runner/src/ ./src/

# Prune the Electron distribution down to what this container actually loads,
# before it is copied into the runtime image. Both removals are checked against
# a live session's /proc/<pid>/maps rather than guessed:
#
#   locales/    41 MB, 55 .pak files. Only en-US.pak is ever mapped — the
#               offscreen window never sets --lang, so Chromium loads the
#               en-US bundle and falls back to it for any page that asks for
#               a locale this build no longer ships.
#   clang_x64_v8_arm64/
#               11 MB, a cross-arch ICU + snapshot duplicate left in the
#               packaged build. No process maps either file; the copy the
#               binary actually reads is icudtl.dat in dist/ itself.
#
# Together these are ~51 MB of the 285 MB runner payload.
RUN set -eux; \
    d=node_modules/electron/dist; \
    find "$d/locales" -maxdepth 1 -type f ! -name 'en-US.pak' -delete; \
    rm -rf "$d/clang_x64_v8_arm64"

# debian:bookworm-slim rather than node:22-bookworm-slim: the runtime needs no
# Node.js at all. Electron embeds its own, and the runner is launched as
# node_modules/electron/dist/electron. The Node base contributed ~150 MB of
# node/npm/corepack/yarn plus /usr/local/include that nothing here executes.
FROM debian:bookworm-slim

# Electron runtime dependencies (Debian)
# See: https://www.electronjs.org/docs/latest/development/build-instructions-linux
RUN apt-get update && apt-get install -y --no-install-recommends \
    # Electron deps
    libgtk-3-0 libnotify4 libnss3 libxss1 libxtst6 libasound2 \
    libgbm1 xdg-utils \
    # isola infra (pulseaudio: audio sink + monitor capture for the stream)
    # iproute2: entrypoint's session_peer_ip falls back to `ip route show
    # default` when the orchestrator runs on the host (no ISOLA_PROXY_IP).
    ffmpeg socat tor iptables iproute2 pulseaudio pulseaudio-utils \
    xvfb ca-certificates \
    # Electron's bundled V8 links libatomic on arm64; the Node base image
    # happened to provide it, a bare Debian slim does not.
    libatomic1 \
    # privilege drop
    gosu \
    # Adwaita's per-size icon directories are 24 MB of GTK stock icons that an
    # offscreen window never renders. cursors/ is kept: that is the X11 cursor
    # theme Xvfb falls back to, and dropping it loses the pointer entirely.
    && find /usr/share/icons/Adwaita -maxdepth 1 -mindepth 1 -type d ! -name cursors \
        -exec rm -rf {} + \
    # cleanup
    && apt-get clean && rm -rf /var/lib/apt/lists/*

# Create isolauser (matches browser-node-chromium uid/gid convention)
RUN useradd -m -s /bin/bash isolauser

# Copy isola-streamer binary
COPY --from=go-builder /out/isola-streamer /usr/local/bin/isola-streamer

# Copy electron-runner + pre-installed node_modules
COPY --from=node-builder /app /opt/electron-runner

# Copy entrypoint
COPY browser-node-electron/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh /usr/local/bin/isola-streamer

# Container dirs (orchestrator uses --tmpfs for writable paths on read-only rootfs)
RUN mkdir -p /run/isola && chown isolauser:isolauser /run/isola

ENV RESOLUTION=1280x720
ENV START_URL=https://example.com
# Matches the encoder's own CFR ceiling (fpsCapFor in isola-streamer): the
# offscreen compositor is told to produce exactly the frames the encoder will
# emit, so nothing is captured that ffmpeg would discard.
ENV ISOLA_FPS=25
ENV ISOLA_FRAME_SOCK=/run/isola/frame.sock

EXPOSE 5900
EXPOSE 9223

# bash's /dev/tcp instead of node's fetch: there is no Node.js in this image.
# grep -m1 exits on the first match so the check cannot block on the response.
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD timeout 2 bash -c 'exec 3<>/dev/tcp/127.0.0.1/5900 && printf "GET /healthz HTTP/1.0\r\n\r\n" >&3 && grep -qm1 " 200 " <&3'

ENTRYPOINT ["/entrypoint.sh"]
