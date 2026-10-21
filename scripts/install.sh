#!/usr/bin/env bash
set -euo pipefail

# ────────────────────────────────────────────────────────────
# isola install script
# Usage: curl -fsSL https://raw.githubusercontent.com/dm0xa/isola-browser/main/scripts/install.sh | bash
# ────────────────────────────────────────────────────────────

readonly DOWNLOAD_BASE="${ISOLA_DOWNLOAD_BASE:-https://github.com/dm0xa/isola-browser/releases/latest/download}"
readonly RELEASES_URL="${ISOLA_RELEASES_URL:-https://github.com/dm0xa/isola-browser/releases}"
readonly BINARY_NAME="isola"

# Install directory, resolved by resolve_install_dir() before use. The default
# is /opt/isola; an unprivileged install that cannot create it falls back to
# ~/.isola rather than aborting.
INSTALL_DIR=""

# Per-session sandbox image the isola binary launches at runtime.
readonly BROWSER_IMAGE="isola-browser:electron"
# Registry repository the released sandbox image is published under. The binary
# and the image ship together and share the ISOL wire header and ABR ladder, so
# the installer prefers the tag matching this release and only then falls back
# to the floating `:electron` tag. ISOLA_BROWSER_IMAGE_REMOTE (read directly in
# browser_image_candidates) pins one exact reference instead; an empty value
# skips the registry.
readonly BROWSER_IMAGE_REPOSITORY="${ISOLA_BROWSER_IMAGE_REPOSITORY:-ghcr.io/dm0xa/isola-browser}"

# Source checkout root — only needed to build BROWSER_IMAGE. Empty when this
# script is piped straight from curl with no checkout nearby.
PROJECT_ROOT=""
_script_path="${BASH_SOURCE[0]:-}"
if [[ -n "$_script_path" && -f "$_script_path" ]]; then
    PROJECT_ROOT="$(cd "$(dirname "$_script_path")/.." && pwd)"
elif [[ -f "./docker-compose.yml" && -d "./browsers" ]]; then
    PROJECT_ROOT="$(pwd)"
fi
readonly PROJECT_ROOT

# ── colour helpers ──────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Colour

info()    { printf "${CYAN}●${NC} %s\n" "$*"; }
success() { printf "${GREEN}✓${NC} %s\n" "$*"; }
warn()    { printf "${YELLOW}⚠${NC} %s\n" "$*"; }
error()   { printf "${RED}✗${NC} %s\n" "$*"; }

# ── which version to install ───────────────────────────────────────────
# DOWNLOAD_BASE is the /releases/latest/download alias, so the version in the
# asset filename must match whatever release is latest *right now*. A hard-coded
# pin does not merely install an older version — it asks for an asset the newer
# release does not contain, and the download 404s. ISOLA_VERSION skips the
# lookup and pins an exact version.
resolve_version() {
    if [[ -n "${ISOLA_VERSION:-}" ]]; then
        printf '%s' "${ISOLA_VERSION#v}"
        return 0
    fi

    # /releases/latest redirects to /releases/tag/vX.Y.Z. Follow it and read the
    # tag out of the final URL: no API call, so no rate limit and no jq.
    local effective=""
    effective="$(curl -fsSIL -o /dev/null -w '%{url_effective}' "${RELEASES_URL}/latest" 2>/dev/null || true)"
    case "$effective" in
        */tag/v*) printf '%s' "${effective##*/tag/v}"; return 0 ;;
    esac
    return 1
}

if ! VERSION="$(resolve_version)"; then
    error "Could not determine the latest isola release."
    error "Check your network, or pin a version: ISOLA_VERSION=1.0.0"
    exit 1
fi
readonly VERSION

# ── detect OS / arch ────────────────────────────────────────────────
detect_platform() {
    local os arch

    case "$(uname -s)" in
        Linux)  os="linux" ;;
        Darwin) os="darwin" ;;
        *)      error "Unsupported OS: $(uname -s)"; exit 1 ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *)            error "Unsupported arch: $(uname -m)"; exit 1 ;;
    esac

    echo "${os}/${arch}"
}

