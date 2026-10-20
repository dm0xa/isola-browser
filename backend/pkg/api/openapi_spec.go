package api

// getOpenAPISpec returns the OpenAPI 3.0.3 specification as a byte slice.
func getOpenAPISpec() []byte {
	return []byte(`{
  "openapi": "3.0.3",
  "info": {
    "title": "Isola Remote Browser API",
    "description": "REST API for creating, managing, and sharing remote browser sessions. Supports session token-based access for ownership control on all session operations.",
    "version": "1.0.0",
    "contact": {
      "name": "Isola",
      "url": "https://github.com/dm0xa/isola"
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
    }
  ],
  "components": {
    "securitySchemes": {
      "sessionToken": {
        "type": "apiKey",
        "in": "header",
        "name": "X-Session-Token",
        "description": "Session control token returned on session creation. Grants owner-level access to a specific session."
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
          "plugins": { "type": "array" }
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
}
