package modulesdk

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/dm0xa/isola-browser/backend/pkg/plugin"
)

// WSHandlerFunc serves a plugin WebSocket sub-route. The SDK hands over the
// raw hijackable ResponseWriter so the module can run its own WebSocket
// upgrade (e.g. gorilla/websocket) — the host byte-pipes everything on that
// connection transparently.
type WSHandlerFunc func(w http.ResponseWriter, r *http.Request, sessionID string)

// Server adapts module capability implementations to the contract v1 HTTP
// surface served over the host-provided UDS.
type Server struct {
	info Info

	authP plugin.AuthProvider
	guard plugin.SessionGuard
	hook  plugin.SessionHook
	dlp   plugin.DLPProvider
	urlP  plugin.URLPolicy
	audit plugin.AuditLogger
	front plugin.FrontendPlugin

	wsSubPath string
	wsHandler WSHandlerFunc

	mux *http.ServeMux
}

// New creates a Server describing itself with the given Info.
func New(info Info) *Server {
	info.API = APIVersion
	s := &Server{info: info, mux: http.NewServeMux()}
	s.mux.HandleFunc("/module/info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.Info())
	})
	return s
}

// SetAuthProvider exposes an AuthProvider at POST /auth/login and /auth/register.
func (s *Server) SetAuthProvider(p plugin.AuthProvider) { s.authP = p }

// SetSessionGuard exposes a SessionGuard at POST /guard/before-create.
func (s *Server) SetSessionGuard(p plugin.SessionGuard) { s.guard = p }

// SetSessionHook exposes a SessionHook at POST /hook/on-create|on-terminate.
func (s *Server) SetSessionHook(p plugin.SessionHook) { s.hook = p }

// SetDLPProvider exposes a DLPProvider at POST /dlp/clipboard.
func (s *Server) SetDLPProvider(p plugin.DLPProvider) { s.dlp = p }

// SetURLPolicy exposes a URLPolicy at POST /url/allow.
func (s *Server) SetURLPolicy(p plugin.URLPolicy) { s.urlP = p }

// SetAuditLogger exposes an AuditLogger at POST /audit/log.
func (s *Server) SetAuditLogger(p plugin.AuditLogger) { s.audit = p }

// SetFrontendPlugin exposes frontend manifest data at GET /frontend/config.
func (s *Server) SetFrontendPlugin(p plugin.FrontendPlugin) { s.front = p }

// SetWebSocket exposes a WebSocket sub-route at GET /ws.
func (s *Server) SetWebSocket(subPath string, h WSHandlerFunc) {
	s.wsSubPath, s.wsHandler = subPath, h
}

// Info returns the (capability-augmented) info document.
func (s *Server) Info() Info {
	info := s.info
	info.Implements = s.capabilities()
	return info
}

// capabilities derives the Implements list from the registered capabilities,
// honoring explicit entries and appending any missing ones.
func (s *Server) capabilities() []string {
	set := map[string]bool{}
	out := make([]string, 0, 8)
	add := func(k string) {
		if !set[k] {
			set[k] = true
			out = append(out, k)
		}
	}
	for _, k := range s.info.Implements {
		add(k)
	}
	if s.authP != nil {
		add(KindAuth)
	}
	if s.guard != nil {
		add(KindSessionGuard)
	}
	if s.hook != nil {
		add(KindSessionHook)
	}
	if s.dlp != nil {
		add(KindDLP)
	}
	if s.urlP != nil {
		add(KindURLPolicy)
	}
	if s.audit != nil {
		add(KindAudit)
	}
	if s.front != nil {
		add(KindFrontend)
	}
	if s.wsHandler != nil {
		add(KindWebSocket)
	}
	return out
}