# ── install directory (ISOLA_INSTALL_DIR wins) ──────────────────────
# /opt/isola is where a service install belongs. When the invoking user cannot
# create it, fall back to ~/.isola instead of aborting, so the same script works
# for a system install and for an unprivileged one on any host.
resolve_install_dir() {
    if [[ -n "${ISOLA_INSTALL_DIR:-}" ]]; then
        INSTALL_DIR="${ISOLA_INSTALL_DIR}"
        return 0
    fi

    INSTALL_DIR="/opt/isola"
    if mkdir -p "$INSTALL_DIR" 2>/dev/null && [[ -w "$INSTALL_DIR" ]]; then
        return 0
    fi

    INSTALL_DIR="${HOME}/.isola"
    info "Cannot write /opt/isola; installing to ${INSTALL_DIR} instead (set ISOLA_INSTALL_DIR to choose another path)."
    mkdir -p "$INSTALL_DIR"
}

service_file_exists() {
    if [[ "$1" == "darwin" ]]; then
        [[ -f "${HOME}/Library/LaunchAgents/com.isola.plist" ]]
    else
        [[ -f /etc/systemd/system/isola.service ]]
    fi
}

# ── stop the previous service or process before replacing the binary ───
stop_isola_if_running() {
    local os="$1"
    local isola_pid

    # A managed service restarts its own process (Restart=always /
    # KeepAlive=true), so stop it through the supervisor first instead of
    # racing it with kill.
    if service_file_exists "$os"; then
        info "Stopping the isola service before replacing the binary…"
        if [[ "$os" == "darwin" ]]; then
            launchctl bootout "gui/$(id -u)/com.isola" >/dev/null 2>&1 || true
        elif command -v systemctl >/dev/null 2>&1; then
            if [[ $EUID -eq 0 ]]; then
                systemctl stop isola >/dev/null 2>&1 || true
            else
                # -n: never block a piped install on a sudo password prompt.
                sudo -n systemctl stop isola >/dev/null 2>&1 || true
            fi
        fi
    fi

    isola_pid="$(pgrep -x isola 2>/dev/null | head -1 || true)"

    if [[ -z "$isola_pid" ]]; then
        info "No running isola process detected."
        return 0
    fi

    warn "isola is already running (PID $isola_pid). It must be stopped before upgrading."
    local answer="Y"
    if [[ -r /dev/tty ]]; then
        printf "${CYAN}Stop isola now? [Y/n] ${NC}"
        read -r answer </dev/tty || answer="Y"
    fi
    if [[ "$answer" =~ ^[Nn] ]]; then
        warn "Skipping shutdown. Stop it manually before restarting."
        return 0
    fi

    kill "$isola_pid" 2>/dev/null || true
    sleep 1
    if kill -0 "$isola_pid" >/dev/null 2>&1; then
        kill -9 "$isola_pid" 2>/dev/null || true
    fi
    success "isola stopped."
}

# ── environment file (single source of truth for the service) ────────
# The systemd unit reads it through EnvironmentFile and the macOS LaunchAgent
# has it rendered into EnvironmentVariables, so it survives upgrades and is the
# one place an operator edits.
env_file_path() {
    printf '%s/isola.env' "${INSTALL_DIR}"
}

write_env_file() {
    local env_file
    env_file="$(env_file_path)"
    [[ -f "$env_file" ]] && return 0

    cat > "$env_file" <<ENVEOF
# isola environment — edit as needed. macOS: re-run the installer to reload the
# LaunchAgent. Linux: sudo systemctl restart isola
ISOLA_PORT=8080
# Session snapshot + per-session policy files. Must be writable by the running
# user; the built-in default (/var/lib/isola) is root-owned, so leaving this
# unset silently disables persistence.
ISOLA_DATA_DIR=${INSTALL_DIR}/data
# Per-session sandbox container limits. The CPU limit must not exceed this
# host's core count: the Docker daemon rejects a --cpus value above it. Left
# unset, isola uses min(3, host cores). Uncomment to pin different caps:
# ISOLA_SESSION_MEMORY_MB=2048
# ISOLA_SESSION_CPUS=2
# ISOLA_SESSION_PIDS=512
# Require an enforcing MAC policy (AppArmor/SELinux) around session containers.
# The sandbox refuses to start without one. Uncomment to allow a host that has
# no LSM (not recommended):
# ISOLA_REQUIRE_MAC=0
ENVEOF
    chmod 600 "$env_file"
    info "Created env file: ${env_file}"
}

