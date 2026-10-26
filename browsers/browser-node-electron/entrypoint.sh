#!/usr/bin/env bash
set -euo pipefail

# ── browser-node-electron entrypoint ─────────────────────────────────
# Container lifecycle:
#   1. Root (PID 1): iptables/ip6tables firewall, optional Tor bootstrap
#   2. gosu isolauser: Xvfb, isola-streamer, electron-runner, socat, ffmpeg
#
# Chromium runs with --no-sandbox (no user namespaces); privilege drop is the
# boundary. Capabilities (SYS_ADMIN, NET_ADMIN, …) stay on PID 1 only.

echo "[browser-node-electron] Starting with resolution=${RESOLUTION}, url=${START_URL}"

# ── Parse resolution ─────────────────────────────────────────────────
WIDTH=$(echo "$RESOLUTION" | cut -d'x' -f1)
HEIGHT=$(echo "$RESOLUTION" | cut -d'x' -f2)
export WIDTH
export HEIGHT

# ── Tor configuration (opt-in) ────────────────────────────────────────
# TOR_ENABLED is set by the orchestrator from the session's tor_enabled flag.
# When on, three things change: (1) the Tor daemon runs as its own UID so the
# sandbox's web egress can be scoped to exactly that UID, (2) direct outbound
# web ports are granted ONLY to that UID, so the browser has no unproxied path
# at all, and (3) startup blocks until Tor has a live circuit — a session never
# serves traffic before the proxy is ready. TOR_EXIT_COUNTRY optionally pins the
# exit to one or more countries (ISO 3166-1 alpha-2, comma-separated).
TOR_ACTIVE=0
TOR_USER=""
TOR_UID=""
TOR_PID=""
# Extra `tor` flags for exit-country pinning, empty when no country was set.
TOR_EXIT_COUNTRY_ARGS=()
if [ "${TOR_ENABLED:-0}" = "1" ] || [ "${TOR_ENABLED:-0}" = "true" ]; then
    TOR_ACTIVE=1
    # Debian's tor package ships the daemon user as `debian-tor`; Alpine's as
    # `tor`. Resolve whichever exists so the firewall UID match is portable.
    for candidate in debian-tor tor; do
        if id -u "$candidate" >/dev/null 2>&1; then
            TOR_USER="$candidate"
            TOR_UID="$(id -u "$candidate")"
            break
        fi
    done
    if [ -z "$TOR_UID" ]; then
        echo "[browser-node-electron] FATAL: TOR_ENABLED=1 but no tor daemon user found"
        exit 2
    fi
    # Exit-country pinning. StrictNodes makes the selection a guarantee rather
    # than a preference, so a country with no reachable exit fails the bootstrap
    # gate below instead of silently egressing elsewhere. A malformed value is
    # fatal for the same reason — this is a privacy control, not a hint.
    if [ -n "${TOR_EXIT_COUNTRY:-}" ]; then
        exit_nodes=""
        IFS=',' read -r -a country_codes <<< "$TOR_EXIT_COUNTRY"
        for code in "${country_codes[@]}"; do
            code="$(printf '%s' "$code" | tr -d '[:space:]' | tr '[:upper:]' '[:lower:]')"
            if ! [[ "$code" =~ ^[a-z]{2}$ ]]; then
                echo "[browser-node-electron] FATAL: TOR_EXIT_COUNTRY has invalid code '${code}' (want ISO 3166-1 alpha-2, e.g. se)"
                exit 2
            fi
            if [ -z "$exit_nodes" ]; then
                exit_nodes="{$code}"
            else
                exit_nodes="${exit_nodes},{$code}"
            fi
        done
        if [ -n "$exit_nodes" ]; then
            TOR_EXIT_COUNTRY_ARGS=(--ExitNodes "$exit_nodes" --StrictNodes 1)
        fi
    fi
fi

# tor_circuit_established reports success once Tor has a usable circuit, by
# asking its control port. A false negative only delays the session; a false
# positive would serve unproxied traffic, so the check is deliberately strict.
tor_circuit_established() {
    printf 'AUTHENTICATE\r\nGETINFO status/circuit-established\r\nQUIT\r\n' \
        | socat -T 2 - TCP:127.0.0.1:9051 2>/dev/null \
        | grep -q 'status/circuit-established=1'
}

