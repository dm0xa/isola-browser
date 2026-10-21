#!/usr/bin/env bash
# ──────────────────────────────────────────────────────────────────────
# Resolve a *working* Docker-compatible API socket.
#
# Why this exists: the dev stack (dev-rebuild.sh + the start-helper it
# generates) used to guess the daemon socket as "$TMPDIR/podman/…". TMPDIR is
# overridden by sandboxes, IDE terminals, launchd jobs and CI runners, so the
# guess missed silently and the backend fell back to config's default
# unix:///var/run/docker.sock — which does not exist on macOS. Every
# `docker run` then failed with "Cannot connect to the Docker daemon" and
# sessions never got a container; the viewer surfaces that as
# "The browser session you are looking for does not exist or has expired".
#
# `podman machine inspect` cannot fix this either: the podman CLI derives the
# socket path from *its own* TMPDIR, so it reports a path under the overridden
# temp dir (and rewrites its ~/.podman symlink to match, going stale when that
# temp dir is reaped). That is why discovery globs every per-user temp root
# instead of trusting the CLI.
#
# A candidate wins only when a daemon actually answers on it, so a stopped
# Docker Desktop socket can never be chosen over a live Podman machine.
#
# The CLI's *active context* is tried before the candidate list, because that is
# the daemon the user's own `docker ps` talks to: with colima and Docker Desktop
# both installed, a path-only guess can land on the other daemon, which holds
# none of the user's images or networks.
# ──────────────────────────────────────────────────────────────────────

# candidate_sockets prints every socket path worth trying, best first.
candidate_sockets() {
  local sock
  # macOS per-user temp roots: the live Podman machine socket lives here no
  # matter what TMPDIR this shell happens to have.
  for sock in /var/folders/*/*/T/podman/*.sock; do
    [[ -S "$sock" ]] && printf '%s\n' "$sock"
  done
  # Same layout, when TMPDIR *is* the real one.
  [[ -n "${TMPDIR:-}" ]] && printf '%s\n' "${TMPDIR%/}"/podman/*.sock
  # Linux rootless.
  [[ -n "${XDG_RUNTIME_DIR:-}" ]] && printf '%s\n' "${XDG_RUNTIME_DIR%/}"/podman/podman.sock
  [[ -n "${XDG_RUNTIME_DIR:-}" ]] && printf '%s\n' "${XDG_RUNTIME_DIR%/}"/docker.sock
  # Podman's "Docker API" symlink — convenient, but stale after a sandboxed
  # podman run, so it is only ever a candidate, never a default.
  printf '%s\n' "${HOME}/.podman/podman-machine-default-api.sock"
  # Colima and OrbStack sockets. Fallbacks only: resolve_docker_host tries the
  # active docker context first, so a colima-backed CLI is never overridden by
  # a different daemon that merely happens to be running.
  printf '%s\n' "${HOME}/.colima/default/docker.sock"
  printf '%s\n' "${HOME}/.orbstack/run/docker.sock"
  # Docker Desktop (macOS), then stock docker.
  printf '%s\n' "${HOME}/.docker/run/docker.sock"
  printf '%s\n' "/var/run/docker.sock"
}

# probe_docker_host HOST — does a daemon with a real server version answer?
# Bounded by DOCKER_PROBE_TICKS quarter-seconds (default 20 = 5s): a stale
# remote tcp:// DOCKER_HOST would otherwise stall the rebuild for the full TCP
# timeout. Requires non-empty output as well as exit 0 — `docker info
# --format` exits 0 with an empty template result, which would let a dead
# socket pass as reachable.
probe_docker_host() {
  local host="$1" pid ticks="${DOCKER_PROBE_TICKS:-20}" out_file rc
  command -v docker >/dev/null 2>&1 || return 1
  out_file=$(mktemp 2>/dev/null) || return 1
  DOCKER_HOST="$host" docker info --format '{{.ServerVersion}}' >"$out_file" 2>/dev/null &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    if [[ "$ticks" -le 0 ]]; then
      kill -TERM "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      rm -f "$out_file"
      return 1
    fi
    sleep 0.25
    ticks=$((ticks - 1))
  done
  wait "$pid" 2>/dev/null
  rc=$?
  [[ -s "$out_file" ]] || rc=1
  rm -f "$out_file"
  return "$rc"
}

# docker_socket_responds SOCKET — a daemon answers over this unix socket.
docker_socket_responds() {
  local sock="$1"
  [[ -n "$sock" && -S "$sock" ]] || return 1
  probe_docker_host "unix://$sock"
}

# current_context_socket prints the unix socket of the CLI's active docker
# context, or nothing (exit 1). `docker context inspect` reports what the user's
# own `docker ps` uses, independently of any DOCKER_HOST in this shell.
current_context_socket() {
  local host
  command -v docker >/dev/null 2>&1 || return 1
  host=$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null) || return 1
  [[ -n "$host" && "$host" == unix:///* ]] || return 1
  printf '%s\n' "${host#unix://}"
}

# resolve_docker_host prints the DOCKER_HOST value to export (unix:///path),
# or nothing (exit 1) when no socket is usable.
resolve_docker_host() {
  local sock

  if [[ -n "${DOCKER_HOST:-}" ]] && probe_docker_host "$DOCKER_HOST"; then
    printf '%s\n' "$DOCKER_HOST"
    return 0
  fi

  # The active context is what the user's shell talks to. A stopped daemon still
  # loses, because the probe has to answer before the socket is accepted.
  if sock=$(current_context_socket) && docker_socket_responds "$sock"; then
    printf 'unix://%s\n' "$sock"
    return 0
  fi

  while IFS= read -r sock; do
    if docker_socket_responds "$sock"; then
      printf 'unix://%s\n' "$sock"
      return 0
    fi
  done < <(candidate_sockets)

  # Nothing answered. Fall back to the first socket that at least exists, so
  # the caller gets a concrete path — and a real connection error from the
  # orchestrator — instead of silently pointing at /var/run/docker.sock.
  while IFS= read -r sock; do
    if [[ -n "$sock" && -S "$sock" ]]; then
      printf 'unix://%s\n' "$sock"
      return 0
    fi
  done < <(candidate_sockets)

  return 1
}