// wire mounts the capability endpoints. Called from ListenAndServe/Serve.
func (s *Server) wire() {
	if s.authP != nil {
		s.mux.HandleFunc("/auth/login", s.authEndpoint(s.authP.Login))
		s.mux.HandleFunc("/auth/register", s.authEndpoint(s.authP.Register))
	}
	if s.guard != nil {
		s.mux.HandleFunc("/guard/before-create", func(w http.ResponseWriter, r *http.Request) {
			var req GuardRequest
			if !s.decode(w, r, &req) {
				return
			}
			if err := s.guard.BeforeCreate(r.Context(), recreateRequest(r, req), userIDOrNil(req.UserID), req.Fingerprint); err != nil {
				writeError(w, http.StatusForbidden, err.Error())
				return
			}
			writeOK(w, GuardResponse{Allowed: true})
		})
	}
	if s.hook != nil {
		s.mux.HandleFunc("/hook/on-create", s.hookEndpoint(s.hook.OnCreate))
		s.mux.HandleFunc("/hook/on-terminate", s.hookEndpoint(s.hook.OnTerminate))
	}
	if s.dlp != nil {
		s.mux.HandleFunc("/dlp/clipboard", func(w http.ResponseWriter, r *http.Request) {
			var req DLPRequest
			if !s.decode(w, r, &req) {
				return
			}
			data, err := decodeB64(req.DataB64)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid data_b64")
				return
			}
			filtered, err := s.dlp.FilterClipboard(data, req.Direction)
			if err != nil {
				writeError(w, http.StatusForbidden, err.Error())
				return
			}
			writeOK(w, DLPResponse{DataB64: encodeB64(filtered)})
		})
	}
	if s.urlP != nil {
		s.mux.HandleFunc("/url/allow", func(w http.ResponseWriter, r *http.Request) {
			var req URLAllowRequest
			if !s.decode(w, r, &req) {
				return
			}
			allowed, reason := s.urlP.AllowURL(r.Context(), req.Session, req.URL)
			status := http.StatusOK
			if !allowed {
				status = http.StatusForbidden
			}
			writeJSONStatus(w, status, URLAllowResponse{Allowed: allowed, Reason: reason})
		})
	}
	if s.audit != nil {
		s.mux.HandleFunc("/audit/log", func(w http.ResponseWriter, r *http.Request) {
			var ev plugin.AuditEvent
			if !s.decode(w, r, &ev) {
				return
			}
			s.audit.Log(r.Context(), ev)
			writeOK(w, map[string]bool{"ok": true})
		})
	}
	if s.front != nil {
		s.mux.HandleFunc("/frontend/config", func(w http.ResponseWriter, _ *http.Request) {
			writeOK(w, s.front.FrontendConfig())
		})
	}
	if s.wsHandler != nil {
		s.mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
			s.wsHandler(w, r, r.URL.Query().Get("session_id"))
		})
	}
}

func (s *Server) authEndpoint(op func(ctx contextT, creds plugin.Credentials) (*plugin.AuthResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var creds plugin.Credentials
		if !s.decode(w, r, &creds) {
			return
		}
		result, err := op(r.Context(), creds)
		if err != nil {
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		writeOK(w, result)
	}
}

func (s *Server) hookEndpoint(op func(ctx contextT, session *plugin.SessionContext) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req HookRequest
		if !s.decode(w, r, &req) {
			return
		}
		if err := op(r.Context(), req.Session); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeOK(w, map[string]bool{"ok": true})
	}
}

func (s *Server) decode(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Serving
// ---------------------------------------------------------------------------

// Serve serves the contract on the provided listener. Blocks until the
// listener closes.
func (s *Server) Serve(l net.Listener) error {
	s.wire()
	srv := &http.Server{Handler: s.mux}
	return srv.Serve(l)
}

// ListenAndServe binds the UDS named by ISOLA_MODULE_SOCKET and serves.
// The socket file is removed first if stale, and again on exit.
func (s *Server) ListenAndServe() error {
	sock := os.Getenv(EnvSocket)
	if sock == "" {
		return fmt.Errorf("modulesdk: %s not set — run under the isola module host", EnvSocket)
	}
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("modulesdk: remove stale socket: %w", err)
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("modulesdk: listen %s: %w", sock, err)
	}
	defer os.Remove(sock)
	return s.Serve(l)
}

// Run is the one-liner for a module main: builds the Server, lets configure
// register capabilities, then serves until the host tears the process down.
func Run(info Info, configure func(*Server)) error {
	s := New(info)
	if configure != nil {
		configure(s)
	}
	return s.ListenAndServe()
}
