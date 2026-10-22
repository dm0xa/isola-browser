# Getting Started

This guide takes you from nothing to a first live session with Isola. It should take a few minutes; the first build is the slow part.

## Requirements

Isola is two pieces with different footprints: the **control plane** (the `isola` process — UI, API, WebSocket relay) and the per-session **browser sandbox** (a container).

### Control plane

The `isola` binary runs on its own — **no Docker and no browser required** — but without the sandbox it cannot launch sessions. The process itself is tiny (**~16 MB RSS idle**); the table is host headroom.

| | Minimum | Recommended |
| :--- | :--- | :--- |
| CPU | 1 vCPU | 2 vCPU |
| RAM | 128 MB | 256 MB |
| Disk | ~50 MB | ~100 MB |
| OS | Linux (x86-64/arm64) or macOS | — |
| Network | one TCP port (default `8080`) | — |

### Browser sandbox (to launch sessions)

Each concurrent session is a Docker container with hard limits: **2 GB RAM** (`--memory 2g --memory-swap 2g`), up to **3 CPU** (`--cpus 3`), and **512 PIDs**. All three are configurable with `ISOLA_SESSION_MEMORY_MB` / `ISOLA_SESSION_CPUS` / `ISOLA_SESSION_PIDS` (see [Configuration](configuration.md#environment-variables)). Idle (an empty page) measures **~320 MB** cgroup memory; budget **~1–2 GB RAM** per active session depending on the page. The shared `isola-browser:electron` image is ~1.2 GB of host disk. The CPU cap cannot exceed the host's core count — the Docker daemon rejects `--cpus` above it — so a host with fewer than 3 cores defaults to its core count, and a pinned `ISOLA_SESSION_CPUS` must not be higher.

Add roughly **1–2 GB RAM and 1–3 vCPU for every concurrent session**, plus one session's RAM again for each pre-started ("warm") container.

The image the binary launches is configurable with `ISOLA_BROWSER_IMAGE` (or `browser.image` in `config.yaml`). That is how an install points at the published `ghcr.io/dm0xa/isola-browser:electron` instead of the locally built `isola-browser:electron`, and [`scripts/install.sh`](../scripts/install.sh) does exactly that before falling back to a build.

isola is **multisession by default**: a launch adds a session and does not tear down the caller's others. On a shared or public host set `ISOLA_MAX_SESSIONS` to the number of sessions the host can hold — launches past the cap are refused with HTTP `429` (`max_sessions_reached`). See [Configuration → Cap concurrent sessions](configuration.md#cap-concurrent-sessions).

### Full stack

- **Docker 24+** with **BuildKit** and **Compose v2**, the `docker` **CLI on `PATH`** (isola shells out to it — the daemon alone is not enough), and access to `/var/run/docker.sock` (Isola uses it to start and stop session containers). Session containers join a Docker network named `browser-net`: the Compose stack creates it, and a binary install must create it itself — `docker network create browser-net`.
- **Disk:** the `isola` image is ~82 MB and `isola-browser:electron` ~1.2 GB; with Docker's build cache, budget **≥5 GB** (~20 GB for a small deployment).
- **Recommended host** for a small deployment (2–3 concurrent sessions): **4+ vCPU, 8+ GB RAM, 20+ GB disk**.

The image build is the slow part of the first run; later starts reuse cached layers.

### All-in-one server

When the control plane, Docker and the sandboxes share one host, size by concurrency: **~2 GB RAM and ~1–2 vCPU per session**, plus **~2 GB** baseline for the OS, Docker and the control plane.

| Concurrent sessions | vCPU | RAM |
| :--- | :--- | :--- |
| 2–3 | 4 | 8 GB |
| ~5 | 8 | 16 GB |
| ~10 | 16 | 32 GB |

A **16 GB / 8 vCPU** host comfortably runs about **5 concurrent sessions**. The 2 GB figure is the per-container cap (worst case): an idle session measures ~320 MB, so light pages fit more. Add both a CPU and a RAM allowance again for each pre-started ("warm") container.

## Run It

```bash
git clone https://github.com/dm0xa/isola-browser.git
cd isola-browser
docker compose up --build
```

Then open [http://localhost:8080](http://localhost:8080). That single process serves the UI, the API, and the WebSocket relay. No `.env` file is required — the defaults work as-is.

> **First build only:** `up --build` compiles the Go backend, builds the React frontend, and constructs the `isola-browser:electron` and `isola` images. Later starts reuse the cached layers and are fast.

> **Not using Compose?** [`scripts/install.sh`](../scripts/install.sh) installs the `isola` binary and sets up a systemd service (or a macOS LaunchAgent), installing Docker and creating the `browser-net` network on the way. See [Deployment → Run Without Compose](deployment.md#run-without-compose).
>
> **Installing by hand instead?** [Deployment → Manual install](deployment.md#manual-install) downloads the release tarball, verifies its checksum, pulls the sandbox image and creates the network, with no script piped to `bash`.

To run it detached and follow logs:

```bash
docker compose up --build -d
docker compose logs -f
```

> **On macOS** the Compose stack works with Docker Desktop, but the sandbox refuses to start without an enforcing MAC policy and Docker Desktop's Linux VM has neither AppArmor nor SELinux. Start the stack with `ISOLA_REQUIRE_MAC=0 docker compose up --build`; on a Linux host leave it at the default `1`. For a binary install, set `ISOLA_REQUIRE_MAC=0` in `isola.env` (the installer warns when no LSM is present).

## Your First Session

1. On the landing page, enter a URL and start a session. Isola provisions a container, boots the browser, and connects the stream.
2. The viewer shows the live page with a toolbar for the session controls: **End**, **extend**, resolution, audio, and a share link.
3. Sessions are time-limited (see `session` [in Configuration](configuration.md#session)). The default session is 10 minutes, extendable in 5-minute steps up to a 30-minute maximum.
4. Closing a session destroys its container. Nothing persists.

### Optional: Launch from the URL bar

Isola's landing page reads a `q` parameter, so it can be registered as a browser
search engine ("site search" in Chrome, a keyword bookmark in Firefox) and used
straight from the address bar:

```
https://isola.example/?q=%s
```

The text is prefilled into the address box, where **Open →** normalises it the
same way as typed input: a URL opens that site, anything else becomes a
DuckDuckGo search. Add `autorun=1` to launch without waiting for the keystroke:

```
https://isola.example/?q=%s&autorun=1
```

`autorun` provisions a session as soon as the page loads, so point it at an
entry you trust yourself to follow — every visit costs a container. Both
parameters are removed from the address bar once read, so reloading the viewer
or the landing page cannot relaunch or re-prefill.

## Optional: Customize

Copy the example environment file and edit it. Every variable is documented inline; anything you leave unset keeps its default.

```bash
cp .env.example .env
# edit .env, then:
docker compose up --build
```

See [Configuration](configuration.md) for the full reference.

## Optional: CLI Shortcuts

If you install [Just](https://github.com/casey/just), the repository ships shortcuts:

```bash
brew install just          # macOS
just up                    # docker compose up --build -d
just logs                  # docker compose logs -f
just down                  # docker compose down
```

## Hacking on Isola

For local UI iteration and Go development you need **Go 1.26+**, **Node 22+**, and **Just**:

```bash
cp .env.example .env
just setup                  # npm install + go mod tidy + build frontend
just build-backend          # API-only host (`backend/isola` from cmd/server)
just test-backend           # go test ./...
```

`just build-backend` does **not** embed the UI. The Compose image (`isola`) and `scripts/dev-rebuild.sh` (`backend/isola-su`) build `cmd/single-user` with `frontend-dist` embedded. Names, gitignore, and helper tools: [Binaries](binaries.md).

The full dev stack (nginx + Vite dev server + backend) is brought up with `just up`, which uses [`deploy/docker-compose.yml`](../deploy/docker-compose.yml).

## Optional: MCP for AI Agents

Enable MCP in `.env` (`ISOLA_MCP_ENABLED`, `ISOLA_MCP_TOKEN`), restart the stack, and point any MCP client at `POST /mcp` with `Authorization: Bearer <token>`. See [Automation](automation.md#mcp-model-context-protocol) for tools, smoke tests, and the snapshot vs screenshot distinction.

## Optional: Lock Down Session Creation

The default Compose stack allows anyone who can reach port `8080` to call `POST /api/sessions` (each call can spawn a container). On a shared or public host, set `ISOLA_API_TOKEN` and `RATE_LIMITING_ENABLED=true` in `.env`, restart, and follow [Security](security.md). The landing page still loads; the UI sends the token from `localStorage.isola_api_token` when `create_auth_required` is true in `/api/config`.

## Next Steps

- [Binaries](binaries.md) — host process, streamer, local build names.
- [Configuration](configuration.md) — sessions, DNS, warm pool, API/MCP tokens.
- [API reference](api.md) — create and control sessions programmatically.
- [Transport](transport.md) — Isola video/audio WebSocket (H.264, Opus, ABR).
- [Automation](automation.md) — MCP tools and the session input WebSocket.
- [Deployment](deployment.md) — put it behind TLS in production.
