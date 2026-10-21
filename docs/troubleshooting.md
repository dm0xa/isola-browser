# Troubleshooting

First stop for anything: the logs and the health endpoint.

```bash
docker compose logs -f          # Isola process + build services
curl -s localhost:8080/health   # liveness
```

## Reading the Logs

Isola logs at three layers; which one you want depends on what broke. Each layer also has a live status endpoint — often more useful than the log itself.

### Session container — `isola-streamer`, entrypoint, Electron

The streamer logs to stderr, which Docker captures as the container's log. There's no separate log file.

```bash
docker ps --filter label=isola.session_id      # live session containers
docker logs -f <container-id>                  # streamer + [browser-node-electron] + Chromium
docker ps -a --filter label=isola.warm=1       # warm-pool containers (no session id)
```

Expect lines such as `isola-streamer listening on :5900`, `cdp: connected to Chromium`, `capture: browser auto-attach enabled`.

The streamer also serves status endpoints on port `5900`, published to host loopback — handy when the log goes quiet:

```bash
docker exec <container-id> curl -s localhost:5900/debug/encoder   # encoder/input/damage JSON
docker exec <container-id> curl -s localhost:5900/healthz         # "ok"
```

### Host process — API, orchestrator, WS relay

`zerolog` writes to stderr; where you read it depends on how Isola was started:

| Started by | Read the logs with |
| :--- | :--- |
| Docker Compose | `docker compose logs -f isola` |
| systemd (installer) | `journalctl -u isola -f` |
| LaunchAgent, macOS (installer) | `tail -f "<install dir>/isola.log"` (default `/opt/isola/isola.log`, or `~/.isola/isola.log` when `/opt/isola` is not writable) |
| `scripts/dev-rebuild.sh` | `tail -f /tmp/isola-backend.log` |

