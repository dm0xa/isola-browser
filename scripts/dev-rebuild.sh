#!/usr/bin/env bash
set -euo pipefail

# ──────────────────────────────────────────────────────────────────────
# isola — one-shot dev rebuild + run for the single-user stack
#
#   Usage: ./scripts/dev-rebuild.sh           # from the repo root, or
#          bash /abs/path/to/dev-rebuild.sh   # from anywhere
#
#   Does the following in order, aborting on any failure:
#     0. validate toolchain (node, npm, go, podman/docker socket)
#        — the socket is probed, not guessed: see scripts/dev-lib/resolve-docker-host.sh
#          (a TMPDIR-derived guess silently broke every container launch)
#     1. build frontend  (frontend/      →  frontend/dist)
#     2. embed frontend  (frontend/dist  →  backend/cmd/single-user/frontend-dist)
#     3. run Go tests    (./pkg/api)
#     4. build backend   (backend/cmd/single-user  →  backend/isola-su)
#     5. ensure /tmp/isola_jwt_secret.env exists (generate if missing)
#     6. ensure /tmp/isola_start_su.sh exists  (sources secret + env, never leaks tokens to cmdline/ps)
#     7. stop any running isola-su (./isola-su, or the absolute path the
#        start-helper execs — see ISOLA_SU_PATTERN below)
#     8. launch new backend via the start-helper (log → /tmp/isola-backend.log)
#     9. wait for GET /health == "ok"  on  http://127.0.0.1:8080
# ──────────────────────────────────────────────────────────────────────

RED='\033[0;31m';   GREEN='\033[0;32m'
YELLOW='\033[1;33m'; CYAN='\033[0;36m';  NC='\033[0m'
info()    { printf "${CYAN}●${NC} %s\n" "$*"; }
success() { printf "${GREEN}✓${NC} %s\n" "$*"; }
warn()    { printf "${YELLOW}⚠${NC} %s\n" "$*"; }
error()   { printf "${RED}✗${NC} %s\n" "$*"; }
die()     { error "$*"; exit 1; }

# ──  resolve absolute paths from script location, not CWD  ─────────
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_DIR=$(CDPATH=   cd -- "$SCRIPT_DIR/.." && pwd)
FRONTEND_DIR="$REPO_DIR/frontend"
BACKEND_DIR="$REPO_DIR/backend"
EMBED_DIR="$BACKEND_DIR/cmd/single-user/frontend-dist"
BINARY_PATH="$BACKEND_DIR/isola-su"

# ──  runtime file locations (same as the earlier manual workflow) ──
JWT_SECRET_FILE="/tmp/isola_jwt_secret.env"
START_HELPER_FILE="/tmp/start_isola_su.sh"
BACKEND_LOG_FILE="/tmp/isola-backend.log"
ISOLA_DATA_DIR="/tmp/isola-data"
BACKEND_PORT=8080
# Health-check budget in 0.5s ticks. 20 ticks (10s) was thin
# enough that a slow podman/colima round-trip in startup reported a false
# "never returned ok" while the server came up seconds later; 60 ticks (30s)
# still fails fast on a genuinely broken build.
HEALTH_WAIT_TICKS=60

# Matches the relative launch form (./isola-su) AND the absolute path the
# start-helper `exec`s. A bare './isola-su' pattern matched only the former, so a
# backend started by this script looked "not running" on the next run and was
# left alive holding :8080 — the new instance then failed its own health check.
# An ERE path boundary keeps isola-streamer / isola-st* out of the match.
ISOLA_SU_PATTERN='(^|/)isola-su($| )'

tput()  { command -v tput >/dev/null 2>&1 && command tput "$@" || true; }
COLS=${COLUMNS:-$(tput cols 2>/dev/null || echo 72)}
hr()    { printf '%*s\n' "$COLS" '' | tr ' ' '─'; }
banner(){ hr; printf "  %s\n" "$*"; hr; }

# ──────────────────────────────────────────────────────────────────
banner "isola  —  dev rebuild + run"
info "repo    = $REPO_DIR"
info "binary  = $BINARY_PATH"
echo

# ── 0. toolchain & socket sanity  ─────────────────────────────────
info "[0/9] checking toolchain..."
for cmd in node npm go; do
  command -v "$cmd" >/dev/null 2>&1 || die "required binary missing in PATH: $cmd"