fatal() {
    echo "[browser-node-electron] FATAL: $1"
    exit 2
}

firewall_fatal() {
    fatal "$1"
}

# ── MAC confinement (fail-closed) ─────────────────────────────────────
# Session containers grant root-in-container a broad capability set (NET_ADMIN,
# SETUID, DAC_OVERRIDE, ...). An enforced kernel LSM — AppArmor's docker-default
# profile or SELinux's container policy — is the layer that keeps that root from
# reaching anything the namespace split does not cover. Refuse to start unless
# one is actually enforcing, so a host that silently failed to apply a profile
# never serves an unconfined sandbox. Opt out with ISOLA_REQUIRE_MAC=0 (not
# recommended).
mac_confined() {
    # AppArmor: module enabled and the current task in an enforcing profile.
    if [ -r /sys/module/apparmor/parameters/enabled ] \
        && [ "$(cat /sys/module/apparmor/parameters/enabled 2>/dev/null)" = "Y" ]; then
        local apparmor_current=""
        [ -r /proc/self/attr/current ] \
            && apparmor_current="$(cat /proc/self/attr/current 2>/dev/null || true)"
        case "$apparmor_current" in
            *"(enforce)") echo "AppArmor ${apparmor_current}"; return 0 ;;
        esac
        return 1
    fi
    # SELinux: enforcing, and a confined (not unconfined) context.
    if [ -r /sys/fs/selinux/enforce ] \
        && [ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = "1" ]; then
        local selinux_current=""
        [ -r /proc/self/attr/current ] \
            && selinux_current="$(cat /proc/self/attr/current 2>/dev/null || true)"
        case "$selinux_current" in
            ""|*unconfined*) return 1 ;;
            *) echo "SELinux ${selinux_current}"; return 0 ;;
        esac
    fi
    return 1
}

require_mac_confinement() {
    if [ "${ISOLA_REQUIRE_MAC:-1}" != "1" ]; then
        echo "[browser-node-electron] WARNING: MAC confinement check disabled (ISOLA_REQUIRE_MAC=0)"
        return 0
    fi
    local active
    if active="$(mac_confined)"; then
        echo "[browser-node-electron] MAC confinement active: ${active}"
        return 0
    fi
    fatal "no enforcing MAC policy confines this container (need AppArmor enforcing or SELinux enforcing) — refusing to start an unconfined sandbox. Set ISOLA_REQUIRE_MAC=0 to override (not recommended)."
}

# Backend peer for streamer/CDP INPUT rules: containerized backend IP, or the
# bridge gateway when the orchestrator runs on the host and dials published ports.
session_peer_ip() {
    if [ -n "${ISOLA_PROXY_IP:-}" ]; then
        echo "${ISOLA_PROXY_IP}"
        return 0
    fi
    local gw
    gw="$(ip route show default 2>/dev/null | awk '{print $3; exit}')"
    if [ -z "$gw" ]; then
        firewall_fatal "cannot determine default gateway for session service ports"
    fi
    echo "$gw"
}

# Docker's embedded resolver (127.0.0.11) is IPv4-only; feeding it to ip6tables
# aborts startup with "host/network not found".
is_ipv4_addr() {
    case "$1" in
        *:*) return 1 ;;
        *) return 0 ;;
    esac
}

is_ipv6_addr() {
    case "$1" in
        *:*) return 0 ;;
        *) return 1 ;;
    esac
}

dns_addr_matches_table() {
    local table="$1"
    local addr="$2"
    if [ "$table" = "iptables" ]; then
        is_ipv4_addr "$addr"
        return
    fi
    if [ "$table" = "ip6tables" ]; then
        is_ipv6_addr "$addr"
        return
    fi
    return 1
}

