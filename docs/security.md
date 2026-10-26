# Security

Isola's threat model is simple: a page you open may be hostile, and it must not reach anything it shouldn't. Each session runs in its own freshly created container with a deliberately constrained runtime, and the container is destroyed when the session ends.

## Isolation Layers

**Session container hardening.** The orchestrator launches every session container with these flags (see [`security_args.go`](../backend/pkg/orchestrator/security_args.go)):

| Control | Setting |
| :--- | :--- |
| Capabilities | `--cap-drop ALL`, adding back only `SETUID`, `SETGID`, `CHOWN`, `DAC_OVERRIDE`, `IPC_LOCK`, `KILL`, `NET_BIND_SERVICE`, `NET_ADMIN` (needed by the entrypoint to set up the sandbox; session processes drop them). `SYS_ADMIN` is deliberately **not** granted: it is what would let a compromised in-container root `umount` the read-only `/proc` masks, rewrite a global sysctl such as `kernel.core_pattern`, or map `/proc/kcore`. Without it, no process in the container can mount, unmount, or write a sysctl at all. |
| Privilege escalation | `--security-opt no-new-privileges`. |
| Filesystem | `--read-only` root, with `tmpfs` for `/tmp` (256 MB), `/run` and `/var/run` (64 MB each), `/home/isolauser` (512 MB), and `/dev/shm` sized from the session resolution. |
| Memory | `--memory 2g --memory-swap 2g`. |
| Processes | `--pids-limit 512`. |
| CPU | `--cpus 3`. |
| User | After firewall setup, PID 1 stays root; Xvfb, Electron, streamer, and helpers run as `isolauser` via `gosu`. |
| GPU | `/dev/dri` is passed through only when `ISOLA_GPU=1` on the orchestrator. |
| Syscalls | Docker's default seccomp profile; override with `ISOLA_SECCOMP_PROFILE`. |
| MAC policy | An enforcing LSM must confine the container — AppArmor's `docker-default` profile or SELinux's container policy. The entrypoint **fails closed** (`ISOLA_REQUIRE_MAC=0` overrides) if neither is enforcing, so a host that failed to apply a profile never serves an unconfined sandbox. |
| Published ports | `127.0.0.1::5900` and `127.0.0.1::9223` — bound to loopback only, never exposed on the host's external interfaces. |

**Control-plane container.** The backend is hardened to the same shape as the sessions it launches (`deploy/docker-compose.yml`):

| Control | Setting |
| :--- | :--- |
| Filesystem | `read_only: true`, with a `tmpfs` at `/tmp` (64 MB, `noexec,nosuid,nodev`) and the `isola-data` volume at `/var/lib/isola` for the session snapshot. |
| Capabilities | `cap_drop: ALL` — the control plane reaches Docker through the socket, not through capabilities. |
| Privilege escalation | `no-new-privileges:true`. |
| Limits | `pids_limit 512`, `mem_limit 1g` with `memswap_limit` equal to it, `cpus 2`. Override with `ISOLA_BACKEND_MEMORY` / `ISOLA_BACKEND_CPUS`. |
| Seccomp | Docker's default profile. `ISOLA_SECCOMP_PROFILE` is deliberately **not** set: the bundled profile is written for the browser image and aborts container setup on Docker 29.x (`runc: reopen exec fifo … operation not permitted`), so it must be validated on the host before it is enabled. |

This narrows the gap between the sandbox and its supervisor: an RCE in the Go process still gets the Docker socket (which is equivalent to host root), but no longer gets a writable rootfs, added capabilities, or unbounded memory and processes.

**Network isolation.** Session containers run on a dedicated Docker bridge (`browser-net`) with inter-container communication disabled; only the backend joins it, and it has no direct internet ingress. Inside the container, the entrypoint applies matching `iptables` **and** `ip6tables` policies (startup **fails closed** if rules cannot be applied). Outbound whitelists permit only HTTP, HTTPS, DNS, and NTP, and block RFC1918 and ULA targets so a session cannot probe your internal network. Docker's embedded resolver (`127.0.0.11`) is applied on IPv4 only so `ip6tables` is not fed invalid host literals.

**Service port ACL.** On INPUT, TCP **5900** (streamer) and **9223** (CDP relay) accept traffic only from the backend peer: `ISOLA_PROXY_IP` when set, otherwise the container default gateway. Other bridge neighbours cannot dial those ports.

**CDP binding.** Chromium is started with `remote-debugging-address=127.0.0.1` (see [`chrome_flags.js`](../browsers/electron-runner/src/chrome_flags.js)); only the in-container relay reaches the debugger from outside the network namespace.

**Access control.** There is deliberately no anonymous access to a session:

- **Session token** — returned at creation; grants view + control.
- **Share token** — grants **view-only** access; input and control attempts are refused with `403`.
- **Session id** — knowing one is never sufficient; a valid token is always required.

**Application controls.** Inbound URLs are validated on session creation (`DISABLE_URL_VALIDATION=true` turns this off — don't use it in a shared or public deployment). When you put Isola behind a reverse proxy, set `TRUSTED_PROXY_CIDRS` so per-IP rate limiting sees the real client address rather than the proxy's. The bundled nginx config also sets strict security headers (CSP, `X-Frame-Options`, `X-Content-Type-Options`, `Referrer-Policy`, `Permissions-Policy`).

## API and MCP Authentication

| Surface | Control | Default |
| :--- | :--- | :--- |
| `POST /api/sessions` | Optional `api.token` / `ISOLA_API_TOKEN` (one or more, each ≥ 32 chars). Clients send `Authorization: Bearer <token>`. | Open (local Compose) |
| `POST /mcp` | Requires `mcp.token` / `ISOLA_MCP_TOKEN` (one or more, each ≥ 32 chars) when MCP is enabled. | Disabled |
| Mutating REST | `RATE_LIMITING_ENABLED=true` — 120 mutating requests/min per client IP. | Off |

For a host reachable from the internet: set `ISOLA_API_TOKEN` and `RATE_LIMITING_ENABLED=true`, enable MCP only with a strong `ISOLA_MCP_TOKEN`, keep `DISABLE_URL_VALIDATION=false`, and terminate TLS in front of the service (see [Deployment](deployment.md)). The web UI reads `localStorage.isola_api_token` (or a signed-in `access_token`) when `GET /api/config` reports `create_auth_required: true`.

## What This Does and Does Not Protect Against

- Isola **does** isolate sessions from each other, from your host's network, and from persistent state: nothing installed or logged in a session survives it.
- Containers share the host kernel. This is strong isolation, not a virtual machine; a kernel-level container escape is out of scope. Run Isola on a host you are prepared to dedicate to it, and keep Docker and the kernel patched.
- The container's own mount table (`mountinfo`) lists the engine's host-side storage paths (an overlay `lowerdir`/`upperdir` under the runtime's data dir). That is inherent to how the runtime composes the root filesystem, discloses nothing reachable, and is not treated as a boundary.
- Tor routing (`tor_enabled`) changes where traffic goes, not how strongly the sandbox is isolated. Exit-country pinning (`tor.exit_country` / a session's `tor_country`) is stricter still: it is applied with `StrictNodes`, so a country with no reachable exit fails the session instead of egressing elsewhere — and because it narrows the pool of exits, it reduces the anonymity Tor otherwise provides. Treat it as a routing control, not a privacy upgrade.
- The 2 GB / 3 CPU / 512 PID limits are DoS guards, not performance tuning; a page that needs more will be constrained.

## Reporting a Vulnerability

Report suspected vulnerabilities privately to the maintainers rather than opening a public issue, and include reproduction steps.