done
success "node $(node -v) · npm $(npm -v) · go $(go version | awk '{print $3}')"

# Resolve a *working* docker/podman socket (see scripts/dev-lib/resolve-docker-host.sh).
# An explicit `DOCKER_HOST=... ./scripts/dev-rebuild.sh` still wins, as long as
# a daemon answers on it. A purely path-based guess is not enough: TMPDIR is
# routinely overridden (sandboxes, IDE terminals, launchd, CI), and a wrong
# guess silently points the backend at a socket that does not exist, so every
# `docker run` fails and sessions never get a container.
. "$SCRIPT_DIR/dev-lib/resolve-docker-host.sh"
REQUESTED_DOCKER_HOST="${DOCKER_HOST:-}"
RESOLVED_DOCKER_HOST="$(resolve_docker_host || true)"
if [[ -n "$RESOLVED_DOCKER_HOST" ]]; then
  export DOCKER_HOST="$RESOLVED_DOCKER_HOST"
  if [[ -n "$REQUESTED_DOCKER_HOST" && "$REQUESTED_DOCKER_HOST" != "$RESOLVED_DOCKER_HOST" ]]; then
    warn "DOCKER_HOST=$REQUESTED_DOCKER_HOST did not answer — using $DOCKER_HOST"
  else
    info "docker daemon socket: $DOCKER_HOST"
  fi
else
  warn "no reachable docker/podman socket found — set DOCKER_HOST explicitly if orchestrator fails."
fi

# Check networks required by the orchestrator defaults (created manually).
if command -v docker >/dev/null 2>&1; then
  MISSING_NETS=""
  for net in browser-net isola; do
    if ! docker network inspect "$net" >/dev/null 2>&1; then
      MISSING_NETS="$MISSING_NETS $net"
    fi
  done
  if [[ -n "$MISSING_NETS" ]]; then
    warn "missing docker networks:$MISSING_NETS  →  create with: docker network create <name>"
  fi
fi

# ── 1. build frontend  ────────────────────────────────────────────
echo
info "[1/9] building frontend (tsc -b + vite build)..."
cd "$FRONTEND_DIR"
# node_modules safety: reinstall only if missing. The user can delete
# node_modules manually to force a clean reinstall — we won't guess.
if [[ ! -d node_modules ]]; then
  warn "node_modules missing — running npm ci first..."
  npm ci --no-audit --no-fund --loglevel=error
fi
npm run build --silent
ASSETS_COUNT=$(find dist -type f \( -name '*.js' -o -name '*.css' -o -name 'index.html' \) | wc -l | tr -d ' ')
success "frontend built — $ASSETS_COUNT assets in frontend/dist"

# ── 2. embed dist into the Go //go:embed source tree  ─────────────
echo
info "[2/9] embedding frontend assets into single-user build tree..."
chmod -R u+w "$EMBED_DIR" 2>/dev/null || true
find "$EMBED_DIR" -mindepth 1 -maxdepth 1 ! -name '.gitkeep' -exec rm -rf {} +
# Copy everything from dist except hidden files; preserve structure.
if command -v rsync >/dev/null 2>&1; then
  rsync -a "$FRONTEND_DIR/dist/" "$EMBED_DIR/"
else
  cp -R "$FRONTEND_DIR/dist/"* "$EMBED_DIR/"
fi
# keep .gitkeep so the tracked layout stays intact.
[[ -f "$EMBED_DIR/.gitkeep" ]] || touch "$EMBED_DIR/.gitkeep"
success "frontend-dist synced."

# ── 3. run the api package Go tests  ──────────────────────────────
echo
info "[3/9] running api package tests..."
cd "$BACKEND_DIR"
# -count=1 disables the Go test cache; this is a dev rebuild after
# source changes, cache defeats the purpose of rerunning.
if go test ./pkg/api -count=1 -timeout 60s; then
  success "api tests passed."
else
  die "api tests failed — aborting rebuild before producing a known-bad binary."
fi