# ── MAC confinement: what the sandbox entrypoint will accept ──────────
# docker_lsm_state returns 0 when the daemon applies an AppArmor/SELinux
# profile, 1 when it applies neither, and 2 when the daemon is unreachable.
docker_lsm_state() {
    local opts=""
    if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
        return 2
    fi
    opts="$(docker info --format '{{json .SecurityOptions}}' 2>/dev/null || true)"
    case "$opts" in
        *apparmor*|*AppArmor*|*selinux*|*SELinux*) return 0 ;;
    esac
    return 1
}

# The sandbox refuses to start without one. Report it and leave the decision to
# the operator; the installer never writes ISOLA_REQUIRE_MAC for them.
report_lsm_state() {
    local docker_ready="$1" state=0

    if [[ "$docker_ready" -ne 1 ]]; then
        info "Docker is not running yet — MAC confinement is checked on the next install run."
        return 0
    fi

    docker_lsm_state || state=$?
    case "$state" in
        0)
            return 0
            ;;
        1)
            warn "No enforcing AppArmor/SELinux profile detected — sessions will refuse to start (fail-closed)."
            info "Enable an LSM on the host, or set ISOLA_REQUIRE_MAC=0 in $(env_file_path) to allow an unconfined sandbox (not recommended)."
            ;;
        *)
            info "Docker is not reachable — MAC confinement is checked on the next install run."
            ;;
    esac
}

# ── set up as system service (systemd or launchd) ─────────────────
setup_service() {
    local os="$1"

    write_env_file

    if [[ "$os" == "linux" ]]; then
        write_systemd_unit
    elif [[ "$os" == "darwin" ]]; then
        write_launchagent
        reload_launchagent
        success "LaunchAgent installed and (re)loaded."
        info "Environment: $(env_file_path) — edit it, then re-run this installer to reload."
        info "Logs: ${INSTALL_DIR}/isola.log"
    fi
}

write_systemd_unit() {
    local svc_file="/etc/systemd/system/isola.service"
    local env_file
    env_file="$(env_file_path)"

    # The unit is installer-owned and regenerated on every run so new defaults
    # reach an existing install; a unit that predates the marker is backed up
    # first because it may carry operator changes.
    if [[ -f "$svc_file" ]] && ! grep -q '^# Managed by isola-installer' "$svc_file"; then
        warn "Existing unit ${svc_file} is not installer-managed."
        if [[ $EUID -eq 0 ]]; then
            cp "$svc_file" "${svc_file}.bak"
            info "Backed it up to ${svc_file}.bak before regenerating."
        else
            warn "Re-run as root to regenerate it; leaving the current unit in place."
            return 0
        fi
    fi

    if [[ $EUID -ne 0 ]]; then
        warn "Need root to create systemd service. Run:"
        printf "  sudo tee ${svc_file} <<'UNIT'\n"
        printf '# Managed by isola-installer — regenerated on every install run.\n'
        printf '[Unit]\nDescription=isola\nAfter=network-online.target docker.service\nWants=network-online.target docker.service\n\n'
        printf '[Service]\nType=simple\nUser=isola\nExecStart=%s\nEnvironmentFile=%s\nRestart=always\nRestartSec=5\nLimitNOFILE=65536\n\n' "${INSTALL_DIR}/${BINARY_NAME}" "$env_file"
        printf '[Install]\nWantedBy=multi-user.target\nUNIT\n'
        printf "\n  sudo useradd --system --user-group --home-dir %s --shell /usr/sbin/nologin isola\n" "${INSTALL_DIR}"
        printf "  sudo chown -R isola:isola %s\n" "${INSTALL_DIR}"
        printf "  sudo usermod -aG docker isola   # lets the service reach the Docker socket\n"
        printf "\n  sudo systemctl daemon-reload\n"
        printf "  sudo systemctl enable --now isola\n"
        return 0
    fi

    cat > "$svc_file" <<UNIT
# Managed by isola-installer — regenerated on every install run.
[Unit]
Description=isola
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=simple
User=isola
ExecStart=${INSTALL_DIR}/${BINARY_NAME}
EnvironmentFile=${env_file}
Restart=always
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
UNIT

    # The unit runs as User=isola; create it if this is a fresh host, otherwise
    # systemd fails the start with "user isola not found".
    if ! getent passwd isola >/dev/null 2>&1; then
        useradd --system --user-group --home-dir "${INSTALL_DIR}" --shell /usr/sbin/nologin isola 2>/dev/null \
            || useradd -r -U -d "${INSTALL_DIR}" -s /sbin/nologin isola
    fi
    chown -R isola:isola "${INSTALL_DIR}" 2>/dev/null || true
    # User=isola needs the Docker group to reach /var/run/docker.sock — without
    # it every session launch fails with a permission error even though the
    # daemon is running.
    usermod -aG docker isola 2>/dev/null || true
    systemctl daemon-reload
    systemctl enable --now isola
    # Restart so an upgrade picks up the new binary and env file.
    systemctl restart isola
    success "systemd service installed and started."
}

