#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────
# isola doctor — read-only diagnostics for "the sandbox could not start"
#
# Answers one question: is the failure in Docker's host port publishing,
# in the container's streamer, or in the backend? It never creates,
# starts, stops, removes or restarts anything, and writes nothing.
#
#   scripts/doctor.sh
#
# Exit status: 0 only when VERDICT is ok.
# ────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Colour

info()    { printf "${CYAN}●${NC} %s\n" "$*"; }
success() { printf "${GREEN}✓${NC} %s\n" "$*"; }
warn()    { printf "${YELLOW}⚠${NC} %s\n" "$*"; }
error()   { printf "${RED}✗${NC} %s\n" "$*"; }

# Pick one portable connectivity probe and say which one is in use: some
# shells cannot open /dev/tcp, so the probe must not rely on it.
PROBE_KIND=""
if command -v nc >/dev/null 2>&1; then
    PROBE_KIND="nc -z -w 2"
elif command -v curl >/dev/null 2>&1; then
    PROBE_KIND="curl --max-time 2"
fi

probe_host_port() { # host port
    if [[ "$PROBE_KIND" == nc* ]]; then
        nc -z -w 2 "$1" "$2" >/dev/null 2>&1
    else
        curl --max-time 2 --connect-timeout 2 -s -o /dev/null "http://$1:$2/" >/dev/null 2>&1
    fi
}

strip_colima_log() {
    local line="$1"
    line="${line#*msg=}"
    line="${line%\"}"
    line="${line#\"}"
    printf '%s' "$line"
}

docker_ok=0
running_total=0
published_total=0
host_ok=0
internal_ok=0
internal_fail=0

printf '%s\n' "isola doctor — read-only, no containers are changed"
printf '%s\n' "────────────────────────────────────────────────────────────"

# ── 1. Docker runtime ───────────────────────────────────────────────
info "[1/5] docker"
docker_summary="$(docker info --format 'server {{.ServerVersion}} · {{.OperatingSystem}}' 2>&1)" || true
if [[ "$docker_summary" == server* ]]; then
    docker_ok=1
    success "docker: ${docker_summary}"
else
    error "docker: $(printf '%s' "$docker_summary" | head -1)"
fi

if command -v colima >/dev/null 2>&1; then
    colima_line="$(colima status 2>&1 | head -1 || true)"
    if [[ -n "$colima_line" ]]; then
        colima_line="$(strip_colima_log "$colima_line")"
        if [[ "$colima_line" == *"is running"* ]]; then
            success "colima: ${colima_line}"
        else
            warn "colima: ${colima_line}"
        fi
    fi
fi

# ── 2. isola containers and their published 5900/tcp mapping ────────
info "[2/5] isola containers (docker ps -a, name prefix isola-)"
containers=""
if [[ "$docker_ok" -eq 0 ]]; then
    warn "docker is unavailable — skipping container checks"
else
    containers="$(docker ps -a --filter 'name=^isola-' --format '{{.Names}}\t{{.Status}}' 2>/dev/null || true)"
    if [[ -z "$containers" ]]; then
        warn "no isola containers (running or stopped)"
    else
        while IFS=$'\t' read -r cname cstatus; do
            [[ -n "$cname" ]] || continue
            printf '    %-28s %s\n' "$cname" "$cstatus"
            case "$cstatus" in
                Up*) ;;
                *) continue ;;
            esac
            running_total=$((running_total + 1))
            mappings="$(docker port "$cname" 5900/tcp 2>/dev/null || true)"
            if [[ -z "$mappings" ]]; then
                warn "${cname}: no published 5900/tcp mapping"
                continue
            fi
            while IFS= read -r mapping; do
                [[ -n "$mapping" ]] || continue
                published_total=$((published_total + 1))
                printf '    %-28s 5900/tcp → %s\n' "" "$mapping"
            done <<< "$mappings"
        done <<< "$containers"
    fi
fi

# ── 3. probe each published address from the host ───────────────────
info "[3/5] host reachability${PROBE_KIND:+ (probe: $PROBE_KIND)}"
if [[ "$docker_ok" -eq 0 ]]; then
    warn "docker is unavailable — skipping host probe"
elif [[ -z "$PROBE_KIND" ]]; then
    warn "neither nc nor curl found — cannot probe published ports from the host"
elif [[ "$published_total" -eq 0 ]]; then
    warn "nothing published to probe"
