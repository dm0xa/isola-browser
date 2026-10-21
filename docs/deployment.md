# Deployment

This page covers running Isola on a server you control rather than on your laptop. The default Compose stack from [Getting started](getting-started.md) is the basis; production adds TLS, sizing, and a few settings.

## Run Detached

```bash
docker compose up --build -d
docker compose logs -f
```

The stack is one process (`isola`) that serves the UI, API, and WebSocket relay, plus a short-lived build service (`browser-node-electron`) that builds `isola-browser:electron` and exits. It needs access to `/var/run/docker.sock` to start session containers.

## Run Without Compose

[`scripts/install.sh`](../scripts/install.sh) — and a hand-built binary — run the control plane directly on the host instead of in a container. The sandbox then needs these prerequisites alongside it:

- **The `docker` CLI on `PATH`.** Isola drives Docker by shelling out to `docker run/inspect/rm/ps`, so the CLI must be installed and reachable by the user running isola — the daemon alone is not enough. A missing CLI shows up in the log as `orphan sweep: docker ps failed error="exec: \"docker\": executable file not found in $PATH"`.
- **Docker socket access.** The user running isola must reach `/var/run/docker.sock`: `sudo usermod -aG docker "$USER"`, then log out and back in. The installer's systemd unit runs as `User=isola` and adds that user to the `docker` group for you.
- **The `browser-net` network.** Session containers are started with `--network browser-net` (the name is hard-coded) and isola never creates it. The Compose stack defines it; a binary install creates it once:

  ```bash
  docker network create --driver bridge \
    --opt com.docker.network.bridge.enable_icc=false \
    --opt com.docker.network.bridge.enable_ip_masquerade=true \
    browser-net
  ```

  ICC is disabled so one session container cannot reach another: with the backend on the host it cannot source-restrict the stream/CDP ports to itself, so the bridge is what isolates concurrent sessions from each other (see the `could not resolve backend browser-net IP` warning in [Troubleshooting](troubleshooting.md)). If `docker network create` fails with `numerical result out of range`, a configured bridge name is longer than Linux's 15-character interface-name limit — drop the option or shorten the name.
- **The session image.** Releases publish the sandbox alongside the binary, so the installer pulls `ghcr.io/dm0xa/isola-browser:v<version>` — the tag matching the installed binary, which matters because the two share the ISOL wire header and ABR ladder — falls back to `ghcr.io/dm0xa/isola-browser:electron`, and re-tags the result as `isola-browser:electron`. When both pulls fail (private package, no network) build it for this architecture instead: `sudo docker build -t isola-browser:electron -f ./browsers/browser-node-electron/Dockerfile ./browsers` (or `just build-browser-node-electron`). An arm64 build will not run on an x86-64 server, so build on the target host or transfer an image that matches it. Set `ISOLA_BROWSER_IMAGE_REMOTE` to pin one exact reference; an empty value skips the registry (the installer then builds from a checkout when one is available). To launch a differently named image altogether, set `ISOLA_BROWSER_IMAGE`.
- **`ISOLA_DATA_DIR` writable by the running user**, or the session snapshot is skipped.

Running as a service, the installer records the environment in `<install dir>/isola.env`, where the default install dir is `/opt/isola` (or `~/.isola` when the invoking user cannot write `/opt/isola`). The systemd unit reads it through `EnvironmentFile`; the macOS LaunchAgent has it rendered into `EnvironmentVariables`, so re-run the installer after editing it. Set `ISOLA_DATA_DIR`, `ISOLA_SESSION_*` and anything else there.

Moving to Compose later: the stack declares its own `browser-net`, so if the installer created one on this host, remove it first (`docker network rm browser-net`) and let the stack recreate it.

Isola listens on **all interfaces** on its port, so on a host with a public IP either put it behind the reverse proxy below or restrict the port in the host/cloud firewall, and gate session creation with `ISOLA_API_TOKEN`.

### Manual install

The installer is the easy path, but every step it performs is plain `curl`, `tar` and `docker` — useful when you would rather not pipe a script to a shell (hardened servers, CI, immutable or non-root installs). Choose the archive that matches `uname -sm`:

| `uname -sm` | Archive |
| :--- | :--- |
| `Linux x86_64` | `isola-linux-amd64` |
| `Linux aarch64` | `isola-linux-arm64` |
| `Darwin arm64` | `isola-darwin-arm64` |
| `Darwin x86_64` | `isola-darwin-amd64` |