xml_escape() {
    printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'
}

# Renders the env file's KEY=VALUE lines as plist <key>/<string> pairs. PATH is
# skipped: the plist owns it, and a duplicate key would make the plist invalid.
plist_environment_entries() {
    local env_file line key value
    env_file="$(env_file_path)"
    [[ -f "$env_file" ]] || return 0

    while IFS= read -r line || [[ -n "$line" ]]; do
        case "$line" in
            ''|'#'*) continue ;;
        esac
        key="${line%%=*}"
        value="${line#*=}"
        case "$key" in
            ''|*[!A-Za-z0-9_]*) continue ;;
            PATH) continue ;;
        esac
        printf '        <key>%s</key>\n        <string>%s</string>\n' \
            "$(xml_escape "$key")" "$(xml_escape "$value")"
    done < "$env_file"
}

write_launchagent() {
    local plist_dir="${HOME}/Library/LaunchAgents"
    local plist="${plist_dir}/com.isola.plist"

    mkdir -p "$plist_dir"
    cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.isola</string>
    <key>ProgramArguments</key>
    <array>
        <string>$(xml_escape "${INSTALL_DIR}/${BINARY_NAME}")</string>
    </array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
$(plist_environment_entries)
    </dict>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$(xml_escape "${INSTALL_DIR}/isola.log")</string>
    <key>StandardErrorPath</key>
    <string>$(xml_escape "${INSTALL_DIR}/isola.log")</string>
</dict>
</plist>
PLIST

    plutil -lint "$plist" >/dev/null 2>&1 || warn "plutil reported a problem with ${plist}."
}

reload_launchagent() {
    local plist="${HOME}/Library/LaunchAgents/com.isola.plist"
    local domain="gui/$(id -u)"

    # bootout/bootstrap pick up a rewritten plist and the new binary; the
    # unload/load pair is the fallback for older launchd versions.
    launchctl bootout "${domain}/com.isola" >/dev/null 2>&1 \
        || launchctl unload "$plist" >/dev/null 2>&1 || true

    if launchctl bootstrap "$domain" "$plist" >/dev/null 2>&1; then
        launchctl enable "${domain}/com.isola" >/dev/null 2>&1 || true
        launchctl kickstart -k "${domain}/com.isola" >/dev/null 2>&1 || true
    else
        launchctl load -w "$plist" >/dev/null 2>&1 || true
    fi
}

