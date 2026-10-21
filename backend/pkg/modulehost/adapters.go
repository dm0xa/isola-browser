package modulehost

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/modulesdk"
	"github.com/dm0xa/isola-browser/backend/pkg/plugin"
	"github.com/dm0xa/isola-browser/backend/pkg/registry"
)

// adapters bridges one module's contract endpoints to the in-process plugin
// interfaces. All adapters share the Module reference; RPC failures surface
// as errors exactly like in-process plugin failures would.
type adapters struct{ m *Module }

func (a adapters) implements(kind string, info modulesdk.Info) bool {
	for _, k := range info.Implements {
		if k == kind {
			return true
		}
	}
	return false
}

// registerInRegistry implements every capability the module reported and
// registers it with the global plugin registry.
func (a adapters) registerInRegistry(info modulesdk.Info) {
	if a.implements(modulesdk.KindAuth, info) {
		registry.RegisterAuth(authAdapter{a.m})
	}
	if a.implements(modulesdk.KindSessionHook, info) {
		registry.RegisterSessionHook(hookAdapter{a.m})
	}
	if a.implements(modulesdk.KindWebSocket, info) {
		registry.RegisterWebSocketPlugin(wsAdapter{a.m})
	}
	if a.implements(modulesdk.KindDLP, info) {
		registry.RegisterDLP(dlpAdapter{a.m})
	}
	if a.implements(modulesdk.KindURLPolicy, info) {
		registry.RegisterURLPolicy(urlAdapter{a.m})
	}
	if a.implements(modulesdk.KindAudit, info) {
		registry.RegisterAuditLogger(auditAdapter{a.m})
	}
	if a.implements(modulesdk.KindSessionGuard, info) {
		registry.RegisterSessionGuard(guardAdapter{a.m})
	}
	if a.implements(modulesdk.KindFrontend, info) {
		registry.RegisterFrontendPlugin(frontendAdapter{a.m})
	}
}

func sharedInfo(m *Module) plugin.Info {
	return plugin.Info{
		Name:        m.man.Module,
		Description: m.man.Description,
		Version:     m.man.Version,
	}
}

// ---------------------------------------------------------------------------
// AuthProvider
// ---------------------------------------------------------------------------

type authAdapter struct{ m *Module }

func (a authAdapter) Info() plugin.Info { return sharedInfo(a.m) }
func (a authAdapter) Name() string      { return a.m.man.Module }

