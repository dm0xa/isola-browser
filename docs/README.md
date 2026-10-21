# Isola documentation

Isola is a self-hosted, disposable browser sandbox. Every browsing session runs in its own hardened Docker container and streams its live view to your browser; when the session ends the container is destroyed, so no cookies, history, or state carry over between sessions.

This directory is the external documentation — what an operator or an API consumer needs to run, configure, and integrate Isola. For the source layout, build targets, and a one-screen quick start, see the repository [README](../README.md).

## Start Here

| I want to… | Read |
| :--- | :--- |
| Run Isola for the first time | [Getting started](getting-started.md) |
| Change ports, session limits, DNS, or the warm pool | [Configuration](configuration.md) |
| Understand how the pieces fit together | [Architecture](architecture.md) |
| See which binaries exist and how they are built | [Binaries](binaries.md) |
| Understand the Isola video/audio wire | [Transport](transport.md) |
| Drive Isola from my own code | [API reference](api.md) |
| Automate with an AI agent (MCP) or a custom client | [Automation](automation.md) |
| Understand the isolation model | [Security](security.md) |
| Lock down API/MCP on a public host | [Security → API and MCP authentication](security.md#api-and-mcp-authentication) |
| Run it in production | [Deployment](deployment.md) |
| Diagnose a problem | [Troubleshooting](troubleshooting.md) |

## At a Glance

- **One process, one port** — a single Go binary serves the React UI, the REST API, the WebSocket relay, and (optionally) an MCP endpoint on the same port (default `8080`).
- **One container per session** — ephemeral, read-only root filesystem, dropped capabilities, loopback-only published ports.
- **Single transport: Isola** — H.264 Annex-B over WebSocket, decoded in the browser with WebCodecs. No VNC and no WebRTC.
- **Automation-ready** — optional MCP (snapshots, screenshots, the network log, resource-graph images, input) plus a JSON input protocol over the session WebSocket for custom clients.

## License

Isola is MIT. See the repository [LICENSE](../LICENSE).