# ── 4. build backend binary  ──────────────────────────────────────
echo
info "[4/9] building backend binary (isola-su)..."
cd "$BACKEND_DIR"
bash ./scripts/build.sh ./isola-su ./cmd/single-user
BIN_SIZE=$(/usr/bin/stat -f '%z' "$BINARY_PATH" 2>/dev/null || stat -c '%s' "$BINARY_PATH" 2>/dev/null || echo "?")
BIN_TS=$(/usr/bin/stat -f '%Sm' -t '%Y-%m-%d %H:%M:%S' "$BINARY_PATH" 2>/dev/null || stat -c '%y' "$BINARY_PATH" 2>/dev/null | cut -d. -f1)
success "isola-su built — ${BIN_SIZE} bytes, built ${BIN_TS}"
# NOTE: We intentionally do NOT run `go clean -cache` here. That command
# purges the entire Go build cache (~hundreds of MB) for ALL Go projects
# on this machine. The "undefined symbol" ghost errors that this step
# can occasionally emit (e.g. after a structural refactor in another
# branch) are content-cache false positives — Go's build cache is
# content-addressed, so legitimate source changes already invalidate
# the right entries automatically. If you ever see such a ghost, run
#
#   go clean -cache          # one-time, manual, from project root
#
# once from your terminal; the next ./dev-rebuild.sh will then compile
# cleanly. We do not codify that into the script because (a) it's
# destructive to all your other Go workspaces, (b) it slows rebuilds
# for everyone by 5-15s, and (c) the user's `git checkout` between
# branches is the only legitimate trigger.

# ── 5. ensure JWT secret env file  ────────────────────────────────
# Secret is sourced by the start helper via `set -a/. file/+a` so it
# never appears in `ps`, shell history, or /proc/*/cmdline.
echo
info "[5/9] ensuring JWT secret file at $JWT_SECRET_FILE..."
if [[ ! -f "$JWT_SECRET_FILE" ]]; then
  (umask 077; printf 'JWT_SECRET=%s\n' "$(head -c 32 /dev/urandom | xxd -p -c 64)") > "$JWT_SECRET_FILE"
  success "generated new JWT secret at $JWT_SECRET_FILE"
else
  chmod 600 "$JWT_SECRET_FILE" 2>/dev/null || true
  if ! grep -q '^JWT_SECRET=' "$JWT_SECRET_FILE"; then
    die "$JWT_SECRET_FILE exists but has no JWT_SECRET= line. Fix or remove it to regenerate."
  fi
  info "using existing JWT secret file."
fi

# ── 6. write the start-helper script  ─────────────────────────────
echo
info "[6/9] writing start-helper to $START_HELPER_FILE..."
umask 022
cat > "$START_HELPER_FILE" <<HELPER_EOF
#!/usr/bin/env bash
set -eu
# Sourced env: JWT_SECRET (never in argv / ps / cmdline).
set -a
. $JWT_SECRET_FILE
set +a
# DOCKER_HOST: inherited from the caller when a daemon answers on it, else
# discovered (see scripts/dev-lib/resolve-docker-host.sh — podman machine socket,
# Docker Desktop, /var/run/docker.sock). Launching this helper directly, from a
# shell with a non-default TMPDIR, must still find the daemon.
. $REPO_DIR/scripts/dev-lib/resolve-docker-host.sh
RESOLVED_DOCKER_HOST="\$(resolve_docker_host || true)"
if [[ -n "\$RESOLVED_DOCKER_HOST" ]]; then
  export DOCKER_HOST="\$RESOLVED_DOCKER_HOST"
fi
# Warm pool: take the two keys from .env when present, so the dev server keeps
# the same pre-provisioned containers as the Compose stack instead of running
# with the code defaults. Only these keys are read; .env stays out of the rest
# of the helper's environment.
if [[ -f $REPO_DIR/.env ]]; then
  while IFS='=' read -r key value; do
    case "\$key" in
      WARM_POOL_ENABLED|WARM_POOL_SIZE)
        if [[ -n "\$value" ]]; then export "\$key=\$value"; fi
        ;;
    esac
  done < $REPO_DIR/.env
