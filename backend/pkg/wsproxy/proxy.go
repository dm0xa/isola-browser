package wsproxy

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     CheckOrigin,
}

// CheckOrigin validates the Origin header for WebSocket upgrade requests.
//
// Browsers always send an Origin header on WebSocket handshakes, so a missing
// Origin is rejected (this blocks non-browser clients and stripped-header
// cross-site hijacking). Browser requests must present an Origin matching the
// request host. A localhost/127.0.0.1 origin is only accepted when the request
// itself is served from localhost, which keeps local development working
// without allowing a locally-served page to hijack a remote deployment.
func CheckOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}

	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}

	if sameHost(parsed.Host, r.Host) {
		return true
	}

	originHost := parsed.Hostname()
	if originHost == "localhost" || originHost == "127.0.0.1" || originHost == "::1" {
		requestHost, _ := splitHostPortLoose(r.Host)
		return requestHost == "localhost" || requestHost == "127.0.0.1" || requestHost == "::1"
	}
	return false
}

func sameHost(originHost, requestHost string) bool {
	originName, originPort := splitHostPortLoose(originHost)
	requestName, requestPort := splitHostPortLoose(requestHost)
	if !strings.EqualFold(originName, requestName) {
		return false
	}
	if originPort == "" || requestPort == "" {
		return true
	}
	return originPort == requestPort
}

func splitHostPortLoose(hostPort string) (string, string) {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return "", ""
	}
	if host, port, err := net.SplitHostPort(hostPort); err == nil {
		return strings.ToLower(host), port
	}
	if strings.HasPrefix(hostPort, "[") && strings.Contains(hostPort, "]") {
		return strings.ToLower(strings.Trim(hostPort, "[]")), ""
	}
	return strings.ToLower(hostPort), ""
}

// Proxy handles WebSocket connections and proxies them to session containers.
type Proxy struct {
	clipboardHook ClipboardHook
	adminAuth     AdminTokenValidator
	cache         *store.MemCache
	st            *store.Store
	orch          *orchestrator.Orchestrator
	activityMu    sync.Mutex
	activity      map[string]time.Time // sessionID → last activity
}

// New creates a new Proxy.
func New(cache *store.MemCache, st *store.Store, orch *orchestrator.Orchestrator) *Proxy {
	p := &Proxy{
		cache:    cache,
		st:       st,
		orch:     orch,
		activity: make(map[string]time.Time),
	}
	return p
}

// SetClipboardHook installs the DLP clipboard policy hook (nil = transparent
// relay). Must be called before serving traffic.
func (p *Proxy) SetClipboardHook(hook ClipboardHook) { p.clipboardHook = hook }

// SetAdminAuth installs the validator for admin-token WS access (shadowing).
// The fn receives the raw admin_token query value and reports whether it is
// a live admin access token. This binary never sets it, so admin tokens are
// always rejected — shadowing is a privileged capability.
func (p *Proxy) SetAdminAuth(fn AdminTokenValidator) { p.adminAuth = fn }

// AdminTokenValidator reports whether the supplied token belongs to a
// currently-authenticated admin.
type AdminTokenValidator func(token string) bool

// StartActivityFlusher periodically writes activity timestamps to DB.
func (p *Proxy) StartActivityFlusher(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Final flush
			p.flushActivity()
			return
		case <-ticker.C:
			p.flushActivity()
		}
	}
}

func (p *Proxy) flushActivity() {
	p.activityMu.Lock()
	toFlush := p.activity
	p.activity = make(map[string]time.Time)
	p.activityMu.Unlock()

	for sid := range toFlush {
		id, err := uuid.Parse(sid)
		if err != nil {
			continue
		}
		if err := p.st.Sessions.UpdateLastActivity(context.Background(), id); err != nil {
			log.Warn().Err(err).Str("session_id", sid).Msg("failed to update last_activity")
		}
	}
}

// trackActivity records activity for a session (debounced — caller should call often).
func (p *Proxy) trackActivity(sessionID string) {
	p.activityMu.Lock()
	p.activity[sessionID] = time.Now()
	p.activityMu.Unlock()
}