apply_dns_resolver_allows() {
    local cmd="$1"
    if [ ! -r /etc/resolv.conf ]; then
        return 0
    fi
    while read -r key addr _rest; do
        if [ "$key" = "nameserver" ] && [ -n "${addr:-}" ]; then
            if ! dns_addr_matches_table "$cmd" "$addr"; then
                continue
            fi
            $cmd -A OUTPUT -p udp -d "$addr" --dport 53 -j ACCEPT \
                || firewall_fatal "${cmd} DNS udp to ${addr}"
            $cmd -A OUTPUT -p tcp -d "$addr" --dport 53 -j ACCEPT \
                || firewall_fatal "${cmd} DNS tcp to ${addr}"
        fi
    done < /etc/resolv.conf
}

apply_egress_ipv4() {
    echo "[browser-node-electron] Setting up iptables firewall"
    iptables -P INPUT DROP || firewall_fatal "iptables INPUT policy"
    iptables -P FORWARD DROP || firewall_fatal "iptables FORWARD policy"
    iptables -P OUTPUT DROP || firewall_fatal "iptables OUTPUT policy"

    iptables -A INPUT -i lo -j ACCEPT || firewall_fatal "iptables INPUT lo"
    iptables -A OUTPUT -o lo -j ACCEPT || firewall_fatal "iptables OUTPUT lo"
    iptables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT \
        || firewall_fatal "iptables INPUT established"
    iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT \
        || firewall_fatal "iptables OUTPUT established"

    iptables -A OUTPUT -p tcp --dport 9050 -d 127.0.0.1 -j ACCEPT \
        || firewall_fatal "iptables Tor SOCKS"

    apply_dns_resolver_allows iptables

    if [ -n "${ISOLA_DNS_PROXY_IP:-}" ] && is_ipv4_addr "$ISOLA_DNS_PROXY_IP"; then
        iptables -A OUTPUT -p udp -d "$ISOLA_DNS_PROXY_IP" --dport 53 -j ACCEPT \
            || firewall_fatal "iptables DNS proxy udp"
        iptables -A OUTPUT -p tcp -d "$ISOLA_DNS_PROXY_IP" --dport 53 -j ACCEPT \
            || firewall_fatal "iptables DNS proxy tcp"
    fi

    for net in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16 169.254.0.0/16 127.0.0.0/8; do
        iptables -A OUTPUT -d "$net" -j DROP || firewall_fatal "iptables DROP ${net}"
    done

    if [ "$TOR_ACTIVE" = "1" ]; then
        # Only Tor's own egress is exempt (below). A blanket allow on ports
        # 53/123 let the browser reach any host directly — traffic that never
        # traversed the circuit — so it deliberately has no place here.
        iptables -A OUTPUT -m owner --uid-owner "$TOR_UID" -j ACCEPT \
            || firewall_fatal "iptables Tor UID egress"
    else
        for port in 80 443 53 123; do
            iptables -A OUTPUT -p tcp --dport "$port" -j ACCEPT \
                || firewall_fatal "iptables egress tcp ${port}"
            iptables -A OUTPUT -p udp --dport "$port" -j ACCEPT \
                || firewall_fatal "iptables egress udp ${port}"
        done
    fi
}

apply_egress_ipv6() {
    if [ ! -f /proc/net/if_inet6 ]; then
        return 0
    fi
    if ! command -v ip6tables >/dev/null 2>&1; then
        firewall_fatal "IPv6 is enabled but ip6tables is not available"
    fi
    echo "[browser-node-electron] Setting up ip6tables firewall"
    ip6tables -P INPUT DROP || firewall_fatal "ip6tables INPUT policy"
    ip6tables -P FORWARD DROP || firewall_fatal "ip6tables FORWARD policy"
    ip6tables -P OUTPUT DROP || firewall_fatal "ip6tables OUTPUT policy"

    ip6tables -A INPUT -i lo -j ACCEPT || firewall_fatal "ip6tables INPUT lo"
    ip6tables -A OUTPUT -o lo -j ACCEPT || firewall_fatal "ip6tables OUTPUT lo"
    ip6tables -A INPUT -m state --state ESTABLISHED,RELATED -j ACCEPT \
        || firewall_fatal "ip6tables INPUT established"
    ip6tables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT \
        || firewall_fatal "ip6tables OUTPUT established"

    apply_dns_resolver_allows ip6tables

    if [ -n "${ISOLA_DNS_PROXY_IP:-}" ] && is_ipv6_addr "$ISOLA_DNS_PROXY_IP"; then
        ip6tables -A OUTPUT -p udp -d "$ISOLA_DNS_PROXY_IP" --dport 53 -j ACCEPT \
            || firewall_fatal "ip6tables DNS proxy udp"
        ip6tables -A OUTPUT -p tcp -d "$ISOLA_DNS_PROXY_IP" --dport 53 -j ACCEPT \
            || firewall_fatal "ip6tables DNS proxy tcp"
    fi

    for net in fc00::/7 fe80::/10 ::1/128; do
        ip6tables -A OUTPUT -d "$net" -j DROP || firewall_fatal "ip6tables DROP ${net}"
    done

    if [ "$TOR_ACTIVE" = "1" ]; then
        # Only Tor's own egress is exempt (below); see the IPv4 twin.
        ip6tables -A OUTPUT -m owner --uid-owner "$TOR_UID" -j ACCEPT \
            || firewall_fatal "ip6tables Tor UID egress"
    else
        for port in 80 443 53 123; do
            ip6tables -A OUTPUT -p tcp --dport "$port" -j ACCEPT \
                || firewall_fatal "ip6tables egress tcp ${port}"
            ip6tables -A OUTPUT -p udp --dport "$port" -j ACCEPT \
                || firewall_fatal "ip6tables egress udp ${port}"
        done
    fi
}

