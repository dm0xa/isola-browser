# Third parties

Isola has no telemetry, analytics, or update checks. The control plane and the
viewer talk only to the instance you run.

**Nothing leaves the sandbox except to your resolver and, for ASN, one DNS
TXT.** Recon — `/api/sessions/{id}/intel`, the panel's *Recon* tab and the
`host_intel` / `probe_host` MCP tools — fetches only the host you name. It has
no provider lookups: no RDAP, no certificate-transparency search, no Wayback.

| Party | Used for | Trigger | What it can see |
| :--- | :--- | :--- | :--- |
| **Your DNS resolver** (default `1.1.1.1`, `8.8.8.8`, `9.9.9.9`; plain, DoT or DoH) | Resolving `A`/`AAAA`, CNAME, PTR — and carrying the ASN TXT query | Any `host_intel` call and every probe (the hostname must resolve) | The hostname or IP being investigated |
| **Team Cymru** | ASN, network prefix, country, organisation | `host_intel` on each resolved public IP | That IP, encoded in a DNS name (`<reversed-ip>.origin.asn.cymru.com`), resolved through your resolver |
| **The target host** | `robots.txt`, `security.txt` (+ `/security.txt` fallback), `sitemap.xml`, `/.well-known/change-password`, `favicon.ico` | `probe_host` (default check set) | A request from the sandbox with `User-Agent: isola-recon/1.0 (+https://github.com/dm0xa/isola-browser)` |

DNS is unavoidable — the sandbox cannot reach a host without resolving it. Point
`dns.servers` / `ISOLA_DNS_SERVERS` at a resolver you trust, or use DoT/DoH
(`dns.mode`), and the ASN lookup travels the same path.

Outside recon, the only external contact is:

| Party | When |
| :--- | :--- |
| **DuckDuckGo** | Only when a user types a phrase (not a URL) into the session's address bar; the sandbox browser navigates there. The control plane never calls it. |

The UI is fully self-contained: no web fonts, no CDN, no third-party scripts.

## Switch

| Setting | Env | Effect |
| :--- | :--- | :--- |
| `intel.enabled` | `ISOLA_INTEL_ENABLED` | Master switch for recon. `false` makes `/intel` answer `403`, hides the panel actions, and the MCP tools return a clear error. Recon is the only feature that issues requests on a caller's behalf, so this is the switch for a deployment that must stay purely passive. |

It defaults to **on**; recon still does nothing until a caller names a host.
Set it to `false` when a session must never add requests of its own.

## TLS-inspecting networks

Recon verifies TLS certificates. On a network that terminates TLS (a corporate
proxy), the sandbox's trust store does not contain the inspecting root, so every
probe fails with:

```
Get "https://example.com/robots.txt": tls: failed to verify certificate:
x509: certificate signed by unknown authority
```

The browser keeps working because Chromium runs with
`--ignore-certificate-errors`; recon deliberately does not. The Headers tab still
shows what Chromium saw, but reports the browser's verdict beside it (CDP
`securityState`): a response whose chain was accepted only because errors were
ignored is labelled `untrusted cert`, and its posture grade is capped at `C`.
Install the proxy's root CA instead of relaxing verification:

```yaml
# config.yaml
intel:
  ca_file: /etc/ssl/certs/corporate-root.pem
```

```bash
# or via env
ISOLA_INTEL_CA_FILE=/path/to/corporate-root.pem
```

The control plane reads the PEM at startup (an unreadable path fails startup)
and passes it to the session container as `INTEL_CA_PEM`, where it is appended to
the system pool used only by recon. `GET /api/config` reports
`intel_ca_configured` so you can confirm it reached the UI. Tor sessions are
unaffected: their traffic does not traverse the inspecting proxy.

With the bundled Compose stack the PEM has to be readable inside the backend
container, so put it in `certs/` (git-ignored) and point the variable at the
mounted path:

```bash
# .env
ISOLA_INTEL_CA_FILE=/etc/isola/certs/corporate-root.pem
```

```bash
mkdir -p certs && cp /path/to/corporate-root.pem certs/
docker compose up -d --force-recreate isola
```

## Tor sessions

When a session is launched with Tor, recon follows the same circuit as the
browser:

- **HTTP probes** use the Tor SOCKS proxy (`socks5://127.0.0.1:9050`). The
  container firewall permits only Tor's egress, so a direct request would be
  dropped; the proxy also receives the hostname, so name resolution happens
  inside the circuit.
- **DNS lookups** go to Tor's DNSPort (`127.0.0.1:5353`), not the configured
  resolver. `A`, `AAAA` and `PTR` resolve normally.
- **ASN/org lookup is unavailable under Tor**: it needs a TXT query, and Tor's
  DNSPort does not answer TXT. The result says so in `notes` rather than
  silently omitting the data.

If you prefer no recon at all in Tor sessions, set `intel.enabled: false` on that
deployment; the Tor wiring is per-session, not per-feature.

## What never leaves the sandbox

Captured request/response metadata, response headers, cookies, bodies, console
output and screenshots stay inside the session. Posture grading, tracker
classification and the cookie-fingerprint catalogue all run in the streamer
process; nothing is sent to a provider to be scored.

## Verifying it

```bash
# Every outbound URL the recon engine knows about (probes build their own from
# the host being investigated, so the list is short by design):
grep -rn 'https://' browsers/isola-streamer/intel.go

# Provider checks no longer exist, so they are rejected before any egress:
curl -s "http://localhost:8080/api/sessions/$ID/intel?host=example.com&checks=rdap" \
  -H "X-Session-Token: $TOKEN"
# -> 400 {"error":"invalid_intel_query","detail":"unknown check: rdap"}
```
