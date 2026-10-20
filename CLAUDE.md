# CLAUDE.md

Guidance for agents working in this repository. Depth lives in
[`README.md`](./README.md), [`CONTRIBUTING.md`](./CONTRIBUTING.md) and
[`docs/`](./docs/README.md); link out instead of restating.

## What this is

**isola** — disposable browser sandboxes. A Go control plane boots a throwaway
Docker container running offscreen Chromium, streams it to the browser over a
WebSocket relay (H.264), and destroys it on close. No VNC, no WebRTC.

## Layout

| Path | What it is |
| :--- | :--- |
| `backend/` | Go control plane. Module `github.com/dm0xa/isola/backend`. `cmd/server` = API-only binary, `cmd/single-user` = API + embedded UI; both are thin wrappers around `internal/app.Run`, which owns middleware and jobs. Logic in `pkg/`: API, orchestrator, WebSocket relay, store, config, MCP. |
| `frontend/` | React 18 + Vite + TypeScript + Tailwind UI, embedded into the Go binary. **No store library** — component state plus module-level singletons, and plain `fetch`/axios polling (`react-query` is mounted at the root but unused). |
| `browsers/` | Sandbox images. `isola-streamer/` is its own Go module (capture, encode, CDP, input; port 5900 in-container). `electron-runner/` holds the Electron/Chromium glue. `browser-node-electron/` builds `isola-browser:electron`. **Build context is `./browsers`**, not the repo root. |
| `deploy/` | Local full stack (nginx + Vite + backend). |
| `docs/` | User-facing documentation. |
| `scripts/` | `install.sh`, release/dev helpers. |

## Commands

Canonical commands are the ones CI runs (`.github/workflows/ci.yml`) — keep them
green. Run the narrowest relevant suite before claiming success.

```bash
just test-backend     # cd backend && go test ./...
just test-streamer    # cd browsers/isola-streamer && go test ./...  (needs ffmpeg)
cd frontend && npm test
just lint-backend     # go vet ./...
just lint-frontend    # eslint
just fmt-backend      # go fmt ./...
```

Builds: `just build-backend`, `just build-frontend`,
`just build-browser-node-electron`. Full stack: `docker compose up --build`;
dev stack: `just up-dev`. Requirements: Go 1.26+, Node 20+, Just. CI does **not**
run `browsers/electron-runner`'s node tests — run them yourself if you touch that
tree.

## Rules

### Backend

- **Go modules are separate.** `backend/` and `browsers/isola-streamer/` have
  their own `go.mod`. Run Go commands from the module directory. `vendor/` is
  gitignored; Docker resolves deps with `go mod download`, so a stale local
  `vendor/` makes a local build diverge from the image. Use `just tidy-backend`
  for `go mod tidy` — do not hand-edit `go.mod`/`go.sum`.
- **Env vars** resolve through `getenv()` in `backend/pkg/config/config.go`:
  `ISOLA_<NAME>` first, then the legacy bare `<NAME>`, and an empty env value can
  never override a non-empty YAML value. A new key needs the `configFile` struct
  entry + its default, `.env.example`, a `backend/config.yaml` comment, and
  `docs/configuration.md`. `config.Load()` reads `config.yaml` relative to the
  CWD and silently falls back to defaults when it is missing. A few older call
  sites still use `os.Getenv` directly and therefore skip the legacy fallback
  (`orchestrator/security_args.go`, `middleware/realip.go`, `cmd/single-user`) —
  do not extend that.
- **Logging** is `github.com/rs/zerolog`. Exception: `backend/pkg/api/handler.go`
  uses the stdlib `log`. Never add chi's `Logger` middleware — the app installs
  `RedactingLogger`, which strips `session_token`/`share_token` from logs.