// HandleWebSocket handles incoming WebSocket connections on /ws/{sessionId}.
// Query params:
//
//	host=1           — marks this as the host connection (can send input)
//	channel=isola    — video relay (Isola transport; empty channel is treated as isola)
//	channel=input    — JSON input relay to isola-streamer
func (p *Proxy) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	sessionID := chi.URLParam(r, "sessionId")
	if sessionID == "" {
		http.Error(w, "missing session ID", http.StatusBadRequest)
		return
	}

	channel := r.URL.Query().Get("channel")
	isHost := r.URL.Query().Get("host") == "1"
	isInput := channel == "input"
	sessionToken := strings.TrimSpace(r.URL.Query().Get("session_token"))
	shareToken := strings.TrimSpace(r.URL.Query().Get("share_token"))
	adminToken := strings.TrimSpace(r.URL.Query().Get("admin_token"))

	log.Info().Str("session_id", sessionID).Bool("host", isHost).Str("channel", channel).Msg("websocket connection request")

	canView, canControl, err := p.authorizeAccess(r.Context(), sessionID, sessionToken, shareToken, adminToken)
	if err != nil {
		log.Error().Err(err).Str("session_id", sessionID).Msg("failed to authorize websocket access")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !canView {
		log.Warn().
			Str("event_type", "session.access_denied").
			Str("session_id", sessionID).
			Str("remote_addr", r.RemoteAddr).
			Bool("has_share_token", shareToken != "").
			Msg("audit: websocket access denied (invalid or missing token)")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !canControl {
		if isHost || isInput {
			log.Warn().
				Str("event_type", "session.control_denied").
				Str("session_id", sessionID).
				Str("remote_addr", r.RemoteAddr).
				Msg("audit: view-only token attempted control/input")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		isHost = false
	}

	addr, err := p.getContainerAddr(r.Context(), sessionID)
	if err != nil {
		log.Error().Err(err).Str("session_id", sessionID).Msg("failed to resolve container address")
		http.Error(w, "session not found or expired", http.StatusNotFound)
		return
	}

	clientConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Msg("websocket upgrade failed")
		return
	}
	defer clientConn.Close()

	if !isHost && !isInput {
		viewerKey := fmt.Sprintf("viewer_count:%s", sessionID)
		p.cache.Incr(r.Context(), viewerKey)
		p.cache.Expire(r.Context(), viewerKey, 1*time.Hour)
		defer p.cache.Decr(r.Context(), viewerKey)
	}

	if !isInput && channel != "" && channel != "isola" {
		http.Error(w, "unsupported channel", http.StatusBadRequest)
		return
	}

	log.Info().Str("session_id", sessionID).Str("container_addr", addr).Str("channel", channel).Msg("proxying connection")

	var bytesIn, bytesOut int64
	var closeOnce sync.Once
	done := make(chan struct{})
	closeAll := func() {
		closeOnce.Do(func() {
			close(done)
			clientConn.Close()
		})
	}

	if isInput {
		containerWS, _, err := websocket.DefaultDialer.Dial(
			fmt.Sprintf("ws://%s/input", addr),
			http.Header{},
		)
		if err != nil {
			log.Error().Err(err).Str("container_addr", addr).Msg("isola: failed to dial input ws")
			return
		}
		defer containerWS.Close()
		log.Info().Str("session_id", sessionID).Str("container_addr", addr).Msg("isola: input relay started")
		proxyDuplex(clientConn, containerWS, sessionID, closeAll, done, p.trackActivity, &bytesIn, &bytesOut, p.clipboardHook, !canControl)
	} else {
		containerWS, _, err := websocket.DefaultDialer.Dial(
			"ws://"+addr+"/?"+r.URL.RawQuery,
			nil,
		)
		if err != nil {
			log.Error().Err(err).Str("container_addr", addr).Msg("isola: failed to dial container ws")
			return
		}
		defer containerWS.Close()
		log.Info().Str("session_id", sessionID).Str("container_addr", addr).Msg("isola: duplex relay started")
		proxyDuplex(clientConn, containerWS, sessionID, closeAll, done, p.trackActivity, &bytesIn, &bytesOut, p.clipboardHook, !canControl)
	}

	log.Info().Str("session_id", sessionID).Str("channel", channel).Int64("bytes_in", bytesIn).Int64("bytes_out", bytesOut).Msg("proxy connection closed")
}

// keepalivePingInterval is how often keepalivePing pings the client. It is a
// variable so tests can exercise the ping cadence without waiting 30s.
var keepalivePingInterval = 30 * time.Second

// keepalivePing sends WebSocket pings every keepalivePingInterval to detect silently dropped connections.
//
// It also refreshes the session's activity clock. Payload-driven activity alone
// is not enough: a viewer watching a static page receives no messages (the
// encoder omits frames identical to the last one), so last_activity would stall
// and the idle reaper would tear down a sandbox that still has a viewer attached.
// A ping is proof the viewer is still there, and a ping write that fails stops
// the refresh so the reaper can collect an abandoned session.
func keepalivePing(clientConn *websocket.Conn, wg *sync.WaitGroup, closeAll func(), done chan struct{}, trackActivity func(string), sessionID string) {
	defer wg.Done()
	defer closeAll()
	ticker := time.NewTicker(keepalivePingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if err := clientConn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
			if trackActivity != nil && sessionID != "" {
				trackActivity(sessionID)
			}
		}
	}
}