# ── install Docker when the CLI is missing ──────────────────────────
install_docker() {
    local os="$1"

    if [[ "$os" == "linux" ]]; then
        info "Installing Docker via the official convenience script (requires sudo)..."
        curl -fsSL https://get.docker.com -o /tmp/get-docker.sh || return 1
        if [[ $EUID -eq 0 ]]; then
            sh /tmp/get-docker.sh || { rm -f /tmp/get-docker.sh; return 1; }
        else
            sudo sh /tmp/get-docker.sh || { rm -f /tmp/get-docker.sh; return 1; }
        fi
        rm -f /tmp/get-docker.sh
        if command -v systemctl >/dev/null 2>&1; then
            sudo systemctl enable --now docker || true
        fi
    elif [[ "$os" == "darwin" ]]; then
        if command -v brew >/dev/null 2>&1; then
            info "Installing Docker Desktop via Homebrew..."
            brew install --cask docker || return 1
            info "Launch Docker Desktop once to finish setup: open -a Docker"
        else
            error "Homebrew not found. Install Docker Desktop: https://www.docker.com/products/docker-desktop/"
            return 1
        fi
    else
        error "Automatic Docker install is not supported on ${os}."
        return 1
    fi

    command -v docker >/dev/null 2>&1
}

# ── fetch the published sandbox image ──────────────────────────────
# The sandbox is released per tag alongside the binary and shares its ISOL wire
# header and ABR ladder, so prefer the matching tag and fall back to the
# floating one. Returns 0 only when the image is now present under the local
# name; failures are non-fatal so the caller can fall back to a source build.
browser_image_candidates() {
    # An explicit reference wins; setting it to an empty value keeps the old
    # documented behaviour of skipping the registry and building locally.
    if [[ -n "${ISOLA_BROWSER_IMAGE_REMOTE+set}" ]]; then
        [[ -n "${ISOLA_BROWSER_IMAGE_REMOTE}" ]] || return 0
        printf '%s\n' "${ISOLA_BROWSER_IMAGE_REMOTE}"
        return 0
    fi
    printf '%s\n' "${BROWSER_IMAGE_REPOSITORY}:v${VERSION}"
    printf '%s\n' "${BROWSER_IMAGE_REPOSITORY}:electron"
}

pull_browser_image() {
    local ref

    while IFS= read -r ref; do
        [[ -n "$ref" ]] || continue
        info "Fetching the session image from ${ref}…"
        if docker pull "$ref" >/dev/null 2>&1; then
            # The binary launches the image by its local name, so re-tag the pull.
            docker tag "$ref" "${BROWSER_IMAGE}"
            success "Session image ${BROWSER_IMAGE} → ${ref}"
            return 0
        fi
        warn "Could not fetch ${ref}."
    done < <(browser_image_candidates)

    info "  Private or not published yet? Authenticate, then re-run:"
    info "  docker login ghcr.io"
    return 1
}

# ── ensure a usable Docker CLI + daemon ─────────────────────────────
# Returns 0 only when the daemon answers. The image is handled separately so a
# missing image never blocks installing the binary or writing the service.
ensure_docker() {
    local os="$1"

    # 1. Is the Docker CLI available?
    if ! command -v docker >/dev/null 2>&1; then
        warn "Docker is required but was not found on this host."

        local answer="Y"
        if [[ -r /dev/tty ]]; then
            printf "${CYAN}Install Docker now? [Y/n] ${NC}"
            read -r answer </dev/tty || answer="Y"
        fi
        if [[ "$answer" =~ ^[Nn] ]]; then
            warn "Skipping Docker install — isola cannot start sessions without it."
            info "Install Docker later: https://docs.docker.com/engine/install/"
            return 1
        fi

        if ! install_docker "$os"; then
            error "Docker installation did not complete: https://docs.docker.com/engine/install/"
            return 1
        fi
    fi

    # 2. Is the daemon reachable?
    if ! docker info >/dev/null 2>&1; then
        warn "Docker CLI found but the daemon is not responding."
        info "Start Docker, then re-run this installer:"
        info "  Linux:  sudo systemctl start docker"
        info "  macOS:  open -a Docker"
        info "If this is a permissions error, join the docker group and log in again:"
        info "  sudo usermod -aG docker ${USER}"
        return 1
    fi
    success "Docker detected."
    return 0
}