- **Routes and errors.** Every route is registered in `Handler.RegisterRoutes`;
  handlers are plain methods with no error return and read path params with
  `chi.URLParam`. Errors go through `respondError(w, status, code)`: `code` is a
  stable snake_case token whose human-readable `message` comes from
  `errorMessageForCode` — a token with no entry there is returned verbatim as the
  message, so add one. When the message must carry detail the token cannot
  express (a validation reason, a guard's own text), use
  `respondErrorMessage(w, status, code, message)` so the code stays matchable.
  Session ownership is proved per-handler with
  `X-Session-Token` (constant-time compare) — there is no auth middleware — and
  the `POST /api/sessions` create gate is a separate `Authorization: Bearer`
  check. Token-bearing responses set `Cache-Control: no-store`.
- **Middleware order is load-bearing**: `RequestID → TrustedRealIP →
  RedactingLogger → Recoverer → CORS → SecurityHeaders → rate limiter`.
  `TrustedRealIP` must stay before the rate limiter, which keys on `RemoteAddr`.
- **API changes** update `backend/pkg/api/openapi_spec.go` — a hand-maintained
  string served at `GET /api/openapi.json`, with nothing validating it — together
  with `docs/api.md`. Tests are `handler_<area>_test.go` in package `api`, build
  the `Handler` directly with `httptest`, and inject chi params by hand (see
  `newTestHandler`).
- **Per-session cache keys** are deleted from a hard-coded list in *both*
  `FinalizeRunningSession` and `CleanupOrphanContainers` — a new key must be
  added to both or it leaks.

### Sandbox and streaming

- **Sandbox hardening is load-bearing.** Do not relax the cap set, the
  `--memory-swap` == `--memory` setting, the tmpfs sizes, the read-only rootfs,
  non-root + `no-new-privileges`, or the fail-closed `iptables` egress — which
  lives in `browsers/browser-node-electron/entrypoint.sh` and applies only when
  PID 1 is root. `SYS_ADMIN` is deliberately omitted; seccomp is opt-in *and*
  x86_64-only. Security-sensitive edits need tests.
- **Never add `--disable-dev-shm-usage`**, and never move Chromium shm off
  `/dev/shm` — its size is derived from the session resolution. Chromium flags
  live in `browsers/electron-runner/src/chrome_flags.js` and their invariants are
  pinned by `browsers/electron-runner/test/chrome_flags.test.js`.
- **Never give a warm container a session label.** Labels are one set or the
  other, and the orphan sweep destroys a warm container carrying
  `isola.session_id`. A warm claim requires an *exact* resolution + theme + fps
  match, otherwise the request cold-starts.
- **Streamer ↔ frontend coupling**: the ABR ladder in `isola-streamer/abr.go` is
  mirrored in `frontend/src/hooks/useIsolaStream.ts`, and the ISOL wire header is
  owned jointly by `electron-runner/src/isol_protocol.js` and
  `isola-streamer/frame_reader.go`. A new input message type touches the streamer
  handlers plus `docs/transport.md` and `docs/automation.md`.
- **The streamer's WS e2e tests require ffmpeg on PATH and fail rather than skip
  without it.** In-container listeners bind `0.0.0.0`; host safety comes from the
  orchestrator publishing `127.0.0.1::5900` and `127.0.0.1::9223`.

### Frontend

- React 18 function components + hooks; Tailwind classes inline (no `cn`/clsx
  helper, no design tokens); icons from `lucide-react`. `npm run build` is
  `tsc -b && vite build`, so unused locals/params are build-fatal.
- Runtime config arrives from `GET /api/config` into `src/lib/config.ts` — do not
  introduce `import.meta.env` / `VITE_*` variables. There is no toast system;
  render errors inline. Tests are colocated `*.test.ts(x)` (vitest, no globals —
  import from `'vitest'` and clean up manually); `npm test` is `vitest run`.

### Repo hygiene

- **Multisession is a contract.** Launching a session never tears down another
  one; over-capacity launches return HTTP `429` (`max_sessions_reached`), gated
  by `ISOLA_MAX_SESSIONS`. Preserve that.
- **Do not commit** `.env*` (except `.env.example`), tokens, secrets, or build
  output — Go binaries (`backend/isola*`, `backend/server`,
  `browsers/isola-streamer/isola-streamer`) and the generated
  `backend/cmd/single-user/frontend-dist/*` (keep `.gitkeep`, or `//go:embed`
  fails to compile). See `.gitignore` for the full list; do not force-add ignored
  files.
- **After changing dependencies**, regenerate `NOTICE` with
  `scripts/generate-notices.mjs`.
- **Do not rename** `cmd/single-user`, the `pkg/version` ldflags target, or the
  `v*` tag shape — `.github/workflows/release.yml` keys off all three. Git tags
  are the version source: release builds stamp `GITHUB_REF_NAME` (minus the
  leading `v`), Docker and `backend/scripts/build.sh` use `git describe`, and a
  bare `go build` reports the `dev`/`unknown` defaults. Do not bump
  `pkg/version` or `frontend/package.json` per release.
- **Update the docs with the code.** Behavior, config, API, security, and
  deployment changes should update the matching `docs/*.md`, keeping `README.md`
  consistent with them.
- **Style.** Go: `gofmt`, small focused packages under `backend/pkg/`, constructor
  funcs returning `*T`. Frontend: strict TypeScript, function components with
  hooks, Tailwind for styling — do not add a state or data-fetching library.
