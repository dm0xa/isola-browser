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
	"sync/atomic"
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

// CheckOrigin: browsers always send Origin; a missing one is rejected, host
// origin must match, and localhost origins only for localhost-served requests.
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
	st            *store.MemStore
	orch          *orchestrator.Orchestrator
	// Presence is what the flusher needs; sync.Map keeps the per-message path lock-free (25-50/s per viewer).
	activity sync.Map // sessionID → *atomic.Int64 (unix nanos)
}

// New creates a new Proxy.
func New(cache *store.MemCache, st *store.MemStore, orch *orchestrator.Orchestrator) *Proxy {
	p := &Proxy{
		cache: cache,
		st:    st,
		orch:  orch,
	}
	return p
}

// SetClipboardHook installs the DLP clipboard policy hook (nil = transparent
// relay). Must be called before serving traffic.
func (p *Proxy) SetClipboardHook(hook ClipboardHook) { p.clipboardHook = hook }

// SetAdminAuth: this binary never sets it, so admin tokens are always rejected — shadowing is privileged.
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
	for _, sid := range p.drainActivity() {
		id, err := uuid.Parse(sid)
		if err != nil {
			continue
		}
		if err := p.st.UpdateLastActivity(context.Background(), id); err != nil {
			log.Debug().Err(err).Str("session_id", sid).Msg("relay: activity")
		}
	}
}

// drainActivity: deleting an entry being recorded now is harmless — this drain already saw the session.
func (p *Proxy) drainActivity() []string {
	var ids []string
	p.activity.Range(func(key, _ any) bool {
		ids = append(ids, key.(string))
		p.activity.Delete(key)
		return true
	})
	return ids
}

// activityDebounce: the flusher needs presence only; 1 Hz keeps the hot path off any lock.
var activityDebounce = time.Second

// trackActivity records activity for a session. It is debounced: a call inside
// activityDebounce is a single atomic load and a compare, with no write and no
// lock, so it cannot become the hot path at video frame rates.
func (p *Proxy) trackActivity(sessionID string) {
	if sessionID == "" {
		return
	}
	now := time.Now().UnixNano()
	if slot, ok := p.activity.Load(sessionID); ok {
		last := slot.(*atomic.Int64)
		if now-last.Load() < int64(activityDebounce) {
			return
		}
		last.Store(now)
		return
	}
	slot := new(atomic.Int64)
	slot.Store(now)
	if existing, loaded := p.activity.LoadOrStore(sessionID, slot); loaded {
		last := existing.(*atomic.Int64)
		if now-last.Load() >= int64(activityDebounce) {
			last.Store(now)
		}
	}
}

// HandleWebSocket: /ws/{sessionId}. host=1 marks the host connection; channel=isola (default)
// relays video, channel=input relays JSON input.
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

	canView, canControl, err := p.authorizeAccess(r.Context(), sessionID, sessionToken, shareToken, adminToken)
	if err != nil {
		log.Warn().Err(err).Str("session_id", sessionID).Msg("relay: authorize failed")
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

	if !isInput && channel != "" && channel != "isola" {
		http.Error(w, "unsupported channel", http.StatusBadRequest)
		return
	}

	addr, err := p.getContainerAddr(r.Context(), sessionID)
	if err != nil {
		log.Warn().Err(err).Str("session_id", sessionID).Msg("relay: address resolve failed")
		http.Error(w, "session not found or expired", http.StatusNotFound)
		return
	}

	log.Info().Str("session_id", sessionID).Str("container_addr", addr).Str("channel", channel).Msg("proxying connection")

	// Dial the container before upgrading: upgrading first gave a dead sandbox a
	// live 101 and a 2 Hz reconnect loop; dialing first answers a prompt 502.
	containerWS, err := p.dialContainer(addr, r, isInput)
	if err != nil {
		log.Error().Err(err).Str("session_id", sessionID).Str("container_addr", addr).Str("channel", channel).Msg("relay: dial failed")
		p.reapGoneContainer(sessionID)
		http.Error(w, "sandbox unavailable", http.StatusBadGateway)
		return
	}
	defer containerWS.Close()

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

	var bytesIn, bytesOut int64
	var closeOnce sync.Once
	done := make(chan struct{})
	closeAll := func() {
		closeOnce.Do(func() {
			close(done)
			clientConn.Close()
		})
	}

	proxyDuplex(clientConn, containerWS, sessionID, closeAll, done, p.trackActivity, &bytesIn, &bytesOut, p.clipboardHook, !canControl)

	log.Debug().Str("session_id", sessionID).Str("channel", channel).Int64("bytes_in", bytesIn).Int64("bytes_out", bytesOut).Msg("relay: connection closed")
}

// dialContainer forwards the client's query string — capability negotiation rides the handshake.
func (p *Proxy) dialContainer(addr string, r *http.Request, isInput bool) (*websocket.Conn, error) {
	target := "ws://" + addr + "/?" + r.URL.RawQuery
	if isInput {
		target = "ws://" + addr + "/input"
	}
	conn, _, err := websocket.DefaultDialer.Dial(target, nil)
	return conn, err
}

// reapGoneContainer: detached and off the response path; the orchestrator serializes per session.
func (p *Proxy) reapGoneContainer(sessionID string) {
	if p.orch == nil || p.st == nil || p.cache == nil {
		return
	}
	id, err := uuid.Parse(sessionID)
	if err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		p.orch.FinalizeIfContainerGone(ctx, p.st, p.cache, id)
	}()
}

// keepalivePingInterval is how often keepalivePing pings the client. It is a
// variable so tests can exercise the ping cadence without waiting 30s.
var keepalivePingInterval = 30 * time.Second

// keepalivePing: a static page sends no frames, so pings alone keep last_activity
// fresh — and a failing ping write lets the reaper collect an abandoned session.
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

// authorizeAccess: capability-based — control token = view+control, share token =
// view-only. No anonymous fallback: knowing a session id is never enough.
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

// AuthorizeWS validates a WS capability from query params; exported for plugin sub-routes.
func (p *Proxy) AuthorizeWS(r *http.Request, sessionID string) (canView, canControl bool, err error) {
	q := r.URL.Query()
	return p.authorizeAccess(r.Context(), sessionID,
		strings.TrimSpace(q.Get("session_token")),
		strings.TrimSpace(q.Get("share_token")),
		strings.TrimSpace(q.Get("admin_token")))
}