`LOG_LEVEL=debug` raises verbosity above the default `info` (`debug`, `info`, `warn`, `error`) — see [Configuration](configuration.md#logging).

### Viewer

Toggle the debug overlay in the viewer and use its **copy** icon, or from the browser console:

```js
copy(JSON.stringify(window.__isolaViewerDebug, null, 2))
```

The in-viewer **Console** panel shows the *remote page's* console, not Isola's.

## The UI Loads but Sessions Never Start

- Confirm Docker is reachable from the container: the stack needs `/var/run/docker.sock` mounted. A missing or permission-denied socket is the most common cause.
- Check that `isola-browser:electron` built: `docker images | grep isola-browser`. If `browser-node-electron` failed, sessions cannot be provisioned.
- Look for `provisioning` stuck in the logs — the container may be failing to boot. `docker logs <session-container>` shows why.

## Sessions Fail to Provision (Docker Errors)

The orchestrator logs the `docker` error verbatim. These have a specific cause:

- `range of CPUs is from 0.01 to N.00, as there are only N CPUs available` — `ISOLA_SESSION_CPUS` is above the host's core count. The daemon refuses `--cpus` above the machine's cores and the request aborts with exit `125`, so **every** session fails. Lower it to the core count, or unset it (isola then uses `min(3, cores)`). See [Deployment → Sizing](deployment.md#sizing).
- `failed to set up container networking: network browser-net not found` — the network session containers join does not exist. The Compose stack defines it; a binary install must create it once: `docker network create browser-net`. See [Deployment → Run Without Compose](deployment.md#run-without-compose).
- `mkdir /var/lib/isola: permission denied` (logged as `snapshot: data dir unavailable, persistence disabled`) — the process cannot write the snapshot directory. Harmless for a single run, but sessions are lost on restart. Point `ISOLA_DATA_DIR` at a path the running user owns. See [Configuration](configuration.md#environment-variables).
- `could not resolve backend browser-net IP; session service ports stay open to the bridge` — expected when the backend runs **on the host** rather than as a container on `browser-net`: there is no container to inspect for that address, so the stream/CDP ports cannot be restricted to the proxy. Disable ICC on the bridge instead (see [Run Without Compose](deployment.md#run-without-compose)).

### A session container exits immediately

A session that provisions but never reaches `running` — the log ends at `stream not ready … after 30s` — means the container died at startup. It is created with `--rm`, so it is gone before you can inspect it; either re-run the image by hand or set `ISOLA_DATA_DIR` and read the orchestrator log for the exit code.

```bash
# Re-run the image with the same entrypoint and read why it exited
docker run --rm --cap-add NET_ADMIN --network browser-net \
  -e RESOLUTION=1280x720 isola-browser:electron
```

The entrypoint fails closed with `exit 2` and a `FATAL:` line naming the cause:

- `FATAL: cannot determine default gateway for session service ports` — the image has no `ip` binary to read the default route (`iproute2` is missing). Current images include it; a locally built image predating that needs a rebuild.
- `FATAL: no enforcing MAC policy confines this container` — the host runs no AppArmor/SELinux profile. See [Deployment → Host Requirements](deployment.md#host-requirements). Hosts without an LSM (Docker Desktop's Linux VM, for example) hit this by design: enable an LSM, or opt out with `ISOLA_REQUIRE_MAC=0` — in `isola.env` for a service install, exported in the shell for a hand-run binary, or in front of `docker compose up`.

Expected container-log noise that is **not** fatal: `Fontconfig error: No writable cache directories` and `Failed to create /home/isolauser/.cache for shader cache`, from the read-only rootfs plus tmpfs home.

## The Stream Is Blank or Frozen

- Isola automatically detects and recovers from the two most common frozen-frame causes — a blocked renderer and empty captures. A manual **recover** action in the toolbar handles a page-level stall. How the stream is framed: [Transport](transport.md).
- A page that pins the CPU (heavy WebGL, a runaway script) can exceed the container's 3 CPU limit. That's a resource limit, not a bug.
- If the picture is frozen but frames keep flowing, the page stopped changing — it's the page, not the transport.

## Sessions End Sooner Than Expected

- `session.default_min` (default 10) sets the starting length and `session.max_min` (default 30) the ceiling.
- `session.inactivity_min` (default 5) terminates an idle session.
- With `session.timer=false`, the sandbox closes as soon as the owner leaves — there's no countdown or extend. This is expected.

## High Resolutions Fail or the Container Is Killed

Requests above `session.max_resolution` are scaled down before the container starts. Captures far larger than the default can exhaust the 2 GB memory limit. Lower `max_resolution`, or raise the container memory limit only if the host has headroom (see [Sizing](deployment.md#sizing)).

## Names Don't Resolve Inside a Session

- In `plain` mode, `dns.servers` entries must be **IP addresses** — hostnames are rejected at startup.
- In `tls`/`https` mode, at least one entry is required, and IP-based endpoints need a certificate with an IP SAN (Cloudflare and Google have one); use a hostname otherwise.
- If the backend cannot determine the browser-network address, it falls back to plaintext resolvers and logs a warning. Check the logs for it.

## Behind a Reverse Proxy

- The WebSocket fails to connect: the proxy must pass `Upgrade`/`Connection` headers and keep long timeouts. See [Deployment](deployment.md#put-it-behind-tls).
- CORS errors in the browser console: set `CORS_ORIGIN` to your public origin.
- Rate limiting blocks legitimate users: set `TRUSTED_PROXY_CIDRS` so Isola sees real client IPs, not the proxy's.

## GPU Encoding Isn't Used

- The host needs `/dev/dri/renderD128`, and both `ISOLA_GPU=1` and the `/dev/dri` mount must be present. Without them Isola uses software encoding; this is expected, not an error.

## `POST /api/sessions` Returns 401

- `ISOLA_API_TOKEN` (or `api.token`) is set and the request has no `Authorization: Bearer <token>` header, or the bearer does not match.
- **Web UI** — confirm `GET /api/config` shows `create_auth_required: true`, then set `localStorage.setItem('isola_api_token', '<token>')` on the Isola origin (same value as in the container env).
- **curl/scripts** — pass `-H "Authorization: Bearer $ISOLA_API_TOKEN"`.

## MCP Returns 401 or `/mcp` Is Missing

- `401` — the `Authorization: Bearer <token>` header is missing or wrong; it must match `mcp.token` / `ISOLA_MCP_TOKEN` in the running `isola` container.
- Backend won't start — `ISOLA_MCP_ENABLED=true` but `ISOLA_MCP_TOKEN` is missing or shorter than 32 characters. Generate with `openssl rand -hex 32`.
- Endpoint not mounted — rare legacy case: `mcp.enabled` true with empty token at runtime logs an error and skips `/mcp`; set a valid token and restart. Confirm with `docker logs isola 2>&1 | grep MCP` (should log that `/mcp` is mounted).
- Cursor shows no tools — reload MCP after deploy; on macOS, export `ISOLA_MCP_TOKEN` before launching Cursor (the Dock app does not load `.env`).
- `navigate` or `create_session` fails right after start — usually the control socket is not ready yet; the server retries 503s automatically. If it still fails, read the session container logs (`docker compose logs browser-node-electron` is the image build service; live session containers are found by their `isola.session_id` label — see [Reading the logs](#reading-the-logs)).
- Empty `snapshot` then `click` fails — call `snapshot` again after `document_seen` is true, or use `inspect_session` first.

## A Share Link Can't Click or Type

Share tokens are view-only by design. Use the session token for control.

## Resetting Everything

```bash
docker compose down          # stop
docker compose down -v       # stop and remove the isola-data volume
just clean                   # also prunes unused Docker resources
```