fi
export PORT="\${PORT:-$BACKEND_PORT}"
export LOG_LEVEL="\${LOG_LEVEL:-debug}"
export CORS_ORIGIN="\${CORS_ORIGIN:-http://localhost:5173,http://localhost:$BACKEND_PORT}"
export ISOLA_DATA_DIR="\${ISOLA_DATA_DIR:-$ISOLA_DATA_DIR}"
mkdir -p "\$ISOLA_DATA_DIR"
cd $BACKEND_DIR
exec $BINARY_PATH
HELPER_EOF
chmod 0755 "$START_HELPER_FILE"
success "start-helper written ($(wc -l < "$START_HELPER_FILE") lines)."

# ── 7. stop any running isola-su  ─────────────────────────────────
echo
info "[7/9] stopping any running isola-su process..."
EXISTING_PIDS=$(pgrep -f "$ISOLA_SU_PATTERN" 2>/dev/null || true)
if [[ -z "$EXISTING_PIDS" ]]; then
  info "no running isola-su — nothing to stop."
else
  warn "found running isola-su PID(s): $(echo "$EXISTING_PIDS" | paste -sd, -)"
  # SIGTERM first, retry with SIGKILL after 3s.
  pkill -TERM -f "$ISOLA_SU_PATTERN" 2>/dev/null || true
  for i in 1 2 3; do
    sleep 1
    if ! pgrep -f "$ISOLA_SU_PATTERN" >/dev/null 2>&1; then break; fi
  done
  if pgrep -f "$ISOLA_SU_PATTERN" >/dev/null 2>&1; then
    warn "SIGTERM not enough — sending SIGKILL..."
    pkill -KILL -f "$ISOLA_SU_PATTERN" 2>/dev/null || true
    sleep 1
  fi
  success "old isola-su stopped."
fi

# ── 8. launch new backend in background via the helper  ────────────
echo
info "[8/9] launching new backend (log → $BACKEND_LOG_FILE)..."
# Truncate log so the `tail` sanity check below is meaningful.
: > "$BACKEND_LOG_FILE"
nohup "$START_HELPER_FILE" >>"$BACKEND_LOG_FILE" 2>&1 &
BACKEND_PID=$!
disown "$BACKEND_PID" 2>/dev/null || true  # no "job terminated" noise on exit
info "backend PID = $BACKEND_PID"

# ── 9. wait for /health == ok  ────────────────────────────────────
echo
info "[9/9] waiting for http://127.0.0.1:${BACKEND_PORT}/health to return 'ok'..."
HEALTH_OK=0
for i in $(seq 1 "$HEALTH_WAIT_TICKS"); do
  if ! kill -0 "$BACKEND_PID" 2>/dev/null; then
    error "backend process exited before reaching healthy state."
    echo "--- last 40 lines of $BACKEND_LOG_FILE ---"
    tail -n 40 "$BACKEND_LOG_FILE"
    exit 1
  fi
  RESPONSE=$(curl -s -m 2 "http://127.0.0.1:${BACKEND_PORT}/health" 2>/dev/null || true)
  if [[ "$RESPONSE" == "ok" ]]; then
    HEALTH_OK=1; break
  fi
  sleep 0.5
done

if [[ "$HEALTH_OK" -ne 1 ]]; then
  error "/health did not return 'ok' within ~$(( HEALTH_WAIT_TICKS / 2 ))s (process is alive)."
  echo "--- last 40 lines of $BACKEND_LOG_FILE ---"
  tail -n 40 "$BACKEND_LOG_FILE"
  die "not healthy — inspect the log above (tail -f $BACKEND_LOG_FILE)."
fi
success "/health = ok (waited ~$(( (i-1) / 2 ))s)."

# ──────────────────────────────────────────────────────────────────
banner "done."
printf "
  ${GREEN}UI (embedded, built assets)${NC}   http://localhost:${BACKEND_PORT}/
  ${CYAN}UI (Vite HMR, if running)${NC}    http://localhost:5173/
  ${YELLOW}health${NC}                       curl http://localhost:${BACKEND_PORT}/health
  ${YELLOW}backend pid${NC}                  $BACKEND_PID
  ${YELLOW}log tail${NC}                     tail -f $BACKEND_LOG_FILE
  ${YELLOW}stop backend${NC}                 pkill -TERM -f '${ISOLA_SU_PATTERN}'
  ${YELLOW}rerun rebuild${NC}                bash $SCRIPT_DIR/$(basename "${BASH_SOURCE[0]}")
\n"
