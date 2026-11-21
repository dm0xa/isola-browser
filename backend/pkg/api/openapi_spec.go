package api

// getOpenAPISpec returns the OpenAPI 3.0.3 specification as a byte slice.
// The literal is built once: GET /api/openapi.json is unauthenticated and
// exempt from the rate limiter, so converting 28 KB of string per request
// allocated and copied the whole document on every hit.
func getOpenAPISpec() []byte {
	return openAPISpec
}

var openAPISpec = []byte(`{
  "openapi": "3.0.3",
  "info": {
    "title": "Isola Remote Browser API",
    "description": "REST API for creating, managing, and sharing remote browser sessions. Supports session token-based access for ownership control on all session operations.",
    "version": "1.0.0",
    "contact": {
      "name": "Isola",
      "url": "https://github.com/dm0xa/isola-browser"
    },
    "license": {
      "name": "MIT"
    }
  },
  "servers": [
    {
      "url": "http://localhost:8080",
      "description": "Local development"
    }
  ],
  "tags": [
    {
      "name": "Sessions",
      "description": "Remote browser session lifecycle"
    },
    {
      "name": "Shares",
      "description": "View-only share links for sessions"
    },
    {
      "name": "Config",
      "description": "Client-visible configuration"
    },
    {
      "name": "Capture",
      "description": "Resource archives recorded from a session"
    }
  ],
  "components": {
    "securitySchemes": {
      "sessionToken": {
        "type": "apiKey",
        "in": "header",
        "name": "X-Session-Token",
        "description": "Session control token returned on session creation. Grants owner-level access to a specific session."
      },
      "archiveKey": {
        "type": "apiKey",
        "in": "header",
        "name": "Authorization",
        "description": "Archive key returned by POST /api/sessions/{id}/capture/start, sent as 'Bearer <archive_key>'. Required to read or delete an archive; the operator listing uses the create-gate API token instead."
      }
    },
    "schemas": {
      "Session": {
        "type": "object",
        "properties": {
          "id": { "type": "string", "format": "uuid", "description": "Unique session identifier" },
          "user_id": { "type": "string", "format": "uuid", "description": "Owner user ID (anonymous sessions use all-zero UUID)" },
          "status": { "type": "string", "enum": ["provisioning", "running", "terminated", "error"], "description": "Current session status" },
          "container_id": { "type": "string", "description": "Docker container ID" },
          "container_host": { "type": "string", "description": "Container network address" },
          "container_port": { "type": "integer", "description": "Container stream port" },
          "target_url": { "type": "string", "description": "URL loaded in the remote browser" },
          "resolution": { "type": "string", "example": "1280x720", "description": "Browser viewport resolution" },
          "browser_type": { "type": "string", "enum": ["chromium"], "description": "Browser engine (Isola always runs Electron/Chromium)" },
          "transport": { "type": "string", "enum": ["isola"], "description": "Streaming transport protocol" },
          "tor_enabled": { "type": "boolean", "default": false, "description": "Whether session traffic is routed through Tor SOCKS5 proxy" },
          "tor_country": { "type": "string", "description": "Pinned Tor exit country/countries (ISO 3166-1 alpha-2, comma-separated) when Tor is enabled" },
          "created_at": { "type": "string", "format": "date-time" },
          "expires_at": { "type": "string", "format": "date-time", "nullable": true },
          "terminated_at": { "type": "string", "format": "date-time", "nullable": true },
          "last_activity_at": { "type": "string", "format": "date-time", "nullable": true }
        }
      },
      "SessionWithMeta": {
        "allOf": [
          { "$ref": "#/components/schemas/Session" },
          {
            "type": "object",
            "properties": {
              "session_token": { "type": "string", "description": "Control token (only returned for session owner)" },
              "ws_url": { "type": "string", "example": "/ws/550e8400-e29b-41d4-a716-446655440000" },
              "remaining_min": { "type": "integer", "description": "Minutes until expiry (-1 = unlimited)" },
              "extend_available": { "type": "boolean", "description": "Whether session can be extended" },
              "max_expires_at": { "type": "string", "format": "date-time", "nullable": true },
              "viewer_count": { "type": "integer", "description": "Number of active viewers" }
            }
          }
        ]
      },
      "CreateSessionRequest": {
        "type": "object",
        "required": ["url"],
        "properties": {
          "url": { "type": "string", "description": "Target URL to load in the remote browser", "example": "https://example.com" },
          "resolution": { "type": "string", "default": "1280x720", "example": "1920x1080" },
          "browser_type": { "type": "string", "enum": ["chromium"], "default": "chromium" },
          "browser_theme": { "type": "string", "enum": ["light", "dark", "auto"], "default": "dark" },
          "transport": { "type": "string", "enum": ["isola"], "default": "isola", "description": "Streaming transport protocol (Isola only)." },
          "tor_enabled": { "type": "boolean", "default": false },
          "tor_country": { "type": "string", "description": "Pin Tor exits to one or more ISO 3166-1 alpha-2 countries (e.g. 'se' or 'se,no'). Requires tor_enabled." },
          "fingerprint": { "type": "string", "description": "Client fingerprint for rate limiting" }
        }
      },
      "CreateSessionResponse": {
        "type": "object",
        "properties": {
          "id": { "type": "string", "format": "uuid" },
          "session_token": { "type": "string", "description": "Control token for session-scoped API access" },
          "ws_url": { "type": "string", "example": "/ws/550e8400-e29b-41d4-a716-446655440000" },
          "transport": { "type": "string", "enum": ["isola"] },
          "expires_at": { "type": "string", "format": "date-time" },
          "max_expires_at": { "type": "string", "format": "date-time" },
          "status": { "type": "string" },
          "remaining_min": { "type": "integer" },
          "extend_available": { "type": "boolean" },
          "share_token": { "type": "string", "description": "Auto-generated share token (when requested)" }
        }
      },
      "PaginatedSessions": {
        "type": "object",
        "properties": {
          "sessions": { "type": "array", "items": { "$ref": "#/components/schemas/Session" } },
          "total": { "type": "integer", "description": "Total number of sessions for the user" },
          "limit": { "type": "integer", "description": "Page size" },
          "offset": { "type": "integer", "description": "Page offset" }
        }
      },
      "CreateShareResponse": {
        "type": "object",
        "properties": {
          "token": { "type": "string", "description": "Share token (32 hex characters)" },
          "share_url": { "type": "string", "example": "http://localhost/share/abc123..." }
        }
      },
      "ListSharesResponse": {
        "type": "object",
        "properties": {
          "shares": {
            "type": "array",
            "items": { "$ref": "#/components/schemas/ShareInfo" }
          }
        }
      },
      "ShareInfo": {
        "type": "object",
        "properties": {
          "token": { "type": "string" },
          "created_at": { "type": "string", "format": "date-time" },
          "expires_at": { "type": "string", "format": "date-time" },
          "share_url": { "type": "string" }
        }
      },
      "ResizeRequest": {
        "type": "object",
        "required": ["width", "height"],
        "properties": {
          "width": { "type": "integer", "minimum": 64, "example": 1280 },
          "height": { "type": "integer", "minimum": 64, "example": 720 }
        }
      },
      "ResizeResponse": {
        "type": "object",
        "properties": {
          "requested_width": { "type": "integer" },
          "requested_height": { "type": "integer" },
          "width": { "type": "integer", "description": "Effective width (may be clamped)" },
          "height": { "type": "integer", "description": "Effective height (may be clamped)" },
          "clamped": { "type": "boolean", "description": "Whether dimensions were clamped to session resolution" }
        }
      },
      "ExtendSessionResponse": {
        "type": "object",
        "properties": {
          "status": { "type": "string" },
          "expires_at": { "type": "string", "format": "date-time" },
          "max_expires_at": { "type": "string", "format": "date-time" },
          "remaining_min": { "type": "integer" },
          "extend_available": { "type": "boolean" },
          "viewer_count": { "type": "integer" }
        }
      },
      "ErrorResponse": {
        "type": "object",
        "properties": {
          "error": { "$ref": "#/components/schemas/APIError" }
        }
      },
      "APIError": {
        "type": "object",
        "properties": {
          "code": { "type": "string", "description": "Machine-readable error code", "example": "session_not_found" },
          "message": { "type": "string", "description": "Human-readable error description", "example": "Session not found" }
        }
      },
      "ConfigResponse": {
        "type": "object",
        "properties": {
          "resolution_visible": { "type": "boolean" },
          "resolutions": { "type": "array", "items": { "type": "object", "properties": { "value": { "type": "string" }, "label": { "type": "string" } } } },
          "browser_visible": { "type": "boolean" },
          "browsers": { "type": "array", "items": { "type": "object", "properties": { "value": { "type": "string" }, "label": { "type": "string" } } } },
          "default_browser": { "type": "string" },
          "transport_visible": { "type": "boolean" },
          "transports": { "type": "array", "items": { "type": "object", "properties": { "value": { "type": "string" }, "label": { "type": "string" } } } },
          "default_transport": { "type": "string" },
          "tor_exit_country": { "type": "string", "description": "Default Tor exit country/countries (ISO 3166-1 alpha-2, comma-separated) applied to tor-enabled sessions; omitted when unset." },
          "network_log_max_entries": { "type": "integer", "description": "Number of network requests retained by the session capture window; omitted when unset (default 1000)." },
          "capture_enabled": { "type": "boolean", "description": "Whether the viewer should offer resource recording (capture.enabled)." },
          "plugins": { "type": "array" }
        }
      },
      "CaptureStartResponse": {
        "type": "object",
        "properties": {
          "archive_id": { "type": "string", "description": "Archive identifier (equal to the session id)" },
          "archive_key": { "type": "string", "description": "Secret key for the archive. Store it; it is never returned again." },
          "started_at": { "type": "string", "format": "date-time" }
        }
      },
      "CaptureStopResponse": {
        "type": "object",
        "properties": {
          "items": { "type": "integer" },
          "bytes": { "type": "integer", "format": "int64" },
          "deduped": { "type": "integer", "format": "int64" },
          "complete": { "type": "boolean", "description": "False when the final pull could not complete (a tombstone marks the archive)" }
        }
      },
      "CaptureStatusResponse": {
        "type": "object",
        "properties": {
          "archive_id": { "type": "string" },
          "session_id": { "type": "string" },
          "recording": { "type": "boolean" },
          "items": { "type": "integer" },
          "bytes": { "type": "integer", "format": "int64" },
          "deduped": { "type": "integer", "format": "int64" },
          "truncated": { "type": "boolean", "description": "The record budget or the streamer's disk budget was exhausted" },
          "started_at": { "type": "string", "format": "date-time" }
        }
      },
      "CaptureItem": {
        "type": "object",
        "properties": {
          "id": { "type": "string" },
          "url": { "type": "string" },
          "final_url": { "type": "string" },
          "method": { "type": "string" },
          "status": { "type": "integer" },
          "mime": { "type": "string" },
          "type": { "type": "string", "description": "CDP resource type (Script, Stylesheet, XHR, ...)" },
          "size_wire": { "type": "integer", "format": "int64" },
          "size_decoded": { "type": "integer", "format": "int64" },
          "sha256": { "type": "string", "description": "Content hash of the decoded body; the blob key" },
          "sha1": { "type": "string", "description": "Display-only hash from the network panel" },
          "filename": { "type": "string", "description": "Host-recomputed download name (<host>_<sha256[:12]><ext>)" },
          "file": { "type": "string", "description": "Host-recomputed readable path inside the archive (files/<host>/<path>)" },
          "tab": { "type": "integer" },
          "origin_host": { "type": "string" },
          "ts_requested": { "type": "integer", "format": "int64" },
          "ts_finished": { "type": "integer", "format": "int64" },
          "ts_saved": { "type": "integer", "format": "int64" },
          "from_disk_cache": { "type": "boolean" },
          "from_service_worker": { "type": "boolean" },
          "state": { "type": "string", "enum": ["captured", "too_large", "missing_body", "excluded_type", "failed"] }
        }
      },
      "CaptureArchive": {
        "type": "object",
        "properties": {
          "schema_version": { "type": "integer" },
          "archive_id": { "type": "string" },
          "session_id": { "type": "string" },
          "target_url": { "type": "string" },
          "created_at": { "type": "string", "format": "date-time" },
          "ended_at": { "type": "string", "description": "RFC3339 timestamp, empty while recording" },
          "complete": { "type": "boolean" },
          "stop_reason": { "type": "string", "enum": ["", "viewer_stop", "session_end", "quota", "crash"] },
          "key_sha256": { "type": "string", "description": "sha256 of the archive key; the key itself is never stored" },
          "counts": { "type": "object", "additionalProperties": { "type": "integer", "format": "int64" } },
          "sizes": { "type": "object", "additionalProperties": { "type": "integer", "format": "int64" } },
          "items": { "type": "array", "items": { "$ref": "#/components/schemas/CaptureItem" } }
        }
      },
      "CaptureSummary": {
        "type": "object",
        "properties": {
          "archive_id": { "type": "string" },
          "session_id": { "type": "string" },
          "target_url": { "type": "string" },
          "created_at": { "type": "string", "format": "date-time" },
          "ended_at": { "type": "string", "format": "date-time" },
          "complete": { "type": "boolean" },
          "items": { "type": "integer" },
          "bytes": { "type": "integer", "format": "int64" }
        }
      },
      "CaptureListResponse": {
        "type": "object",
        "properties": {
          "captures": { "type": "array", "items": { "$ref": "#/components/schemas/CaptureSummary" } }
        }
      }
    }
  },
  "paths": {
    "/api/config": {
      "get": {
        "tags": ["Config"],
        "summary": "Get client configuration",
        "description": "Returns feature flags, available resolutions, browsers, transports, and plugin config. No authentication required.",
        "operationId": "getConfig",
        "responses": {
          "200": {
            "description": "Client configuration",
            "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ConfigResponse" } } }
          }
        }
      }
    },
    "/api/sessions": {
      "post": {
        "tags": ["Sessions"],
        "summary": "Create a browser session",
        "description": "Creates a new remote browser session. Returns immediately with status 'provisioning'. The session transitions to 'running' asynchronously.",
        "operationId": "createSession",
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CreateSessionRequest" } } }
        },
        "responses": {
          "202": {
            "description": "Session creation accepted, provisioning in background",
            "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CreateSessionResponse" } } }
          },
          "400": { "description": "Invalid request", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "429": { "description": "Rate limited or max sessions reached", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}": {
      "get": {
        "tags": ["Sessions"],
        "summary": "Get session details",
        "description": "Returns full session metadata. Returns limited info for unauthenticated viewers; full details (including session_token) for the owner via X-Session-Token.",
        "operationId": "getSession",
        "security": [{ "sessionToken": [] }, {}],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "responses": {
          "200": { "description": "Session details", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/SessionWithMeta" } } } },
          "404": { "description": "Session not found", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      },
      "delete": {
        "tags": ["Sessions"],
        "summary": "Delete a session",
        "description": "Terminates a browser session, stops the container, and cleans up all associated resources. Authenticate via X-Session-Token header.",
        "operationId": "deleteSession",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "responses": {
          "200": { "description": "Session terminated", "content": { "application/json": { "schema": { "type": "object", "properties": { "status": { "type": "string", "example": "terminated" } } } } } },
          "403": { "description": "Session does not belong to you", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      },
      "patch": {
        "tags": ["Sessions"],
        "summary": "Extend session TTL",
        "description": "Adds time to a running session's expiry, up to the configured maximum. Requires owner authorization (X-Session-Token).",
        "operationId": "extendSession",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "responses": {
          "200": { "description": "Session extended", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ExtendSessionResponse" } } } },
          "403": { "description": "Not authorized", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}/resize": {
      "post": {
        "tags": ["Sessions"],
        "summary": "Resize remote browser viewport",
        "description": "Changes the capture resolution of a running Isola session. Dimensions are clamped to the session's launch resolution.",
        "operationId": "resizeSession",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "requestBody": {
          "required": true,
          "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ResizeRequest" } } }
        },
        "responses": {
          "200": { "description": "Resize applied", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ResizeResponse" } } } },
          "400": { "description": "Invalid request or unsupported transport", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "403": { "description": "Not authorized", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}/graph.png": {
      "get": {
        "tags": ["Sessions"],
        "summary": "Render the captured network as a resource-graph PNG",
        "description": "Renders the session's network log as a PNG: hosts, resource-type groups and files, laid out left-to-right (or top-to-bottom). The image is produced by the streamer, which holds the capture, and is never cached. Every option is validated before proxying; renders are limited to 2 in flight per session (429) and 45 seconds (504). Requires owner authorization (X-Session-Token).",
        "operationId": "getResourceGraph",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" },
          { "name": "theme", "in": "query", "schema": { "type": "string", "enum": ["dark", "light"], "default": "dark" }, "description": "Image theme" },
          { "name": "direction", "in": "query", "schema": { "type": "string", "enum": ["horizontal", "vertical"], "default": "horizontal" }, "description": "Flow direction" },
          { "name": "expand", "in": "query", "schema": { "type": "string", "enum": ["origin", "all", "collapsed"], "default": "origin" }, "description": "origin = the opened host's type groups, all = every group and resource, collapsed = hosts only" },
          { "name": "type", "in": "query", "schema": { "type": "string" }, "description": "Comma-separated resource types to keep: document,script,stylesheet,image,font,xhr,media,other" },
          { "name": "failed", "in": "query", "schema": { "type": "string", "enum": ["0", "1"] }, "description": "Draw only failed requests" },
          { "name": "scale", "in": "query", "schema": { "type": "integer", "minimum": 1, "maximum": 4, "default": 2 }, "description": "Raster scale" },
          { "name": "width", "in": "query", "schema": { "type": "integer", "minimum": 320, "maximum": 8000, "default": 1400 }, "description": "Frame the image is fitted into (multiplied by scale) and where long sibling runs wrap" },
          { "name": "height", "in": "query", "schema": { "type": "integer", "minimum": 320, "maximum": 8000, "default": 900 }, "description": "Frame the image is fitted into (multiplied by scale) and where long sibling runs wrap" },
          { "name": "origin", "in": "query", "schema": { "type": "string" }, "description": "Override the host the graph starts from" },
          { "name": "caption", "in": "query", "schema": { "type": "string", "enum": ["0", "1"], "default": "1" }, "description": "Draw the provenance caption (origin, counts, theme, UTC time)" }
        ],
        "responses": {
          "200": { "description": "PNG image", "content": { "image/png": { "schema": { "type": "string", "format": "binary" } } } },
          "400": { "description": "Invalid graph options", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "403": { "description": "Not authorized", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found or no longer active", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "409": { "description": "Session container is not ready (session is still starting)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "429": { "description": "A graph render is already running for this session (graph_render_busy)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "502": { "description": "Render failed or the image exceeded the output limit (graph_render_failed)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "504": { "description": "Render timed out (graph_render_timeout)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}/intel": {
      "get": {
        "tags": ["Sessions"],
        "summary": "Recon intelligence for a host captured in the session",
        "description": "Proxies the streamer's /intel endpoint: passive DNS enrichment (resolved IPs, PTR, ASN/org/country, CNAME chain) and, when checks are named, active probes from inside the sandbox (robots.txt, security.txt, sitemap.xml, well-known change-password, favicon hashes). Nothing runs until this call names a host, each check is a single bounded request to that host, and no third-party provider is ever contacted. Disabled when ISOLA_INTEL_ENABLED=false. Requires owner authorization (X-Session-Token).",
        "operationId": "getSessionIntel",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" },
          { "name": "host", "in": "query", "schema": { "type": "string" }, "description": "Bare hostname to enrich or probe (required unless ips is given)" },
          { "name": "ips", "in": "query", "schema": { "type": "string" }, "description": "Comma-separated IP addresses to enrich instead of resolving host" },
          { "name": "mode", "in": "query", "schema": { "type": "string", "enum": ["dns", "probe", "all"] }, "description": "dns = passive only; probe = active checks; all = both. Inferred when omitted." },
          { "name": "checks", "in": "query", "schema": { "type": "string" }, "description": "Comma list or 'all': robots, sitemap, securitytxt, wellknown, favicon. Default robots,securitytxt,sitemap." }
        ],
        "responses": {
          "200": { "description": "DNS and/or probe results", "content": { "application/json": { "schema": { "type": "object", "properties": { "host": { "type": "string" }, "dns": { "type": "object" }, "probes": { "type": "object" }, "generated_at": { "type": "integer" } } } } } },
          "400": { "description": "Invalid query or a non-public probe target (invalid_intel_query)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "403": { "description": "Not authorized, or recon is disabled by the operator", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found or no longer active", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "409": { "description": "Session container is not ready", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "502": { "description": "The streamer could not be reached (intel_failed)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "504": { "description": "The recon request timed out (intel_failed)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}/shares": {
      "get": {
        "tags": ["Shares"],
        "summary": "List share tokens for a session",
        "description": "Returns all active share tokens for a session. Requires owner authorization (X-Session-Token).",
        "operationId": "listShares",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "responses": {
          "200": { "description": "List of share tokens", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ListSharesResponse" } } } },
          "403": { "description": "Not authorized", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      },
      "post": {
        "tags": ["Shares"],
        "summary": "Create a share token",
        "description": "Generates a new view-only share link for a running session. The share token expires with the session. Requires owner authorization (X-Session-Token).",
        "operationId": "createShare",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "responses": {
          "201": { "description": "Share token created", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CreateShareResponse" } } } },
          "403": { "description": "Not authorized", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found or not running", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}/shares/{token}": {
      "delete": {
        "tags": ["Shares"],
        "summary": "Revoke a share token",
        "description": "Deletes a specific share token, immediately invalidating the share link. Requires owner authorization (X-Session-Token).",
        "operationId": "revokeShare",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" },
          { "name": "token", "in": "path", "required": true, "schema": { "type": "string" }, "description": "Share token to revoke" }
        ],
        "responses": {
          "200": { "description": "Share revoked", "content": { "application/json": { "schema": { "type": "object", "properties": { "status": { "type": "string", "example": "revoked" } } } } } },
          "403": { "description": "Not authorized", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/share/{token}": {
      "get": {
        "tags": ["Shares"],
        "summary": "Resolve a share token",
        "description": "Looks up a share token and returns view-only session information. No authentication required.",
        "operationId": "resolveShare",
        "parameters": [
          { "name": "token", "in": "path", "required": true, "schema": { "type": "string" }, "description": "Share token (32 hex characters)" }
        ],
        "responses": {
          "200": {
            "description": "Share resolved",
            "content": {
              "application/json": {
                "schema": {
                  "type": "object",
                  "properties": {
                    "id": { "type": "string", "format": "uuid" },
                    "target_url": { "type": "string" },
                    "vnc_password": { "type": "string", "description": "Legacy field; always empty for Isola sessions" },
                    "status": { "type": "string" },
                    "expires_at": { "type": "string", "format": "date-time" },
                    "transport": { "type": "string", "enum": ["isola"] }
                  }
                }
              }
            }
          },
          "404": { "description": "Share link expired or invalid", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "410": { "description": "Session no longer active", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}/capture/start": {
      "post": {
        "tags": ["Capture"],
        "summary": "Start recording downloaded resources",
        "description": "Starts resource capture in the sandbox and creates a host archive under the configured capture directory. Returns the archive key exactly once; store it, because only its sha256 is persisted. Requires owner authorization (X-Session-Token).",
        "operationId": "startCapture",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "responses": {
          "200": { "description": "Recording started", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CaptureStartResponse" } } } },
          "403": { "description": "Not authorized, or resource capture is disabled", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found or not running", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "409": { "description": "Container not ready, or a capture is already recording", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "502": { "description": "The streamer could not be reached (capture_failed)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}/capture/stop": {
      "post": {
        "tags": ["Capture"],
        "summary": "Stop recording and finalize the archive",
        "description": "Stops the sandbox capture, performs one final pull and marks the archive complete. Idempotent: a second stop returns the stored totals. Requires owner authorization (X-Session-Token).",
        "operationId": "stopCapture",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "responses": {
          "200": { "description": "Recording stopped", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CaptureStopResponse" } } } },
          "403": { "description": "Not authorized, or resource capture is disabled", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found or not running", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "409": { "description": "No capture is recording (capture_not_recording)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "502": { "description": "The final pull failed (capture_failed); the archive is marked with a tombstone", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/sessions/{id}/capture": {
      "get": {
        "tags": ["Capture"],
        "summary": "Live capture status",
        "description": "Returns the recording state and totals for a session, or 404 when it was never recorded. Requires owner authorization (X-Session-Token).",
        "operationId": "getCaptureStatus",
        "security": [{ "sessionToken": [] }],
        "parameters": [
          { "name": "id", "in": "path", "required": true, "schema": { "type": "string", "format": "uuid" }, "description": "Session ID" }
        ],
        "responses": {
          "200": { "description": "Capture status", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CaptureStatusResponse" } } } },
          "403": { "description": "Not authorized, or resource capture is disabled", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Session not found, not running, or never recorded (archive_not_found)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/captures": {
      "get": {
        "tags": ["Capture"],
        "summary": "List resource archives",
        "description": "Operator listing of every archive on the host, newest first. Uses the same bearer posture as session creation: when api.token is configured, Authorization: Bearer is required.",
        "operationId": "listCaptures",
        "security": [{}, { "archiveKey": [] }],
        "responses": {
          "200": { "description": "Archive summaries", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CaptureListResponse" } } } },
          "401": { "description": "Operator bearer token missing or invalid", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "403": { "description": "Resource capture is disabled", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/captures/{archiveID}": {
      "get": {
        "tags": ["Capture"],
        "summary": "Get an archive's metadata",
        "description": "Returns meta.json for one archive, including every captured item. Authorized by the archive key (Authorization: Bearer <archive_key>).",
        "operationId": "getCaptureArchive",
        "security": [{ "archiveKey": [] }],
        "parameters": [
          { "name": "archiveID", "in": "path", "required": true, "schema": { "type": "string" }, "description": "Archive ID (the session ID)" }
        ],
        "responses": {
          "200": { "description": "Archive metadata with items", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/CaptureArchive" } } } },
          "403": { "description": "Missing or invalid archive key (archive_key_invalid)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Archive not found (archive_not_found)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      },
      "delete": {
        "tags": ["Capture"],
        "summary": "Delete an archive",
        "description": "Removes the archive directory. Authorized by the archive key or the operator bearer token.",
        "operationId": "deleteCaptureArchive",
        "security": [{ "archiveKey": [] }],
        "parameters": [
          { "name": "archiveID", "in": "path", "required": true, "schema": { "type": "string" }, "description": "Archive ID (the session ID)" }
        ],
        "responses": {
          "200": { "description": "Archive deleted", "content": { "application/json": { "schema": { "type": "object", "properties": { "status": { "type": "string", "example": "deleted" } } } } } },
          "403": { "description": "Missing or invalid archive key (archive_key_invalid)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Archive not found (archive_not_found)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/captures/{archiveID}/blobs/{sha256}": {
      "get": {
        "tags": ["Capture"],
        "summary": "Download one archived resource body",
        "description": "Serves the decoded response body. Content-Type comes from the archive metadata, the response is an attachment, nosniff and never cached. Authorized by the archive key.",
        "operationId": "getCaptureBlob",
        "security": [{ "archiveKey": [] }],
        "parameters": [
          { "name": "archiveID", "in": "path", "required": true, "schema": { "type": "string" }, "description": "Archive ID (the session ID)" },
          { "name": "sha256", "in": "path", "required": true, "schema": { "type": "string", "pattern": "^[0-9a-f]{64}$" }, "description": "Blob content hash" }
        ],
        "responses": {
          "200": { "description": "Resource body", "content": { "application/octet-stream": { "schema": { "type": "string", "format": "binary" } } } },
          "403": { "description": "Missing or invalid archive key (archive_key_invalid)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Archive or blob not found (blob_not_found)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/captures/{archiveID}/files/{path}": {
      "get": {
        "tags": ["Capture"],
        "summary": "Download one archived resource by its readable path",
        "description": "Serves the decoded response body stored at files/<host>/<path>. Content-Type comes from the archive metadata, the response is an attachment, nosniff and never cached. Authorized by the archive key. Paths outside files/ are rejected.",
        "operationId": "getCaptureFile",
        "security": [{ "archiveKey": [] }],
        "parameters": [
          { "name": "archiveID", "in": "path", "required": true, "schema": { "type": "string" }, "description": "Archive ID (the session ID)" },
          { "name": "path", "in": "path", "required": true, "schema": { "type": "string" }, "description": "Readable path as recorded in the item's file field (files/<host>/<path>)" }
        ],
        "responses": {
          "200": { "description": "Resource body", "content": { "application/octet-stream": { "schema": { "type": "string", "format": "binary" } } } },
          "400": { "description": "Path outside files/ or malformed (invalid_path)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "403": { "description": "Missing or invalid archive key (archive_key_invalid)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Archive or file not found (blob_not_found)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/captures/{archiveID}/download": {
      "get": {
        "tags": ["Capture"],
        "summary": "Download an archive as a zip",
        "description": "Streams a zip containing meta.json and every captured blob. Authorized by the archive key.",
        "operationId": "downloadCaptureArchive",
        "security": [{ "archiveKey": [] }],
        "parameters": [
          { "name": "archiveID", "in": "path", "required": true, "schema": { "type": "string" }, "description": "Archive ID (the session ID)" }
        ],
        "responses": {
          "200": { "description": "Zip archive", "content": { "application/zip": { "schema": { "type": "string", "format": "binary" } } } },
          "403": { "description": "Missing or invalid archive key (archive_key_invalid)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } },
          "404": { "description": "Archive not found (archive_not_found)", "content": { "application/json": { "schema": { "$ref": "#/components/schemas/ErrorResponse" } } } }
        }
      }
    },
    "/api/openapi.json": {
      "get": {
        "tags": ["Config"],
        "summary": "OpenAPI specification",
        "description": "Returns the OpenAPI 3.0 specification for this API. No authentication required.",
        "operationId": "getOpenAPISpec",
        "responses": {
          "200": { "description": "OpenAPI 3.0 specification", "content": { "application/json": {} } }
        }
      }
    }
  }
}`)
