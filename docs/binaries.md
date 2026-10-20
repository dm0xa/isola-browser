# Binaries

Isola is two Go programs at runtime: the **host process** (UI, API, WebSocket relay) and **`isola-streamer`** inside each session container. `backend/cmd/` holds the host's two variants (below).

Built artifacts are gitignored. See [`.gitignore`](../.gitignore) (the “Binaries and build artifacts” block).

## What Docker Ships

`docker compose up --build` does not leave a host binary. It builds two images:

| Image | Binary inside | Source |
| :--- | :--- | :--- |
| `isola` | `/isola` | [`backend/cmd/single-user`](../backend/cmd/single-user) |
| `isola-browser:electron` | `/usr/local/bin/isola-streamer` (path in the image) | [`browsers/isola-streamer`](../browsers/isola-streamer) |

`isola` is the host. The Dockerfile copies `frontend/dist` into `backend/cmd/single-user/frontend-dist/` and compiles with [`backend/scripts/build.sh`](../backend/scripts/build.sh) as `/isola`. `//go:embed frontend-dist/*` bakes the UI into that binary. One process, one port (`8080`).

`isola-streamer` never runs on the host. Each session container encodes H.264 and speaks the Isola wire on container port `5900`. See [Transport](transport.md).

## Host-Process Variants

Two `main` packages implement the Isola process. They share the same API, orchestrator, and relay. They differ in how the UI is served.

| On-disk name | Package | UI | Who uses it |
| :--- | :--- | :--- | :--- |
| `isola` (in the image) / `backend/isola-su` | `backend/cmd/single-user` | Embedded (`frontend-dist`) | Compose; `scripts/dev-rebuild.sh` |
| `backend/isola` | `backend/cmd/server` | None (API and WS only) | `just build-backend` |

`cmd/single-user` is the self-contained product: in-memory store, embedded SPA. If the embed directory is empty except `.gitkeep`, it logs `frontend not embedded` and runs API-only.

`cmd/server` is the same backend **without** a baked-in UI. Use it behind Vite (`just up` / [`deploy/docker-compose.yml`](../deploy/docker-compose.yml)) or when you only need the API.

Default `go build ./cmd/server` from `backend/` names the file `server`. Default `go build ./cmd/single-user` names it `single-user`. Those names are gitignored too.

### Build the embedded host on the laptop

```bash
# UI into the embed dir, then the binary (scripts/dev-rebuild.sh does both)
cd frontend && npm run build
rsync -a dist/ ../backend/cmd/single-user/frontend-dist/
cd ../backend && ./scripts/build.sh ./isola-su ./cmd/single-user
```

`frontend-dist` is generated. Git keeps only `backend/cmd/single-user/frontend-dist/.gitkeep` so `//go:embed` still has a directory. Wipe everything else in that folder; do not commit hashed JS/CSS.

### Build the API-only host

```bash
just build-backend
# equivalent:
cd backend && ./scripts/build.sh isola ./cmd/server
```

Output: `backend/isola`.

## `isola-streamer`

```bash
cd browsers/isola-streamer && go build -o isola-streamer .
```

That local file is gitignored. Session containers use the copy built in [`browsers/browser-node-electron/Dockerfile`](../browsers/browser-node-electron/Dockerfile), not this path.

## Version Stamps

Git tags are the source of truth. The release workflow strips the leading `v`
from the pushed tag and stamps `pkg/version` via `-ldflags`; Docker and
`backend/scripts/build.sh` derive the same fields from `git describe --tags`
(which returns the nearest tag, plus commits since it when `HEAD` is untagged).
`GET /api/config` returns `version`, `commit`, and `build_time`, and the embedded
UI displays those backend values.

There is no per-release version bump in the source tree: `pkg/version` defaults
to `dev` for a bare `go build`, and `frontend/package.json` keeps an inert
`0.0.0` (it is not the product version and is not shipped in the bundle).