# ── ensure the per-session browser image is present ─────────────────
ensure_browser_image() {
    if docker image inspect "${BROWSER_IMAGE}" >/dev/null 2>&1; then
        success "Session image ${BROWSER_IMAGE} is ready."
        return 0
    fi

    warn "The per-session image ${BROWSER_IMAGE} is not present."

    # The published image comes first: it is the only path that works without a
    # source checkout. It is non-interactive, so a piped install never blocks.
    if pull_browser_image; then
        return 0
    fi

    if [[ -z "$PROJECT_ROOT" || ! -f "${PROJECT_ROOT}/docker-compose.yml" ]]; then
        info "Build it once from a source checkout:"
        info "  git clone https://github.com/dm0xa/isola-browser.git && cd isola-browser"
        info "  docker compose up --build"
        return 0
    fi

    # No TTY (piped install): proceed without prompting rather than blocking on
    # a read, matching the prompt's [Y/n] default.
    local build_answer=""
    if [[ -r /dev/tty ]]; then
        printf "${CYAN}Build ${BROWSER_IMAGE} now? [Y/n] ${NC}"
        read -r build_answer </dev/tty || build_answer=""
    fi
    if [[ "$build_answer" =~ ^[Nn] ]]; then
        info "Skipping build. Build it later from the source checkout:"
        info "  cd ${PROJECT_ROOT} && docker compose up --build"
        return 0
    fi

    info "Building ${BROWSER_IMAGE} (the first build takes a while)…"
    if ( cd "$PROJECT_ROOT" && docker compose build browser-node-electron ); then
        success "Session image ${BROWSER_IMAGE} built."
    else
        error "Build failed. Try: cd ${PROJECT_ROOT} && docker compose up --build"
    fi
}

# ── ensure the Docker network session containers join ───────────────
# The orchestrator starts every session container with `--network browser-net`
# (that name is hard-coded) and never creates the network itself. The Compose
# stack defines it; a binary-only install must create it here, or every session
# fails to provision with "network browser-net not found".
#
# ICC is disabled so one session container cannot reach another: when the
# backend runs on the host rather than as a container on this bridge, it cannot
# source-restrict the stream/CDP ports to itself, so the bridge is the isolation.
ensure_browser_network() {
    if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
        return 0
    fi

    if docker network inspect browser-net >/dev/null 2>&1; then
        success "Docker network browser-net is ready."
        return 0
    fi

    info "Creating Docker network browser-net (session containers join it)…"
    # The bridge interface name is capped at 15 characters (IFNAMSIZ - 1), so it
    # is abbreviated here; br-isola-browser would be 16 and create would fail.
    if docker network create \
        --driver bridge \
        --opt com.docker.network.bridge.enable_icc=false \
        --opt com.docker.network.bridge.enable_ip_masquerade=true \
        --opt com.docker.network.bridge.name=br-isola-brows \
        browser-net >/dev/null; then
        success "Docker network browser-net created."
    else
        error "Could not create browser-net — sessions will not start until it exists."
        info "  docker network create browser-net"
    fi
}