Pin an exact release tag. Do **not** use the `/releases/latest/download` alias with a hard-coded filename: the asset name carries the version, so a stale name simply 404s. `VERSION` below is an example — substitute a real tag from the [releases page](https://github.com/dm0xa/isola-browser/releases).

```bash
VERSION=1.0.0                              # a real release tag, without the leading v
GOOS=linux GOARCH=amd64                    # from `uname -sm` above
BASE="https://github.com/dm0xa/isola-browser/releases/download/v${VERSION}"
ARCHIVE="isola-${GOOS}-${GOARCH}-v${VERSION}.tar.gz"

curl -fsSLO "${BASE}/${ARCHIVE}"
curl -fsSLO "${BASE}/checksums.txt"
# Verify before running the binary — checksums.txt covers every archive, so
# check only the one you downloaded. macOS: swap sha256sum for `shasum -a 256`.
grep "${ARCHIVE}" checksums.txt | sha256sum -c -

tar xzf "${ARCHIVE}"                      # extracts ./isola
./isola                                    # UI + API + relay on :8080
```

The binary ships without the sandbox image (they are published as separate artifacts of the same release). Fetch the per-session image for the tag you installed and re-tag it to the name the binary launches, `isola-browser:electron` — the same step the installer performs:

```bash
docker pull ghcr.io/dm0xa/isola-browser:v${VERSION}
docker tag  ghcr.io/dm0xa/isola-browser:v${VERSION} isola-browser:electron
```

`v${VERSION}` matches the binary you installed; that is the point of the per-tag images, since the streamer and the frontend share the ISOL wire header and ABR ladder. Releases that predate the per-tag images only have `:electron`.

If the package is private, authenticate first (`docker login ghcr.io`). If you have a source checkout you can build it locally instead (`docker compose build browser-node-electron`), but arm64 and x86-64 images are not interchangeable — build on the target host or on a matching architecture. To launch a differently named image altogether, set `ISOLA_BROWSER_IMAGE`.

Finally create the `browser-net` network (command and rationale above) and start `./isola`. Everything else in this section applies as-is: the `docker` CLI on `PATH`, Docker socket access, and a writable `ISOLA_DATA_DIR`. To run it as a service rather than in the foreground, write the unit yourself — `scripts/install.sh` generates one for reference.

## Host Requirements

Keep an enforcing MAC policy active on the host. Session containers rely on AppArmor's `docker-default` profile (or SELinux's container policy) as a defence-in-depth layer, and the entrypoint **fails closed** if neither is enforcing — `docker info` reports the active profile under `Security Options`, and sessions log `MAC confinement active: …` when the check passes. `ISOLA_REQUIRE_MAC=0` overrides the check for hosts that deliberately run without an LSM (not recommended). The installer reports the detected state but never sets the override; Docker Desktop's Linux VM has neither LSM, for example.

## Put It Behind TLS

Isola speaks plain HTTP on `8080`. Terminate TLS in front of it with a reverse proxy. Bind the container to loopback so only the proxy reaches it, and tell Isola the public origin:

```yaml
# docker-compose.yml (services.isola)
ports:
  - "127.0.0.1:8080:8080"
environment:
  - CORS_ORIGIN=https://isola.example.com
  - TRUSTED_PROXY_CIDRS=172.16.0.0/12
```

`TRUSTED_PROXY_CIDRS` makes per-IP rate limiting use the real client address from `X-Forwarded-For` instead of the proxy's address.

The proxy must forward both the API and the WebSocket, with upgrade headers:

```nginx
location / {
    proxy_pass http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_read_timeout 3600s;
    proxy_send_timeout 3600s;
}
```

The repository ships a fuller nginx example, including a commented HTTPS + HSTS server block, in [`nginx/nginx.conf`](../nginx/nginx.conf).

## Sizing

Each session container is capped at **2 GB RAM, 3 CPUs, and 512 PIDs**. Size the host for peak concurrency:

- **Cores** — the CPU cap can never exceed the host's core count; the Docker daemon refuses `--cpus` above it. The default is 3, so a host with fewer than 3 cores gets its own core count automatically, and an explicit `ISOLA_SESSION_CPUS` above it fails every session with a Docker exit `125`.
- **~2 GB per concurrent session** — plus memory for the OS, Docker, and the Isola process itself.
- A **warm pool** of size *N* keeps *N* idle containers ready, each costing a full session's memory while it waits. A claimed container is no longer idle, so peak concurrency plus *N* idle containers is the memory to budget.
- **Disk** — used by the images and by `/dev/shm` allocations; the browser images are the bulk of it.

## GPU Acceleration

If the host has an Intel render device (`/dev/dri/renderD128`), you can enable hardware-accelerated encoding:

```yaml
# docker-compose.yml (services.isola)
environment:
  - ISOLA_GPU=1
volumes:
  - /dev/dri:/dev/dri
```

The dev stack in [`deploy/docker-compose.yml`](../deploy/docker-compose.yml) already mounts `/dev/dri`. When no GPU is available, Isola falls back to software encoding.

## Persistence and Volumes

| Volume | Purpose |
| :--- | :--- |
| `/var/run/docker.sock` | **Required.** Lets Isola launch session containers. |
| `./backend/config.yaml` → `/config.yaml` | Configuration (read-only). Restart to apply changes. |
| `browsers/isola-base/seccomp.json` → `/etc/isola/seccomp.json` | Seccomp profile for session containers. |
| `isola-data` → `/var/lib/isola` | Persistent application data: the session snapshot (`sessions.json`) and per-session policy files. Set `ISOLA_DATA_DIR` to relocate it. |

Session *content* (cookies, history, files inside the browser) is never persisted — it lives only inside the container and is discarded on teardown.

## Updating

```bash
git pull
docker compose up --build -d
```

Rebuilding replaces the images; the `isola-data` volume and your `.env` are preserved. In-flight sessions end when their containers are replaced.

## Production Checklist

- [ ] TLS terminated in front of Isola; container bound to `127.0.0.1`.
- [ ] `CORS_ORIGIN` set to the public origin.
- [ ] `TRUSTED_PROXY_CIDRS` set to your proxy's network.
- [ ] `DISABLE_URL_VALIDATION` **false** (the default).
- [ ] `ISOLA_API_TOKEN` set (≥ 32 chars) so anonymous clients cannot spawn sessions; operators store the same value in `localStorage.isola_api_token` for the web UI (see [Configuration](configuration.md#require-a-token-to-create-sessions)).
- [ ] `RATE_LIMITING_ENABLED=true` to cap mutating API abuse per IP.
- [ ] MCP enabled only if needed, with a strong `ISOLA_MCP_TOKEN` (≥ 32 chars); keep MCP and API tokens distinct unless you have a reason to share one secret.
- [ ] Host sized for peak concurrency (and the warm pool, if enabled).
- [ ] Docker and the host kernel kept patched — see [Security](security.md).
