// Package modulesdk is the open SDK for building isola modules: standalone
// binaries that plug into an isola backend over a Unix-domain-socket HTTP
// contract.
//
// A module process listens on the socket named by the ISOLA_MODULE_SOCKET
// environment variable (injected by the module host), answers GET /module/info
// with its Info document, and serves the endpoints matching the capabilities
// it registered via Server.Set*. See .docs/module-sdk.md for the full guide.
package modulesdk

import (
	"github.com/dm0xa/isola-browser/backend/pkg/plugin"
)

// APIVersion is the host↔module contract version this SDK implements.
const APIVersion = "v1"

// EnvSocket carries the UDS path the module must listen on.
const EnvSocket = "ISOLA_MODULE_SOCKET"

// Capability kind identifiers reported in Info.Implements. The host registers
// a registry adapter per reported kind.
const (
	KindAuth         = "auth"
	KindSessionHook  = "session_hook"
	KindWebSocket    = "websocket"
	KindDLP          = "dlp"
	KindURLPolicy    = "url_policy"
	KindAudit        = "audit"
	KindSessionGuard = "session_guard"
	KindFrontend     = "frontend"
)

// Info is the module self-description served at GET /module/info.
type Info struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	API         string   `json:"api"`
	Description string   `json:"description,omitempty"`
	Implements  []string `json:"implements"`
	// SubPath is the WebSocket sub-route served under /ws/{sessionId}/
	// when KindWebSocket is implemented (e.g. "network").
	SubPath string `json:"subpath,omitempty"`
}

// ---------------------------------------------------------------------------
// Contract v1 payloads. Session contexts, credentials, auth results, and audit
// events reuse the backend plugin types directly, so module code speaks the
// exact same data model as in-process plugins. Field names are wire-stable —
// extend only, never rename.
// ---------------------------------------------------------------------------

// GuardRequest is the body of POST /guard/before-create (SessionGuard).
type GuardRequest struct {
	UserID      string            `json:"user_id"`
	Fingerprint string            `json:"fingerprint"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	RemoteAddr  string            `json:"remote_addr"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// GuardResponse is the SessionGuard verdict. HTTP 200 = allow; 403 with a
// reason = reject; anything else = transport-level failure.
type GuardResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// URLAllowRequest is the body of POST /url/allow (URLPolicy).
type URLAllowRequest struct {
	Session *plugin.SessionContext `json:"session,omitempty"`
	URL     string                 `json:"url"`
}

// URLAllowResponse is the URLPolicy verdict.
type URLAllowResponse struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// DLPRequest is the body of POST /dlp/clipboard (DLPProvider).
// Direction is "in" (client→browser) or "out" (browser→client).
type DLPRequest struct {
	DataB64   string `json:"data_b64"`
	Direction string `json:"direction"`
}

// DLPResponse carries the (possibly rewritten) clipboard payload. A 403 with
// a reason blocks the transfer entirely.
type DLPResponse struct {
	DataB64 string `json:"data_b64"`
}

// HookRequest is the body of POST /hook/on-create and POST /hook/on-terminate
// (SessionHook).
type HookRequest struct {
	Session *plugin.SessionContext `json:"session,omitempty"`
}

// ErrorBody is the standard non-2xx response. Reason is shown to the user or
// logged by the host.
type ErrorBody struct {
	Reason string `json:"reason"`
}