# ── main ────────────────────────────────────────────────────────────
main() {
    printf "\n${CYAN}╭──────────────────────────────────────╮${NC}\n"
    printf "${CYAN}│    isola Installer                   │${NC}\n"
    printf "${CYAN}╰──────────────────────────────────────╯${NC}\n\n"

    # 1. detect platform and resolve the install location
    local platform
    platform="$(detect_platform)"
    local os="${platform%/*}"
    local arch="${platform#*/}"
    info "Platform: ${os}/${arch}"

    resolve_install_dir
    info "Install directory: ${INSTALL_DIR}"

    # 2. Docker first: the MAC decision and the session image both need the
    # daemon, and a missing daemon must not leave a crash-looping service.
    local docker_ready=0
    if ensure_docker "$os"; then
        docker_ready=1
    fi

    # 3. stop an existing service/process before replacing the binary
    stop_isola_if_running "$os"

    # 4. build download URL
    local archive="${BINARY_NAME}-${os}-${arch}-v${VERSION}.tar.gz"
    local download_url="${DOWNLOAD_BASE}/${archive}"
    local checksum_url="${DOWNLOAD_BASE}/checksums.txt"

    info "Downloading isola ${VERSION} for ${os}/${arch}..."
    info "URL: ${download_url}"

    # 5. create install dir
    mkdir -p "${INSTALL_DIR}"

    # 6. download
    local tmp_file
    tmp_file="$(mktemp)"
    curl -fsSL --retry 3 --retry-delay 2 -# -o "$tmp_file" "$download_url" || {
        error "Download failed from ${download_url}"
        rm -f "$tmp_file"
        exit 1
    }

    # 7. verify checksum (mandatory — never install an unverified binary)
    local expected
    expected="$(curl -fsSL "$checksum_url" 2>/dev/null | awk -v f="$archive" '$2==f || $2=="*"f {print $1; exit}' || true)"
    if [[ -z "$expected" ]]; then
        error "Could not fetch a checksum for ${archive} from ${checksum_url}; refusing to install."
        rm -f "$tmp_file"
        exit 1
    fi
    local actual
    actual="$(sha256sum "$tmp_file" 2>/dev/null | cut -d' ' -f1 || shasum -a 256 "$tmp_file" 2>/dev/null | cut -d' ' -f1)"
    if [[ -z "$actual" || "$expected" != "$actual" ]]; then
        error "Checksum verification failed (expected ${expected}, got ${actual:-none})."
        rm -f "$tmp_file"
        exit 1
    fi
    success "Checksum verified"

    # 8. extract (tar.gz or zip or raw binary)
    if file "$tmp_file" | grep -q 'gzip compressed'; then
        info "Extracting..."
        tar xzf "$tmp_file" -C "${INSTALL_DIR}" "$BINARY_NAME" 2>/dev/null || {
            # binary might be at root of archive without prefix
            tar xzf "$tmp_file" -C "${INSTALL_DIR}" --transform 's|.*/||' --wildcards "*${BINARY_NAME}*" 2>/dev/null
        }
    elif file "$tmp_file" | grep -q 'Zip archive'; then
        info "Extracting..."
        unzip -o "$tmp_file" -d "${INSTALL_DIR}" >/dev/null 2>&1
    elif file "$tmp_file" | grep -q 'ELF\|Mach-O'; then
        # raw binary
        cp "$tmp_file" "${INSTALL_DIR}/${BINARY_NAME}"
    else
        error "Unknown archive format."
        rm -f "$tmp_file"
        exit 1
    fi

    chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
    rm -f "$tmp_file"

    success "isola installed to ${INSTALL_DIR}/${BINARY_NAME}"

    # 9. environment file + MAC confinement report
    write_env_file
    report_lsm_state "$docker_ready"

    # 10. optionally set up as a system service
    if [[ $docker_ready -eq 1 ]] || service_file_exists "$os"; then
        local setup_svc="Y"
        if [[ -r /dev/tty ]]; then
            printf "\n${CYAN}Set up isola as a system service (auto-start on boot)? [Y/n] ${NC}\n"
            read -r setup_svc </dev/tty || setup_svc="Y"
        fi
        if [[ ! "$setup_svc" =~ ^[Nn] ]]; then
            setup_service "$os"
        else
            info "No service created. To run manually:"
            info "  ${INSTALL_DIR}/${BINARY_NAME}"
            if [[ "$os" == "linux" && "$EUID" -ne 0 ]]; then
                info "The invoking user needs Docker socket access:"
                info "  sudo usermod -aG docker ${USER}   # then log out and back in"
            fi
        fi
    else
        warn "Docker is not ready, so no service was written yet."
        info "Start Docker, then re-run this installer:"
        info "  Linux:  sudo systemctl start docker"
        info "  macOS:  open -a Docker"
        info "The binary itself is installed: ${INSTALL_DIR}/${BINARY_NAME}"
    fi

    # 11. session image + the network session containers join
    if [[ $docker_ready -eq 1 ]]; then
        ensure_browser_image
        ensure_browser_network
    fi

    # 12. next steps
    printf "\n${GREEN}┌─────────────────────────────────────────────┐${NC}\n"
    printf "${GREEN}│  installation complete!                     │${NC}\n"
    printf "${GREEN}│                                             │${NC}\n"
    printf "${GREEN}│  Start isola:                               │${NC}\n"
    printf "${GREEN}│    ${INSTALL_DIR}/${BINARY_NAME}             ${NC}\n"
    printf "${GREEN}└─────────────────────────────────────────────┘${NC}\n\n"
    info "Transports: isola (Electron) only"
}

main "$@"