apply_session_input_ipv4() {
    local peer="$1"
    iptables -A INPUT -p tcp -s "$peer" --dport 5900 -j ACCEPT \
        || firewall_fatal "iptables INPUT 5900 from ${peer}"
    iptables -A INPUT -p tcp -s "$peer" --dport 9223 -j ACCEPT \
        || firewall_fatal "iptables INPUT 9223 from ${peer}"
}

apply_session_input_ipv6() {
    local peer="$1"
    if [ ! -f /proc/net/if_inet6 ]; then
        return 0
    fi
    if ! command -v ip6tables >/dev/null 2>&1; then
        return 0
    fi
    if ! is_ipv6_addr "$peer"; then
        return 0
    fi
    ip6tables -A INPUT -p tcp -s "$peer" --dport 5900 -j ACCEPT \
        || firewall_fatal "ip6tables INPUT 5900 from ${peer}"
    ip6tables -A INPUT -p tcp -s "$peer" --dport 9223 -j ACCEPT \
        || firewall_fatal "ip6tables INPUT 9223 from ${peer}"
}

# ── Firewall (root only) ──────────────────────────────────────────────
if [ "$(id -u)" = "0" ]; then
    require_mac_confinement
    apply_egress_ipv4
    apply_egress_ipv6
    SESSION_PEER_IP="$(session_peer_ip)"
    apply_session_input_ipv4 "$SESSION_PEER_IP"
    apply_session_input_ipv6 "$SESSION_PEER_IP"

    # ── Tor daemon + fail-closed bootstrap gate (opt-in) ──
    # Run as the Tor user (not root) so the firewall's owner match can tell
    # Tor's egress apart from the browser's. The gate below is the same
    # contract as the Chromium node: no circuit, no session.
    if [ "$TOR_ACTIVE" = "1" ]; then
        TOR_DATA_DIR="/tmp/tor-data"
        rm -rf "$TOR_DATA_DIR"
        mkdir -p "$TOR_DATA_DIR"
        chown "$TOR_UID" "$TOR_DATA_DIR"
        echo "[browser-node-electron] Starting Tor daemon as ${TOR_USER}"
        tor --User "$TOR_USER" \
            --SOCKSPort 9050 \
            --DNSPort 5353 \
            --ControlPort 9051 \
            --CookieAuthentication 0 \
            --DataDirectory "$TOR_DATA_DIR" \
            ${TOR_EXIT_COUNTRY_ARGS[@]+"${TOR_EXIT_COUNTRY_ARGS[@]}"} &
        TOR_PID=$!

        # A country-pinned bootstrap can take longer (fewer candidate exits), so
        # the gate gets more time — but it still fails closed.
        TOR_BOOTSTRAP_SECS=30
        if [ "${#TOR_EXIT_COUNTRY_ARGS[@]}" -gt 0 ]; then
            TOR_BOOTSTRAP_SECS=60
        fi
        echo "[browser-node-electron] Waiting for Tor to bootstrap..."
        TOR_READY=0
        for _ in $(seq 1 "$TOR_BOOTSTRAP_SECS"); do
            if tor_circuit_established; then
                TOR_READY=1
                break
            fi
            sleep 1
        done
        if [ "$TOR_READY" = "1" ]; then
            echo "[browser-node-electron] Tor circuit established"
        else
            echo "[browser-node-electron] FATAL: Tor bootstrap failed after ${TOR_BOOTSTRAP_SECS}s — refusing to start without Tor routing"
            kill "$TOR_PID" 2>/dev/null || true
            exit 2
        fi
    fi

    # Make /run/isola writable for isolauser
    mkdir -p /run/isola
    chown isolauser:isolauser /run/isola