// getContainerAddr looks up the TCP address of a session's stream container.
// A single port (5900) serves all channels.
func (p *Proxy) getContainerAddr(ctx context.Context, sessionID string) (string, error) {
	key := fmt.Sprintf("session:%s", sessionID)
	addr, err := p.cache.Get(ctx, key)
	if store.IsKeyNotFound(err) {
		return "", fmt.Errorf("session %s not found", sessionID)
	}
	if err != nil {
		return "", err
	}
	return addr, nil
}

// authorizeAccess returns (canView, canControl) for a WebSocket connection.
//
// Access is capability-based: a valid control token grants view+control, a
// valid share token grants view-only. There is deliberately no anonymous
// fallback — knowing a session ID must never be enough to view or control
// someone else's browser.
func (p *Proxy) authorizeAccess(ctx context.Context, sessionID, sessionToken, shareToken, adminToken string) (bool, bool, error) {
	// Admin shadow access: view-only, validated by the injected admin auth.
	// Checked first so an admin never falls into anonymous denial.
	if adminToken != "" {
		if p.adminAuth != nil && p.adminAuth(adminToken) {
			return true, false, nil
		}
		return false, false, nil
	}

	if sessionToken != "" {
		expected, err := p.cache.Get(ctx, fmt.Sprintf("session:%s:control_token", sessionID))
		if err == nil && subtle.ConstantTimeCompare([]byte(sessionToken), []byte(expected)) == 1 {
			return true, true, nil
		}
		if err != nil && !store.IsKeyNotFound(err) {
			return false, false, err
		}
	}

	// A share token identifies the presenter as a VIEWER.
	if shareToken != "" {
		sessionFromShare, err := p.cache.Get(ctx, "share:"+shareToken)
		if store.IsKeyNotFound(err) {
			return false, false, nil
		}
		if err != nil {
			return false, false, err
		}
		if subtle.ConstantTimeCompare([]byte(sessionFromShare), []byte(sessionID)) == 1 {
			return true, false, nil
		}
		return false, false, nil
	}

	return false, false, nil
}

// AuthorizeWS validates a WebSocket capability from the request's query
// parameters (session_token, share_token, or admin_token) for sessionID and
// returns (canView, canControl).
//
// Exported so plugin sub-routes under /ws/{sessionId}/... can enforce the same
// capability model as the main relay instead of trusting a bare session id.
func (p *Proxy) AuthorizeWS(r *http.Request, sessionID string) (canView, canControl bool, err error) {
	q := r.URL.Query()
	return p.authorizeAccess(r.Context(), sessionID,
		strings.TrimSpace(q.Get("session_token")),
		strings.TrimSpace(q.Get("share_token")),
		strings.TrimSpace(q.Get("admin_token")))
}
