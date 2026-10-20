# Configuration

Isola reads configuration from a YAML file and from environment variables. Environment variables override the file; the file overrides built-in defaults.

- **Compose** — the stack bind-mounts [`backend/config.yaml`](../backend/config.yaml) into the container at `/config.yaml`. Edit it and restart the stack to apply changes.
- **Environment variables** — every setting can also be supplied as an environment variable named after that setting, optionally prefixed with `ISOLA_`. The resolver checks `ISOLA_<NAME>` first, then the unprefixed `<NAME>`. For example, `DNS_MODE` and `ISOLA_DNS_MODE` both work.
- **`.env`** — [`docker-compose.yml`](../docker-compose.yml) maps values from `.env` onto container environment variables. Copy [`.env.example`](../.env.example) to `.env` to override the defaults.

## `config.yaml` Reference

### `server` / `docker`

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `server.port` | `"8080"` | `PORT` | HTTP port the binary listens on inside the container. |
| `docker.host` | `unix:///var/run/docker.sock` | `DOCKER_HOST` | Docker endpoint used to launch session containers. |

### `dns`

How session containers resolve names. See [Encrypted DNS](#encrypted-dns) for the DoT/DoH options.

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `dns.mode` | `plain` | `DNS_MODE` | `plain`, `tls` (DNS-over-TLS), or `https` (DNS-over-HTTPS). |
| `dns.servers` | `1.1.1.1`, `8.8.8.8`, `9.9.9.9` | `DNS_SERVERS` (comma-separated) | Resolvers. In `plain` mode these must be IP addresses. |

### `tor`

Optional Tor exit-node pinning for sessions created with `tor_enabled` (see the [API](api.md#create-a-session) and [Automation](automation.md#tools)). A per-session `tor_country` overrides this default.

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `tor.exit_country` | *(empty)* | `TOR_EXIT_COUNTRY` (comma-separated) | ISO 3166-1 alpha-2 country code(s) to pin Tor exits to (e.g. `se` or `se,no`). Empty = any exit. |

Pinning is strict (`StrictNodes`): if no reachable exit exists in the chosen country, the session fails to start rather than egressing from another country, and a country with few exits can also slow the bootstrap. Restricting exits shrinks the anonymity set, so treat this as a routing control, not a privacy upgrade.

### `cors`

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `cors.origin` | `http://localhost:5173` | `CORS_ORIGIN` | Allowed browser origin(s). Set this to your public origin when you serve Isola through a reverse proxy on a different host. Several origins may be given, separated by commas or spaces. |

### `session`

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `session.default_min` | `10` | `SESSION_DEFAULT_MIN` | Starting session length in minutes (`0` = unlimited). |
| `session.max_min` | `30` | `SESSION_MAX_MIN` | Maximum length a session can be extended to. |
| `session.extend_min` | `5` | `SESSION_EXTEND_MIN` | Minutes added by each extend. |
| `session.inactivity_min` | `5` | `INACTIVITY_MIN` | Idle minutes before auto-termination. A viewer attached to the session counts as activity even while the page is static, so this reclaims sandboxes nobody is watching rather than ones merely sitting still. |
| `session.timer` | `true` | `SESSION_TIMER` | When `false`, no countdown and no extend — the sandbox closes when the owner leaves. |
| `session.resolution` | `"1920x1080"` | `RESOLUTION` | Default capture resolution. |
| `session.max_resolution` | `"1920x1080"` | `MAX_RESOLUTION` | Hard cap applied to any requested resolution (aspect ratio preserved); larger requests are scaled down before the container starts. |
| `session.network_log_max_entries` | `1000` | `NETWORK_LOG_MAX_ENTRIES`, `ISOLA_NETWORK_LOG_MAX_ENTRIES` | Number of network requests one session's capture window keeps. Older requests roll off, and the viewer, the in-viewer panel and MCP `get_network` all see the same window. Metadata only. Values ≤ `0` fall back to the default; values above `10000` are capped. |

> The Compose `.env` sets `RESOLUTION=1280x720`, overriding the file default of `1920x1080`.

### `warm_pool`

Pre-started containers, so a session is live the moment a user hits enter rather than after a container boot. Each warm container costs a full session's memory (the per-container limit — default 2 GB, see `ISOLA_SESSION_MEMORY_MB`), so size the pool to expected concurrency.

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `warm_pool.enabled` | `false` | `WARM_POOL_ENABLED` | Keep containers pre-warmed. |
| `warm_pool.size` | `1` | `WARM_POOL_SIZE` | Number of containers to keep warm (minimum 1). |

The pool serves a claim only when the request matches what it was built with: Isola transport, the configured resolution, default theme and frame rate, no Tor/bandwidth override. Anything else transparently falls back to a cold start. The session timer starts at entry either way.

### `browser`

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `browser.theme` | `auto` | `BROWSER_THEME` | `dark`, `light`, or `auto`. |
| `browser.image` | `isola-browser:electron` | `BROWSER_IMAGE`, `ISOLA_BROWSER_IMAGE` | Docker image used for session containers. Defaults to the image the Compose stack builds; point it at a published/prebuilt image to run without building locally. |

> **Note:** `browser.type` / `browser.chromium` exist so Chromium flags can be passed into the Electron image. Isola has a single browser image (`isola-browser:electron`) and a single transport.

### `settings`

Controls what the landing page offers.

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `settings.resolution_visible` | `true` | — | Show the resolution picker. |
| `settings.resolutions` | `1080p`, `720p` | — | Resolutions offered in the picker. |
| `settings.browser_visible` | `false` | — | Show the browser picker (hidden; a single Electron image is used). |
| `settings.transport_visible` | `false` | — | Show the transport picker (single transport). |
| `settings.transports` | `isola` | — | Available transports. |
| `settings.default_transport` | `isola` | `DEFAULT_TRANSPORT` | Transport used when none is requested. |
| `settings.isola_tabs` | `true` | — | Show the tab strip and address bar in the viewer. |

### `api`

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `api.token` | none | `API_TOKEN`, `ISOLA_API_TOKEN` | One or more Bearer secrets for `POST /api/sessions`. YAML: a string, comma-separated string, or a list. Env: comma-separated. Each token ≥ 32 chars. Empty = anonymous create. |

`GET /api/config` includes `create_auth_required: true` when any token is configured, so the UI knows to attach a bearer from `localStorage.isola_api_token` (or a signed-in `access_token`). Any configured token is accepted. MCP `create_session` calls the handler in-process and is not blocked by this gate.

### `mcp`

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `mcp.enabled` | `false` | `MCP_ENABLED`, `ISOLA_MCP_ENABLED` | Mount the MCP endpoint at `POST /mcp`. |
| `mcp.token` | none | `MCP_TOKEN`, `ISOLA_MCP_TOKEN` | One or more Bearer secrets for every MCP request. Same YAML/env shapes as `api.token`. Each ≥ 32 chars when MCP is enabled. |

If `mcp.enabled` is true but no token is set, or any token is shorter than 32 characters, the process **fails at startup**. Generate tokens with `openssl rand -hex 32`. See [Automation → MCP](automation.md#mcp-model-context-protocol).

### `logging`

| Key | Default | Env | Meaning |
| :--- | :--- | :--- | :--- |
| `logging.level` | `info` | `LOG_LEVEL` | `debug`, `info`, `warn`, or `error`. |

## Environment Variables

In addition to the per-setting variables above:

| Variable | Scope | Meaning |
| :--- | :--- | :--- |
| `ISOLA_PORT` | Compose / `.env` | Host port the UI and API are published on (default `8080`). Read by Compose, not by the binary. |
| `DISABLE_URL_VALIDATION` | Container | When `true`, skips the URL safety checks on session creation. **Do not enable in a shared or public deployment.** |
| `ISOLA_SESSION_MEMORY_MB` | Container | Per-session container RAM limit in MB (default `2048`, i.e. 2 GB). |
| `ISOLA_SESSION_CPUS` | Container | Per-session container CPU limit (default `3`; fractions allowed, e.g. `1.5`). |
| `ISOLA_SESSION_PIDS` | Container | Per-session container PID limit (default `512`). |
| `ISOLA_DATA_DIR` | Container | Directory for the session snapshot (`sessions.json`) and per-session policy files. Default `/var/lib/isola`; it must be writable by the running user or persistence is silently skipped with `data dir unavailable, persistence disabled`. |
| `ISOLA_BROWSER_IMAGE` | Container | Docker image used for session containers (default `isola-browser:electron`, the image the Compose stack builds). Point it at a published/prebuilt image to run without a local build. |
| `ISOLA_MAX_SESSIONS` | Container | Cap on concurrently active sessions (provisioning + running) across the whole instance. Unset/`0` = unlimited. Launches beyond the cap are refused with HTTP `429` (`max_sessions_reached`). |
| `ISOLA_GPU` | Container | Set to `1` to pass `/dev/dri` into session containers for hardware-accelerated encoding. |
| `ISOLA_SECCOMP_PROFILE` | Container | Path to a seccomp profile for session containers. |
| `ISOLA_REQUIRE_MAC` | Container | Require an enforcing MAC policy (AppArmor or SELinux) to confine session containers; the entrypoint refuses to start without one. Default `1`; set `0` to allow an unconfined host (**not recommended**). |
| `RATE_LIMITING_ENABLED` | Container | When `true`, limits mutating REST calls to 120/min per client IP (GET/HEAD exempt). Recommended on internet-facing hosts. |
| `DATABASE_URL`, `REDIS_URL`, `JWT_SECRET`, `AUTH_ENABLED` | Container | Multi-user store and JWT login; not needed for the default Compose stack. `AUTH_ENABLED=true` requires `JWT_SECRET` of at least 32 characters. |

## Common Recipes

### Change the public port

```bash
# .env
ISOLA_PORT=9000
```

### Longer sessions

```bash
# .env
SESSION_DEFAULT_MIN=30
SESSION_MAX_MIN=120
INACTIVITY_MIN=15
```

### Bigger (or smaller) session containers

Override the per-session `docker run` limits (defaults: **2 GB RAM, 3 CPUs, 512 PIDs**). This only raises the *cap* — it lets a session use more before the OOM killer; the host still needs the headroom (see [Getting Started → Requirements](getting-started.md#requirements)). Values must be positive; anything else is ignored and the default kept. The CPU cap cannot exceed the host's core count — the Docker daemon refuses `--cpus` above it — so leave `ISOLA_SESSION_CPUS` unset on a host with fewer than 3 cores (isola then uses the core count); a value above it makes every session fail with a Docker exit `125`.

```bash
# .env
ISOLA_SESSION_MEMORY_MB=4096   # 2g -> 4g
ISOLA_SESSION_CPUS=4
ISOLA_SESSION_PIDS=1024
```

Applies to new sessions — rebuild the backend image so the new limits take effect:

```bash
docker compose up -d --build isola
```

### Cap concurrent sessions

The default is **multisession**: every launch adds a session and none of the caller's other sessions is torn down, so an open `POST /api/sessions` can be pointed at a host until it runs out of memory. `ISOLA_MAX_SESSIONS` is the admission gate — set it to the number of sessions the host can actually hold (see the all-in-one sizing table in [Getting Started → Requirements](getting-started.md#requirements)). A launch past the cap gets HTTP `429` with code `max_sessions_reached`; the UI shows *Maximum concurrent sessions reached*.

```bash
# .env — allow up to 5 concurrent sessions
ISOLA_MAX_SESSIONS=5
```

Sessions already active keep running; the cap only gates new launches. Combine with `ISOLA_API_TOKEN` and `RATE_LIMITING_ENABLED` on internet-facing hosts.

The UI does not dead-end on the cap: a `429` makes it wait and retry with backoff for a few attempts (honouring the `Retry-After` header the backend sends) while showing *waiting for a free session*, then reports a clear message. The cap is also published as `max_sessions` in `GET /api/config` (`0` = unlimited).

### Encrypted DNS

Set `dns.mode` to `tls` or `https`. In those modes each `servers` entry is a DoT/DoH endpoint — a bare IP or hostname is expanded, and a full `tls://` or `https://` URL is used verbatim.

```yaml
dns:
  mode: "https"
  servers:
    - "1.1.1.1"                       # -> https://1.1.1.1/dns-query
    - "https://dns.google/dns-query"  # used verbatim
    - "tls://dns.quad9.net:853"       # scheme overrides mode
```

In `tls`/`https` mode the backend runs a small forwarder on the browser network and points containers at it; the encrypted hop is upstream. The container firewall allows only port 53 to the backend. If the browser network address cannot be determined, the deployment falls back to plaintext resolvers and logs a warning.

### Enable the warm pool

```yaml
warm_pool:
  enabled: true
  size: 2
```

Or without editing the file: `WARM_POOL_ENABLED=true WARM_POOL_SIZE=2`.

### Require a token to create sessions

In `.env` (picked up by Compose):

```bash
ISOLA_API_TOKEN="$(openssl rand -hex 32)"
# several: ISOLA_API_TOKEN="token-one,token-two"
```

Or in `config.yaml`:

```yaml
api:
  token:
    - "<openssl rand -hex 32>"
    - "<another>"
```

Programmatic clients send `Authorization: Bearer <token>` on `POST /api/sessions`. For the web UI on the same origin, set the same value once in the browser console:

```js
localStorage.setItem('isola_api_token', '<your token>')
```

See [Security → API and MCP authentication](security.md#api-and-mcp-authentication).

### Enable MCP

In `.env` (picked up by Compose):

```bash
ISOLA_MCP_ENABLED=true
ISOLA_MCP_TOKEN="$(openssl rand -hex 32)"
# several: ISOLA_MCP_TOKEN="token-one,token-two"
```

Or in `config.yaml`:

```yaml
mcp:
  enabled: true
  token:
    - "<openssl rand -hex 32>"
    - "<another>"
```

Any MCP client works: point it at `POST /mcp` with `Authorization: Bearer <token>` (see [Automation → MCP](automation.md#mcp-model-context-protocol)).