fi

RUN_AS_USER="gosu isolauser"
if [ "$(id -u)" != "0" ]; then
    RUN_AS_USER=""
fi

# Set Electron env (session processes run as isolauser when started as root)
export ELECTRON_DISABLE_SANDBOX=1
export HOME=/tmp
export DISPLAY=
# Honor the requested resolution up to a hard ceiling. Capture size == encode
# size == window size (the encoder never rescales), so mouse coordinates stay
# 1:1 at any size.
#
# The ceiling exists because the sandbox framebuffer is w*h*4 bytes of BGRA
# and the host's memory is finite: a 3456x2234 capture (30 MB/frame) gets the
# Electron process OOM-killed within seconds of the first viewer connecting on
# a 2 GB host. Oversized requests are scaled down *proportionally*, so the
# aspect ratio is preserved and the viewer's object-fit mapping stays
# crop-free. Defaults to 1080p; raise ISOLA_MAX_WIDTH/ISOLA_MAX_HEIGHT (both)
# on a host with enough RAM.
MAX_WIDTH="${ISOLA_MAX_WIDTH:-1920}"
MAX_HEIGHT="${ISOLA_MAX_HEIGHT:-1080}"
PIXELS=$((WIDTH * HEIGHT))
if [ "$WIDTH" -gt "$MAX_WIDTH" ] || [ "$HEIGHT" -gt "$MAX_HEIGHT" ]; then
    scaled=$(awk -v w="$WIDTH" -v h="$HEIGHT" -v mw="$MAX_WIDTH" -v mh="$MAX_HEIGHT" \
        'BEGIN { s = (mw / w) < (mh / h) ? (mw / w) : (mh / h); printf "%d %d", int(w * s), int(h * s) }')
    WIDTH=${scaled% *}
    HEIGHT=${scaled#* }
    echo "[browser-node-electron] Capping capture ${PIXELS}px → ${WIDTH}x${HEIGHT} (max ${MAX_WIDTH}x${MAX_HEIGHT})"
    PIXELS=$((WIDTH * HEIGHT))
fi
# H.264 4:2:0 requires even dimensions; Xvfb/Electron windows do too.
WIDTH=$((WIDTH - (WIDTH % 2)))
HEIGHT=$((HEIGHT - (HEIGHT % 2)))
export RESOLUTION="${WIDTH}x${HEIGHT}"
PIXELS=$((WIDTH * HEIGHT))
export ISOLA_WIDTH="${WIDTH}"
export ISOLA_HEIGHT="${HEIGHT}"

# 25fps, the same ceiling the encoder runs at (fpsCapFor in isola-streamer,
# applied to both the MSE and WebCodecs paths). Capture is damage-driven, so a
# static page ships far fewer frames than this; the cap exists so a page that
# animates cannot hand the encoder frames it would only drop. ffmpeg is fed
# -r <cap> and its CFR filter discards the excess, but the producer has already
# paid to composite, read back and ship those frames by then — so the two
# numbers are kept equal rather than letting capture run 20% ahead of encode.
export ISOLA_FPS="${ISOLA_FPS:-${WEBRTC_FRAMERATE:-25}}"
if [ "$ISOLA_FPS" -gt 25 ]; then
    echo "[browser-node-electron] Capping ISOLA_FPS ${ISOLA_FPS}→25"
    export ISOLA_FPS=25
fi

# Resolution-aware bitrate: 6 Mbps @ 720p, scaling linearly with pixel area.
# The formula is bits-per-pixel preserving, so a 1080p capture gets the same
# per-pixel quality as 720p (~13.5 Mbps) instead of being flattened into a
# low-resolution-quality stream by a fixed ceiling. The 60 Mbps guard only
# exists so a malformed resolution string cannot ask for a nonsense bitrate;
# the MAX_WIDTH/MAX_HEIGHT cap above keeps real values well under it.
# The viewer's ABR ladder steps down from this base when the network can't
# keep up.
BASE_PIXELS=$((1280 * 720))
export ISOLA_BITRATE_KBPS="${ISOLA_BITRATE_KBPS:-$((6000 * PIXELS / BASE_PIXELS))}"
if [ "$ISOLA_BITRATE_KBPS" -lt 2000 ]; then ISOLA_BITRATE_KBPS=2000; fi
if [ "$ISOLA_BITRATE_KBPS" -gt 60000 ]; then ISOLA_BITRATE_KBPS=60000; fi
export BROWSER_THEME="${BROWSER_THEME:-light}"

# ── Audio pipeline: PulseAudio null sink → ffmpeg Opus RTP :5006 ──────
# Electron plays into the sink; ffmpeg captures the monitor so the stream
# carries page audio. The streamer gates forwarding via its /audio toggle
# (default off — the viewer's audio button flips it).
PULSE_SOCKET="/tmp/pulse-socket"
PULSE_COOKIE="/tmp/pulse-cookie"
if [ "${ISOLA_AUDIO:-1}" = "1" ]; then
    echo "[browser-node-electron] Starting PulseAudio null sink (as isolauser)"
    dd if=/dev/urandom of="${PULSE_COOKIE}" bs=256 count=1 2>/dev/null
    chmod 666 "${PULSE_COOKIE}"
    # PulseAudio refuses to run as root, and on Debian it needs a writable
    # config dir (XDG_CONFIG_HOME — HOME alone is ignored for the daemon)
    # plus a writable XDG_RUNTIME_DIR. All inside the sandbox.
    mkdir -p /tmp/pa-home/.config/pulse /tmp/pa-runtime \
        && chown -R isolauser:isolauser /tmp/pa-home /tmp/pa-runtime
    HOME=/tmp/pa-home XDG_CONFIG_HOME=/tmp/pa-home/.config XDG_RUNTIME_DIR=/tmp/pa-runtime \
        gosu isolauser pulseaudio --start \
        --exit-idle-time=-1 \
        --load="module-native-protocol-unix auth-cookie=${PULSE_COOKIE} socket=${PULSE_SOCKET}" \
        --load="module-null-sink sink_name=browser_out sink_properties=device.description=Browser_Output" \
        --load="module-always-sink" \
        2>/dev/null || true
    sleep 1
    if ! pgrep -x pulseaudio >/dev/null 2>&1; then
        echo "[browser-node-electron] WARNING: PulseAudio failed to start — audio capture disabled"
    else
        export PULSE_SERVER="unix:${PULSE_SOCKET}"
        export PULSE_COOKIE="${PULSE_COOKIE}"
        echo "[browser-node-electron] Starting audio capture (monitor → Opus RTP :5006)"
        if [ -n "$RUN_AS_USER" ]; then
            $RUN_AS_USER ffmpeg -y -loglevel warning \
                -f pulse -i browser_out.monitor \
                -c:a libopus -application lowdelay -frame_duration 20 \
                -b:a 32k -packet_loss 10 -fec on -ar 48000 -ac 2 \
                -f rtp udp://127.0.0.1:5006 \
                >/tmp/audio-ffmpeg.err 2>&1 &
        else
            ffmpeg -y -loglevel warning \
                -f pulse -i browser_out.monitor \
                -c:a libopus -application lowdelay -frame_duration 20 \
                -b:a 32k -packet_loss 10 -fec on -ar 48000 -ac 2 \
                -f rtp udp://127.0.0.1:5006 \
                >/tmp/audio-ffmpeg.err 2>&1 &
        fi
        AUDIO_FFMPEG_PID=$!
    fi
fi

# ── Start isola-streamer (UDS listener on /run/isola/frame.sock) ──────
echo "[browser-node-electron] Starting isola-streamer on :5900"
if [ -n "$RUN_AS_USER" ]; then
    $RUN_AS_USER /usr/local/bin/isola-streamer &
else
    /usr/local/bin/isola-streamer &
fi
ISOLA_STREAMER_PID=$!
sleep 1

# ── Start electron-runner (offscreen browser → UDS client) ────────────
echo "[browser-node-electron] Starting Xvfb display :99 (GTK needs display even for offscreen)"
if [ -n "$RUN_AS_USER" ]; then
    $RUN_AS_USER Xvfb :99 -screen 0 "${WIDTH}x${HEIGHT}x24" -ac +extension RANDR &
else
    Xvfb :99 -screen 0 "${WIDTH}x${HEIGHT}x24" -ac +extension RANDR &
fi
XVFB_PID=$!
export DISPLAY=:99
export BROWSER_THEME
# Wait for Xvfb socket to be ready
for i in $(seq 1 10); do
  [ -S /tmp/.X11-unix/X99 ] && break
  sleep 0.2
done

echo "[browser-node-electron] Starting electron-runner"
cd /opt/electron-runner
export RESOLUTION="${WIDTH}x${HEIGHT}"
export ISOLA_FPS
export ISOLA_CTRL_SOCK_UID="$(id -u isolauser 2>/dev/null || true)"
export ISOLA_CTRL_SOCK_GID="$(id -g isolauser 2>/dev/null || true)"
ELECTRON_DATA_DIR="/tmp/electron-data-${SESSION_ID:-$$}"
mkdir -p "${ELECTRON_DATA_DIR}"
chown isolauser:isolauser "${ELECTRON_DATA_DIR}" 2>/dev/null || true
ELECTRON_EXTRA_FLAGS=()
if [ "${ISOLA_STRICT_CERTS:-0}" != "1" ]; then
    ELECTRON_EXTRA_FLAGS+=(--ignore-certificate-errors)
fi
if [ -n "$RUN_AS_USER" ]; then
    $RUN_AS_USER node_modules/electron/dist/electron \
      --no-sandbox \
      --disable-breakpad \
      "${ELECTRON_EXTRA_FLAGS[@]}" \
      --user-data-dir="${ELECTRON_DATA_DIR}" \
      . &
else
    node_modules/electron/dist/electron \
      --no-sandbox \
      --disable-breakpad \
      "${ELECTRON_EXTRA_FLAGS[@]}" \
      --user-data-dir="${ELECTRON_DATA_DIR}" \
      . &
fi
ELECTRON_PID=$!

# ── Forward CDP port ──────────────────────────────────────────────────
echo "[browser-node-electron] Starting CDP socat forward on :9223"
if [ -n "$RUN_AS_USER" ]; then
    $RUN_AS_USER socat TCP-LISTEN:9223,fork,reuseaddr,bind=0.0.0.0 TCP:127.0.0.1:9222 &
else
    socat TCP-LISTEN:9223,fork,reuseaddr,bind=0.0.0.0 TCP:127.0.0.1:9222 &
fi
SOCAT_PID=$!

# ── Signal handlers ───────────────────────────────────────────────────
cleanup() {
    echo "[browser-node-electron] Shutting down..."
    kill ${ELECTRON_PID} 2>/dev/null || true
    kill ${SOCAT_PID} 2>/dev/null || true
    kill ${ISOLA_STREAMER_PID} 2>/dev/null || true
    kill ${XVFB_PID} 2>/dev/null || true
    kill ${AUDIO_FFMPEG_PID} 2>/dev/null || true
    kill ${TOR_PID} 2>/dev/null || true
    pulseaudio -k 2>/dev/null || true
    wait 2>/dev/null
    echo "[browser-node-electron] Shutdown complete"
}
trap cleanup SIGTERM SIGINT

echo "[browser-node-electron] Ready (streamer=${ISOLA_STREAMER_PID}, electron=${ELECTRON_PID})"

# ── Wait for electron (critical process) ──────────────────────────────
wait ${ELECTRON_PID}
echo "[browser-node-electron] Electron exited, shutting down"
cleanup
