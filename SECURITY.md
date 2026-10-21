# Security

Isola's threat model is a hostile page inside a disposable container. See
[docs/security.md](./docs/security.md) for the isolation controls.

## Reporting a vulnerability

Please do **not** open a public issue for a security bug.

Report it privately via GitHub Security Advisories on
[dm0xa/isola-browser](https://github.com/dm0xa/isola-browser/security/advisories/new).

Include a short description, impact, and steps to reproduce. We will
acknowledge the report and work on a fix before any public disclosure.

## What is in scope

- Escape from a session container to the host or to another session
- Forging session, share, API, or MCP tokens
- SSRF from session create / navigate that reaches the Isola host or RFC1918
- Auth bypass on the API or WebSocket relay

## What is out of scope

- A malicious page attacking its own session (that is the sandbox)
- Denial of service by spawning many sessions on an open `POST /api/sessions`
  (set `ISOLA_MAX_SESSIONS`, `ISOLA_API_TOKEN` and `RATE_LIMITING_ENABLED` on public hosts)
- Bugs that require a compromised Isola process or Docker socket