func (a authAdapter) Login(ctx context.Context, creds plugin.Credentials) (*plugin.AuthResult, error) {
	var res plugin.AuthResult
	if err := a.m.clientSnapshot().post(ctx, "/auth/login", creds, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func (a authAdapter) Register(ctx context.Context, creds plugin.Credentials) (*plugin.AuthResult, error) {
	var res plugin.AuthResult
	if err := a.m.clientSnapshot().post(ctx, "/auth/register", creds, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ---------------------------------------------------------------------------
// SessionGuard
// ---------------------------------------------------------------------------

type guardAdapter struct{ m *Module }

func (g guardAdapter) Info() plugin.Info { return sharedInfo(g.m) }

func (g guardAdapter) BeforeCreate(ctx context.Context, r *http.Request, userID uuid.UUID, fingerprint string) error {
	req := modulesdk.GuardRequest{
		UserID:      userID.String(),
		Fingerprint: fingerprint,
		Method:      r.Method,
		Path:        r.URL.Path,
		RemoteAddr:  r.RemoteAddr,
		Headers:     modulesdk.FlattenHeaders(r.Header),
	}
	var res modulesdk.GuardResponse
	if err := g.m.clientSnapshot().post(ctx, "/guard/before-create", req, &res); err != nil {
		if apiErr, ok := err.(*apiError); ok && apiErr.StatusCode == http.StatusForbidden {
			return fmt.Errorf("%s", apiErr.Reason)
		}
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// URLPolicy
// ---------------------------------------------------------------------------

type urlAdapter struct{ m *Module }

func (u urlAdapter) Info() plugin.Info { return sharedInfo(u.m) }

func (u urlAdapter) AllowURL(ctx context.Context, session *plugin.SessionContext, rawURL string) (bool, string) {
	req := modulesdk.URLAllowRequest{Session: session, URL: rawURL}
	var res modulesdk.URLAllowResponse
	if err := u.m.clientSnapshot().post(ctx, "/url/allow", req, &res); err != nil {
		return false, err.Error()
	}
	return res.Allowed, res.Reason
}

// ---------------------------------------------------------------------------
// DLPProvider
// ---------------------------------------------------------------------------

type dlpAdapter struct{ m *Module }

func (d dlpAdapter) Info() plugin.Info { return sharedInfo(d.m) }

func (d dlpAdapter) FilterClipboard(data []byte, direction string) ([]byte, error) {
	req := modulesdk.DLPRequest{DataB64: base64.StdEncoding.EncodeToString(data), Direction: direction}
	var res modulesdk.DLPResponse
	if err := d.m.clientSnapshot().post(context.Background(), "/dlp/clipboard", req, &res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(res.DataB64)
}

// ---------------------------------------------------------------------------
// AuditLogger
// ---------------------------------------------------------------------------

type auditAdapter struct{ m *Module }

func (a auditAdapter) Info() plugin.Info { return sharedInfo(a.m) }

func (a auditAdapter) Log(ctx context.Context, event plugin.AuditEvent) {
	// Fire-and-forget: audit must never block the session path.
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	if err := a.m.clientSnapshot().post(ctx, "/audit/log", event, nil); err != nil {
		log.Warn().Str("module", a.m.man.Module).Err(err).Msg("modulehost: audit log delivery failed")
	}
}

// ---------------------------------------------------------------------------
// SessionHook
// ---------------------------------------------------------------------------

type hookAdapter struct{ m *Module }

func (h hookAdapter) Info() plugin.Info { return sharedInfo(h.m) }

func (h hookAdapter) OnCreate(ctx context.Context, session *plugin.SessionContext) error {
	return h.m.clientSnapshot().post(ctx, "/hook/on-create", modulesdk.HookRequest{Session: session}, nil)
}

func (h hookAdapter) OnTerminate(ctx context.Context, session *plugin.SessionContext) error {
	return h.m.clientSnapshot().post(ctx, "/hook/on-terminate", modulesdk.HookRequest{Session: session}, nil)
}

// ---------------------------------------------------------------------------
// WebSocketPlugin — raw byte pipe over the module UDS
// ---------------------------------------------------------------------------

type wsAdapter struct{ m *Module }

func (w wsAdapter) Info() plugin.Info { return sharedInfo(w.m) }
func (w wsAdapter) SubPath() string   { return w.m.man.SubPath }

// ServeWS transparently proxies the upgraded connection to the module's
// /ws endpoint over the UDS. The module performs its own WebSocket upgrade
// (the SDK hands it the hijackable writer), so no websocket semantics are
// interpreted here — after the module's 101 the pipe is raw bytes.
func (w wsAdapter) ServeWS(bw http.ResponseWriter, br *http.Request, sessionID string) {
	pipeWebSocket(bw, br, w.m)
}

// ---------------------------------------------------------------------------
// FrontendPlugin
// ---------------------------------------------------------------------------

type frontendAdapter struct{ m *Module }

func (f frontendAdapter) Info() plugin.Info { return sharedInfo(f.m) }

// FrontendConfig translates the manifest's declarative UI spec. Chunk points
// at the host-served ESM bundle so the SPA can dynamic-import it.
func (f frontendAdapter) FrontendConfig() plugin.FrontendPluginConfig {
	m := f.m.man
	cfg := plugin.FrontendPluginConfig{
		Chunk: fmt.Sprintf("/modules/%s/frontend.js", m.Module),
	}
	if m.Frontend.Route != nil {
		cfg.Route = &plugin.PluginRouteConfig{Path: m.Frontend.Route.Path, Component: m.Frontend.Route.Component}
	}
	if m.Frontend.ToolbarButton != nil {
		cfg.ToolbarButton = &plugin.ToolbarButtonConfig{
			ID: m.Frontend.ToolbarButton.ID, Icon: m.Frontend.ToolbarButton.Icon,
			Label: m.Frontend.ToolbarButton.Label, Order: m.Frontend.ToolbarButton.Order,
		}
	}
	if m.Frontend.SidePanel != nil {
		cfg.SidePanel = &plugin.SidePanelConfig{ComponentID: m.Frontend.SidePanel.ComponentID, Title: m.Frontend.SidePanel.Title}
	}
	return cfg
}