else
    while IFS=$'\t' read -r cname cstatus; do
        [[ -n "$cname" ]] || continue
        case "$cstatus" in Up*) ;; *) continue ;; esac
        seen=""
        while IFS= read -r mapping; do
            [[ -n "$mapping" ]] || continue
            case " $seen " in *" $mapping "*) continue ;; esac
            seen="$seen $mapping"
            phost="${mapping%:*}"
            pport="${mapping##*:}"
            # A wildcard bind is probed on loopback, the way the backend does.
            case "$phost" in
                0.0.0.0|'[::]'|::) phost="127.0.0.1" ;;
            esac
            if probe_host_port "$phost" "$pport"; then
                host_ok=$((host_ok + 1))
                success "${cname}: ${mapping} reachable from the host"
            else
                error "${cname}: ${mapping} refused from the host"
            fi
        done <<< "$(docker port "$cname" 5900/tcp 2>/dev/null || true)"
    done <<< "$containers"
fi

# ── 4. probe the streamer inside the same container ─────────────────
info "[4/5] streamer inside the container (GET /healthz on 127.0.0.1:5900)"
if [[ "$docker_ok" -eq 0 ]]; then
    warn "docker is unavailable — skipping container probe"
elif [[ "$running_total" -eq 0 ]]; then
    warn "no running containers to probe"
else
    while IFS=$'\t' read -r cname cstatus; do
        [[ -n "$cname" ]] || continue
        case "$cstatus" in Up*) ;; *) continue ;; esac
        healthz="$(docker exec "$cname" bash -c \
            'exec 3<>/dev/tcp/127.0.0.1/5900 && printf "GET /healthz HTTP/1.0\r\n\r\n" >&3 && head -1 <&3' \
            2>&1)" || true
        if [[ "$healthz" == HTTP/*200* ]]; then
            internal_ok=$((internal_ok + 1))
            success "${cname}: ${healthz}"
        else
            internal_fail=$((internal_fail + 1))
            error "${cname}: ${healthz:-no response}"
        fi
    done <<< "$containers"
fi

# ── 5. backend ──────────────────────────────────────────────────────
backend_url="${ISOLA_URL:-}"
if [[ -z "$backend_url" ]]; then
    backend_url="http://localhost:${ISOLA_PORT:-8080}"
fi
backend_url="${backend_url%/}"
info "[5/5] backend"
backend_ok=0
if command -v curl >/dev/null 2>&1; then
    backend_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "${backend_url}/api/config" 2>/dev/null || true)"
    if [[ -n "$backend_code" && "$backend_code" != "000" ]]; then
        backend_ok=1
        success "${backend_url}/api/config → HTTP ${backend_code}"
    else
        error "${backend_url}/api/config → no response"
    fi
else
    bwout="${backend_url#http://}"
    bhost="${bwout%%:*}"
    bport="${bwout##*:}"
    bport="${bport%%/*}"
    if probe_host_port "$bhost" "$bport"; then
        backend_ok=1
        warn "${backend_url} port open (curl missing, HTTP status not checked)"
    else
        error "${backend_url} unreachable"
    fi
fi

# ── verdict ─────────────────────────────────────────────────────────
printf '%s\n' "────────────────────────────────────────────────────────────"
if [[ "$docker_ok" -eq 0 ]]; then
    printf 'VERDICT: docker daemon is not reachable — start the Docker runtime (colima start) and re-run\n'
    exit 1
elif [[ "$backend_ok" -eq 0 ]]; then
    printf 'VERDICT: backend not reachable — start isola or check the port (%s)\n' "$backend_url"
    exit 1
elif [[ "$running_total" -eq 0 ]]; then
    printf 'VERDICT: no isola containers running (warm pool disabled or backend stopped)\n'
    exit 1
elif [[ "$internal_fail" -gt 0 && "$internal_ok" -eq 0 ]]; then
    printf 'VERDICT: containers are up but the streamer is not answering inside them — check the isola log and the container\n'
    exit 1
elif [[ -n "$PROBE_KIND" && "$published_total" -gt 0 && "$host_ok" -lt "$published_total" ]]; then
    printf 'VERDICT: host port publishing is broken (containers healthy) — run: colima restart\n'
    exit 1
elif [[ "$published_total" -eq 0 ]]; then
    warn "containers run without published ports; sessions fall back to the container IP (browser-net)"
    printf 'VERDICT: ok\n'
    exit 0
else
    printf 'VERDICT: ok\n'
    exit 0
fi
