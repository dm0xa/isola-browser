package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/dm0xa/isola/backend/pkg/config"
	isomw "github.com/dm0xa/isola/backend/pkg/middleware"
	"github.com/dm0xa/isola/backend/pkg/models"
	"github.com/dm0xa/isola/backend/pkg/orchestrator"
	"github.com/dm0xa/isola/backend/pkg/store"
	"github.com/dm0xa/isola/backend/pkg/version"
)

// ---------------------------------------------------------------------------
// Extension points — general code is module-unaware.
// Main.go populates these from the plugin registry at startup; handler code
// never imports plugin or registry packages.
// ---------------------------------------------------------------------------

// SessionEvent carries metadata for lifecycle callbacks.
type SessionEvent struct {
	SessionID   string
	UserID      string
	ContainerID string
	TargetURL   string
	BrowserType string
	VncPassword string
	Host        string
	Port        int
	CdpPort     int
}

// PluginConfigFunc returns plugin metadata for the /api/config endpoint.
type PluginConfigFunc func() []map[string]interface{}

// HandlerExt holds optional extension points plugged in by main.go.
// Every field is nil-safe — if nil the corresponding feature is simply disabled.
type HandlerExt struct {
	// ValidateSession is called before a session is provisioned. One shot — main.go
	// composes guards + URL policies into a single func. Non-nil error = reject.
	ValidateSession func(ctx context.Context, r *http.Request, userID uuid.UUID, fingerprint, targetURL string) error
	// OnSessionReady fires after container is running + DB updated.
	OnSessionReady func(ctx context.Context, ev SessionEvent)
	// OnSessionDeleted fires after a session is terminated + container removed.
	OnSessionDeleted func(ctx context.Context, ev SessionEvent)
	// PluginConfig provides frontend plugin metadata. Nil = no plugins.
	PluginConfig PluginConfigFunc
}

// Handler holds dependencies for HTTP handlers.
type Handler struct {
	store *store.Store
	cache *store.MemCache
	orch  *orchestrator.Orchestrator
	cfg   *config.Config
	ext   *HandlerExt
	warm  *orchestrator.WarmPool

	// warmPinWarnOnce logs the DNS-pin downgrade for warm claims once per
	// process instead of on every claim.
	warmPinWarnOnce sync.Once
}

// New creates a new Handler.
func New(store *store.Store, cache *store.MemCache, orch *orchestrator.Orchestrator, cfg *config.Config) *Handler {
	return &Handler{store: store, cache: cache, orch: orch, cfg: cfg}
}

// SetWarmPool attaches the warm-container pool. Optional — when unset every
// session is provisioned cold.
func (h *Handler) SetWarmPool(p *orchestrator.WarmPool) { h.warm = p }

// SetExt attaches optional extension points. Must be called before RegisterRoutes.
func (h *Handler) SetExt(ext *HandlerExt) { h.ext = ext }

// RegisterRoutes mounts all API routes on the given router.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Route("/api", func(r chi.Router) {
		// Config (public — client needs to know what features are enabled)
		r.Get("/config", h.GetConfig)

		// Sessions (public — no auth required to create)
		r.Post("/sessions", h.CreateSession)

		// Session management — all require X-Session-Token to prove ownership
		r.Get("/sessions/{id}", h.GetSession)
		r.Patch("/sessions/{id}", h.ExtendSession)
		r.Post("/sessions/{id}/resize", h.ResizeSession)
		r.Post("/sessions/{id}/audio", h.ToggleAudio)
		r.Get("/sessions/{id}/graph.png", h.GetResourceGraph)
		r.Delete("/sessions/{id}", h.DeleteSession)

		// Share management for a session
		r.Post("/sessions/{id}/share", h.CreateShare) // deprecated, use /shares
		r.Get("/sessions/{id}/shares", h.ListShares)
		r.Post("/sessions/{id}/shares", h.CreateShare)
		r.Delete("/sessions/{id}/shares/{token}", h.RevokeShare)
	})

	// Share resolver (public — maps token to view-only session)
	r.Get("/api/share/{token}", h.GetShare)

	// OpenAPI specification (public)
	r.Get("/api/openapi.json", h.GetOpenAPISpec)
}

// GetConfig returns client-visible feature flags and settings.
func (h *Handler) GetConfig(w http.ResponseWriter, r *http.Request) {
	type optionEntry struct {
		Value string `json:"value"`
		Label string `json:"label"`
	}
	resolutions := make([]optionEntry, len(h.cfg.Resolutions))
	for i, o := range h.cfg.Resolutions {
		resolutions[i] = optionEntry{Value: o.Value, Label: o.Label}
	}
	browsers := make([]optionEntry, len(h.cfg.BrowserOptions))
	for i, o := range h.cfg.BrowserOptions {
		browsers[i] = optionEntry{Value: o.Value, Label: o.Label}
	}
	transports := make([]optionEntry, len(h.cfg.TransportOptions))
	for i, o := range h.cfg.TransportOptions {
		transports[i] = optionEntry{Value: o.Value, Label: o.Label}
	}
	resp := map[string]interface{}{}
	if len(resolutions) > 0 {
		resp["resolutions"] = resolutions
	}
	if len(browsers) > 0 {
		resp["browsers"] = browsers
	}
	if len(transports) > 0 {
		resp["transports"] = transports
	}
	if h.cfg.DefaultTransport != "" {
		resp["default_transport"] = h.cfg.DefaultTransport
	}
	// Isola tab strip / address bar (defaults to enabled when unset).
	isolaTabs := true
	if h.cfg.IsolaTabs != nil {
		isolaTabs = *h.cfg.IsolaTabs
	}
	resp["isola_tabs"] = isolaTabs
	// Session timer flag: when false the client hides countdown/extend and is
	// expected to terminate the session when the owner closes the page.
	resp["timer"] = h.cfg.SessionTimer
	// Concurrent-session cap (ISOLA_MAX_SESSIONS). 0 = unlimited. Lets the client
	// explain a 429 instead of showing a bare error.
	resp["max_sessions"] = h.cfg.MaxSessions
	// Network capture window (session.network_log_max_entries), so the viewer
	// trims its buffers to the same number the streamer keeps. Omitted when
	// unset; the client falls back to the default.
	if h.cfg.NetworkLogMaxEntries > 0 {
		resp["network_log_max_entries"] = h.cfg.NetworkLogMaxEntries
	}
	// Tor exit-country default (tor.exit_country), so a UI can prefill it. Omitted
	// when unset; a per-session tor_country still overrides it.
	if h.cfg.TorExitCountry != "" {
		resp["tor_exit_country"] = h.cfg.TorExitCountry
	}
	plugins := h.pluginConfig()
	if len(plugins) > 0 {
		resp["plugins"] = plugins
	}
	resp["version"] = version.Version
	resp["commit"] = version.Commit
	resp["build_time"] = version.BuildTime
	if len(h.cfg.APITokens) > 0 {
		resp["create_auth_required"] = true
	}
	respondJSON(w, http.StatusOK, resp)
}

func (h *Handler) CreateSession(w http.ResponseWriter, r *http.Request) {
	if err := h.authorizedForSessionCreate(r); err != nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	// Every session is anonymous.
	userID := uuid.Nil

	var req models.CreateSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_request_body")
		return
	}
	if req.URL == "" {
		respondError(w, http.StatusBadRequest, "url_is_required")
		return
	}
	req.URL = normalizeOrSearch(req.URL)
	// Normalize: prepend https:// if no protocol
	if !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://") {
		req.URL = "https://" + req.URL
	}
	var validatedTarget *validatedSessionTarget
	if h.cfg.DisableURLValidation {
		parsed, _ := url.Parse(req.URL)
		hostname := ""
		if parsed != nil {
			hostname = parsed.Hostname()
		}
		validatedTarget = &validatedSessionTarget{PinnedHost: hostname}
	} else {
		var err error
		validatedTarget, err = validateSessionURL(req.URL)
		if err != nil {
			host := ""
			if parsed, perr := url.Parse(req.URL); perr == nil {
				host = parsed.Hostname()
			}
			log.Printf("create session: reject host %q: %v", host, err)
			respondErrorMessage(w, http.StatusBadRequest, "invalid_url", err.Error())
			return
		}
	}
	if req.Resolution == "" {
		req.Resolution = h.cfg.Resolution
	}
	// Cap the launch resolution before it reaches the container: the capture
	// framebuffer is w*h*4 bytes, and an oversized one OOM-kills the sandbox
	// as soon as a viewer connects (see Config.ClampResolution).
	req.Resolution = h.cfg.ClampResolution(req.Resolution)

	if req.BrowserType == "" {
		req.BrowserType = h.cfg.BrowserType
	}

	// Validate browser type
	if !h.cfg.IsBrowserEnabled(req.BrowserType) {
		respondErrorMessage(w, http.StatusBadRequest, "unsupported_browser_type", fmt.Sprintf("Unsupported or disabled browser type: %s (supported: chromium)", req.BrowserType))
		return
	}

	// Tor exit-country pinning (optional). Normalize before the value can reach
	// a container; a malformed code is a client error, not silently ignored.
	torCountry, err := config.NormalizeTorCountry(req.TorCountry)
	if err != nil {
		respondErrorMessage(w, http.StatusBadRequest, "invalid_tor_country", err.Error())
		return
	}
	if !req.TorEnabled {
		// A country only means something with Tor on; drop it so the stored
		// session reflects what actually runs.
		torCountry = ""
	} else if torCountry == "" {
		// No per-request pin: fall back to the server default (tor.exit_country).
		torCountry = h.cfg.TorExitCountry
	}
	req.TorCountry = torCountry

	// All sessions use the Isola transport (Electron image).
	req.Transport = "isola"

	// Run session validators (guards + URL policies, composed by main.go from registry)
	if h.ext != nil && h.ext.ValidateSession != nil {
		if err := h.ext.ValidateSession(r.Context(), r, userID, req.Fingerprint, req.URL); err != nil {
			// Rate-limit guards surface as 429; policy rejections (URL
			// allowlist, entitlements) as 403 — the client shouldn't retry
			// a forbidden URL with backoff.
			if strings.HasPrefix(strings.ToLower(err.Error()), "too many requests") {
				respondErrorMessage(w, http.StatusTooManyRequests, "too_many_requests", err.Error())
			} else {
				respondErrorMessage(w, http.StatusForbidden, "forbidden", err.Error())
			}
			return
		}
	}

	// Multiple concurrent anonymous sessions are allowed: a launch no longer
	// tears down the caller's other sessions. The instance-wide cap below
	// (ISOLA_MAX_SESSIONS) is the admission gate instead.

	sessionToken, err := generatePassword(32)
	if err != nil {
		log.Printf("create session: generate session token: %v", err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	// Compute durations — expiry starts when browser is actually running.
	// With the timer disabled, duration 0 means unlimited: ActivateSession
	// leaves expires_at NULL and every cache TTL below skips expiry — the
	// session ends when the owner closes the page or clicks End.
	sessionDuration := h.sessionDuration()

	// Create session in DB (expiry set later when container is ready)
	session := &models.Session{
		ID:          uuid.New(),
		UserID:      userID,
		Status:      models.SessionStatusProvisioning,
		TargetURL:   req.URL,
		Resolution:  req.Resolution,
		FrameRate:   req.FrameRate,
		BrowserType: req.BrowserType,
		TorEnabled:  req.TorEnabled,
		TorCountry:  req.TorCountry,
	}

	// Admission control: refuse once the configured concurrent-session cap is
	// reached. The check and the insert are atomic, so simultaneous launches
	// cannot overshoot the cap. 0 = unlimited.
	if err := h.store.Sessions.CreateSessionIfCapacity(r.Context(), session, h.cfg.MaxSessions); err != nil {
		if errors.Is(err, store.ErrMaxSessionsReached) {
			log.Printf("create session: refused, max sessions (%d) reached", h.cfg.MaxSessions)
			// Tell a waiting client how long to back off before retrying; the UI
			// retries a few times before it gives up.
			w.Header().Set("Retry-After", "5")
			respondError(w, http.StatusTooManyRequests, "max_sessions_reached")
			return
		}
		log.Printf("create session: db insert: %v", err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if err := h.cache.Set(r.Context(), controlTokenKey(session.ID.String()), sessionToken, provisioningTokenTTL(sessionDuration)); err != nil {
		log.Printf("create session: failed to store session token: %v", err)
		h.store.Sessions.TerminateSession(r.Context(), session.ID)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	// Start container asynchronously (use background context since request ctx is canceled after response)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("create session: goroutine panic for %s: %v", session.ID.String(), r)
				h.store.Sessions.UpdateSessionStatus(context.Background(), session.ID, models.SessionStatusError, "", "", 0)
			}
		}()

		bgCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		browserTheme := req.BrowserTheme
		if browserTheme == "" {
			if h.cfg.BrowserTheme == "auto" {
				browserTheme = "dark"
			} else {
				browserTheme = h.cfg.BrowserTheme
			}
		}

		createStart := time.Now()
		effectiveFPS := req.FrameRate
		if effectiveFPS <= 0 {
			effectiveFPS = 30
		}
		info, warmed := h.claimWarmContainer(bgCtx, session.ID.String(), req.URL, req.Resolution, browserTheme, userID.String(), effectiveFPS, req.TorEnabled, req.BandwidthKbps, validatedTarget)
		if !warmed {
			var err error
			info, err = h.orch.CreateSession(bgCtx, orchestrator.CreateSessionParams{
				SessionID:     session.ID.String(),
				UserID:        userID.String(),
				TargetURL:     req.URL,
				Resolution:    req.Resolution,
				BrowserType:   req.BrowserType,
				PinnedHost:    validatedTarget.PinnedHost,
				PinnedIP:      validatedTarget.PinnedIP,
				BrowserTheme:  browserTheme,
				BrowserFlags:  h.cfg.BrowserFlagsFor(req.BrowserType),
				StreamMode:    "isola",
				AudioEnabled:  req.AudioEnabled != nil && *req.AudioEnabled, // default false (toggled online)
				BandwidthKbps: req.BandwidthKbps,
				FrameRate: func() int {
					if req.FrameRate > 0 {
						return req.FrameRate
					}
					return 30
				}(),
				TorEnabled: req.TorEnabled,
				TorCountry: req.TorCountry,
				// For isola transport: always capture network/console/cookie CDP
				// events at creation time so the viewer can toggle the panel on
				// later and still see the full traffic history (container env can't
				// be changed after launch).
				NetworkMonitor: true,
				// Capture window: the streamer drops the oldest entries past this
				// count and the viewer trims to the same number. Baked in at launch
				// because container env cannot change afterwards.
				NetworkLogMaxEntries: h.cfg.NetworkLogMaxEntries,
				DockerImage:          h.cfg.SessionImage(),
				DNSMode:              h.cfg.DNSMode,
				DNSServers:           h.cfg.DNSServers,
			})
			if err != nil {
				log.Printf("create session: orchestrator error: %v", err)
				if err := h.store.Sessions.UpdateSessionStatus(bgCtx, session.ID, models.SessionStatusError, "", "", 0); err != nil {
					log.Printf("create session: failed to update error status: %v", err)
				}
				return
			}
		}
		log.Printf("create session: container ready in %v (warm=%v)", time.Since(createStart), warmed)

		// Wait for stream to be ready before marking session as running. Warm
		// claims skip this: the pool probed the stream right before handing the
		// container over.
		streamMode := "isola"
		if !warmed {
			if err := h.orch.WaitForStream(bgCtx, info.Host, info.Port, streamMode, req.BrowserType, 30*time.Second); err != nil {
				log.Printf("create session: stream not ready: %v", err)
				failSess := *session
				failSess.ContainerID = info.ContainerID
				failSess.ContainerHost = info.Host
				failSess.ContainerPort = info.Port
				h.finalizeSession(bgCtx, &failSess)
				h.store.Sessions.UpdateSessionStatus(bgCtx, session.ID, models.SessionStatusError, "", "", 0)
				return
			}
		}

		// Store in cache for WS proxy BEFORE marking running.
		// This prevents a race: frontend polls DB → sees "running" → connects WS
		// → proxy looks up cache → key missing → 404 → disconnect.
		// Port 5900: isola-streamer HTTP/WebSocket (video + control).
		key := fmt.Sprintf("session:%s", session.ID.String())
		streamAddr := net.JoinHostPort(info.Host, fmt.Sprintf("%d", info.Port))
		h.cache.Set(bgCtx, key, streamAddr, sessionDuration)
		cacheAddr, cacheErr := h.cache.Get(bgCtx, key)
		_ = cacheAddr
		_ = cacheErr

		// Store CDP address when available (separate key for proxy CDP routing)
		if info.CdpPort > 0 {
			cdpKey := fmt.Sprintf("session:%s:cdp", session.ID.String())
			cdpAddr := net.JoinHostPort(info.Host, fmt.Sprintf("%d", info.CdpPort))
			h.cache.Set(bgCtx, cdpKey, cdpAddr, sessionDuration)
		}

		// Store transport type in the cache so GetSession can return it
		transportKey := fmt.Sprintf("session:%s:transport", session.ID.String())
		h.cache.Set(bgCtx, transportKey, streamMode, sessionDuration)

		// Unlimited sessions (timer: false or default_min = 0): the control
		// token must live as long as the session — not just the 10-minute
		// provisioning window. FinalizeRunningSession deletes it when the
		// session ends.
		if sessionDuration <= 0 {
			h.cache.Persist(bgCtx, controlTokenKey(session.ID.String()))
		} else {
			applySessionTTL(bgCtx, h.cache, controlTokenKey(session.ID.String()), provisioningTokenTTL(sessionDuration))
		}

		// Activate session: mark running + set expiry to NOW + sessionDuration
		if err := h.store.Sessions.ActivateSession(bgCtx, session.ID, info.ContainerID, info.Host, info.Port, sessionDuration); err != nil {
			// Clean up the cache key we just set
			h.cache.Del(bgCtx, key, transportKey, controlTokenKey(session.ID.String()))
			log.Printf("create session: activate error: %v", err)
			return
		}

		// Notify extensions (session hooks + audit, composed by main.go from registry)
		if h.ext != nil && h.ext.OnSessionReady != nil {
			h.ext.OnSessionReady(bgCtx, SessionEvent{
				SessionID:   session.ID.String(),
				UserID:      userID.String(),
				ContainerID: info.ContainerID,
				TargetURL:   req.URL,
				BrowserType: req.BrowserType,
				Host:        info.Host,
				Port:        info.Port,
				CdpPort:     info.CdpPort,
			})
		}
	}()

	transport := "isola"

	cdpURL := fmt.Sprintf("/ws/%s?channel=input", session.ID.String())

	setSensitiveNoStore(w)
	respondJSON(w, http.StatusAccepted, models.CreateSessionResponse{
		ID:           session.ID.String(),
		SessionToken: sessionToken,
		WsURL:        fmt.Sprintf("/ws/%s", session.ID.String()),
		CdpURL:       cdpURL,
		BandwidthKbps: func() int {
			if req.BandwidthKbps > 0 {
				return req.BandwidthKbps
			}
			return 8000
		}(),
		Transport: transport,
		Status:    models.SessionStatusProvisioning,
		// ExpiresAt / RemainingMin / ExtendAvailable set once browser is running
	})
}

// claimWarmContainer serves a session from the warm pool when the request
// is eligible and a matching container is available.
//
// Eligibility mirrors what the pool bakes in at container creation, because
// container env and /etc/hosts cannot be changed after launch:
//   - isola transport only (the pool runs the Electron image);
//   - exact resolution match (encoder bitrate is baked from the resolution);
//   - the effective frame rate must match (the cap is baked);
//   - default resolved theme (the window background is baked);
//   - no Tor or explicit bandwidth override.
//
// DNS pinning: the cold path passes the validated IP via --add-host, which a
// running container cannot gain. Warm claims therefore rely on the browser
// container's own egress firewall (it drops every RFC1918/loopback
// destination), which is the primary SSRF control; the pin is a second layer.
// The first such claim logs a warning so operators see the downgrade.
//
// On success the browser is navigated to the session's target URL and the
// container info is returned ready to activate. The second return value
// reports whether the claim succeeded; false means "fall back to cold".
func (h *Handler) claimWarmContainer(
	ctx context.Context,
	sessionID, targetURL, resolution, theme, userID string,
	effectiveFPS int,
	torEnabled bool,
	bandwidthKbps int,
	target *validatedSessionTarget,
) (*orchestrator.SessionInfo, bool) {
	if h.warm == nil || h.warm.Disabled() {
		return nil, false
	}
	// Guards for settings baked at container creation that this request
	// cannot override.
	if torEnabled || bandwidthKbps > 0 {
		return nil, false
	}

	info := h.warm.Claim(ctx, resolution, theme, effectiveFPS)
	if info == nil {
		return nil, false
	}

	if target != nil && target.PinnedIP != "" {
		h.warmPinWarnOnce.Do(func() {
			log.Printf("create session: warm container claimed for a DNS-pinned target (%s); the container egress firewall is the SSRF control for this session, not the pin", target.PinnedHost)
		})
	}

	// Point the browser at the session's URL. On failure the container is
	// unusable as this session — tear it down and take the cold path so the
	// user never lands on a warm-but-wrong page.
	navCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := h.warm.Navigate(navCtx, info.Host, info.Port, targetURL); err != nil {
		log.Printf("create session: warm navigate failed (%v); falling back to cold", err)
		if errors.Is(err, orchestrator.ErrNavigateUnsupported) {
			// The running browser image predates the warm-pool endpoint: stop
			// serving claims so the next user does not pay this fallback too.
			h.warm.NotifyNavigateDisabled()
		}
		if err := h.orch.TerminateSession(context.Background(), info.ContainerID); err != nil {
			log.Printf("create session: warm container teardown failed: %v", err)
		}
		return nil, false
	}
	log.Printf("create session: warm container %s claimed for session %s (user %s, target %s)",
		info.ContainerID[:12], sessionID, userID, targetURL)
	return info, true
}

func (h *Handler) GetSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_session_id")
		return
	}

	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if session == nil {
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}

	ownerAuthorized, err := h.authorizeSessionOwner(r, session)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	// Attach wsUrl and timing info
	type sessionWithMeta struct {
		models.Session
		SessionToken    string `json:"session_token,omitempty"`
		ShareURL        string `json:"share_url,omitempty"`
		WsURL           string `json:"ws_url"`
		RemainingMin    int    `json:"remaining_min"`
		ExtendAvailable bool   `json:"extend_available"`
		MaxExpiresAt    string `json:"max_expires_at,omitempty"`
		ViewerCount     int    `json:"viewer_count"`
		// When true, the sandbox was launched with CDP Network/Runtime capture
		// enabled — the viewer can surface a toggle for the traffic panel.
		// Currently always true for isola transport.
		NetworkMonitor bool `json:"network_monitor"`
	}

	// Populate transport from the cache (set at session activation time)
	transportKey := fmt.Sprintf("session:%s:transport", session.ID.String())
	if t, err := h.cache.Get(r.Context(), transportKey); err == nil {
		session.Transport = t
	} else if session.Transport == "" {
		session.Transport = "isola"
	}

	remainingMin := 0
	extendAvailable := false
	maxExpiry := session.CreatedAt.Add(time.Duration(h.cfg.SessionMaxMin) * time.Minute)
	maxExpiryStr := maxExpiry.Format(time.RFC3339)

	if session.ExpiresAt == nil {
		// Unlimited session (default_min=0): no countdown, no extend.
		remainingMin = -1
		maxExpiryStr = ""
	} else if session.Status == models.SessionStatusRunning {
		remaining := time.Until(*session.ExpiresAt)
		remainingMin = int(remaining.Minutes())
		if remainingMin < 0 {
			remainingMin = 0
		}
		extendAvailable = session.ExpiresAt.Before(maxExpiry)
	}

	sessionToken := ""
	shareURL := ""
	requestHasToken := extractSessionToken(r) != ""
	if ownerAuthorized {
		sessionToken, err = h.getControlToken(r.Context(), session.ID.String())
		if err != nil {
			respondError(w, http.StatusInternalServerError, "internal_error")
			return
		}
	} else if session.UserID == uuid.Nil && session.Status == models.SessionStatusRunning && requestHasToken {
		// Recover a control token that is genuinely gone (the cache entry
		// expired while the session is still running). If a token still exists
		// the caller simply failed to present it, and returning the live token
		// would let anyone who knows the session id take over the session —
		// "knowing a session id is never sufficient" (docs/security.md).
		existing, getErr := h.getControlToken(r.Context(), session.ID.String())
		if getErr != nil {
			respondError(w, http.StatusInternalServerError, "internal_error")
			return
		}
		if existing == "" {
			sessionToken, err = h.ensureControlToken(r.Context(), session)
			if err != nil {
				respondError(w, http.StatusInternalServerError, "internal_error")
				return
			}
		}
	} else if session.Status == models.SessionStatusRunning {
		shareURL, err = h.ensureShareURL(r.Context(), r, session)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "internal_error")
			return
		}
	}

	setSensitiveNoStore(w)
	respondJSON(w, http.StatusOK, sessionWithMeta{
		Session:         *session,
		SessionToken:    sessionToken,
		ShareURL:        shareURL,
		WsURL:           fmt.Sprintf("/ws/%s", session.ID.String()),
		RemainingMin:    remainingMin,
		ExtendAvailable: extendAvailable,
		MaxExpiresAt:    maxExpiryStr,
		ViewerCount:     getViewerCount(r.Context(), h.cache, session.ID.String()),
		// Network capture is always on for Isola (in-viewer panel toggle).
		NetworkMonitor: true,
	})
}

func (h *Handler) DeleteSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_session_id")
		return
	}

	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil || session == nil {
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}
	ownerAuthorized, err := h.authorizeSessionOwner(r, session)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !ownerAuthorized {
		respondError(w, http.StatusForbidden, "session_does_not_belong_to_you")
		return
	}

	// Terminate container + clear caches + hooks (shared with background reapers)
	h.finalizeSession(r.Context(), session)

	respondJSON(w, http.StatusOK, map[string]string{"status": models.SessionStatusTerminated})
}

// finalizeSession stops the container, clears cache, fires hooks, and marks terminated.
func (h *Handler) finalizeSession(ctx context.Context, session *models.Session) {
	h.orch.FinalizeRunningSession(ctx, h.store, h.cache, session)
}

// OnBackgroundSessionEnd is wired into the orchestrator for expiry/idle/health termination.
func (h *Handler) OnBackgroundSessionEnd(ctx context.Context, sess *models.Session) {
	if sess == nil {
		return
	}
	userIDStr := "anonymous"
	if sess.UserID != uuid.Nil {
		userIDStr = sess.UserID.String()
	}
	if h.ext != nil && h.ext.OnSessionDeleted != nil {
		h.ext.OnSessionDeleted(ctx, SessionEvent{
			SessionID:   sess.ID.String(),
			UserID:      userIDStr,
			ContainerID: sess.ContainerID,
			TargetURL:   sess.TargetURL,
			BrowserType: sess.BrowserType,
		})
	}
}

func (h *Handler) ExtendSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_session_id")
		return
	}

	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil || session == nil || session.Status != models.SessionStatusRunning {
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}
	ownerAuthorized, err := h.authorizeSessionOwner(r, session)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !ownerAuthorized {
		respondError(w, http.StatusForbidden, "session_does_not_belong_to_you")
		return
	}

	// Unlimited sessions have no expiry to extend.
	if session.ExpiresAt == nil {
		respondError(w, http.StatusBadRequest, "session_unlimited")
		return
	}

	maxExpiry := session.CreatedAt.Add(time.Duration(h.cfg.SessionMaxMin) * time.Minute)

	// Current expiry or now if already expired
	baseTime := time.Now()
	if session.ExpiresAt.After(baseTime) {
		baseTime = *session.ExpiresAt
	}

	newExpiry := baseTime.Add(time.Duration(h.cfg.SessionExtendMin) * time.Minute)

	// Cap at max expiry
	if newExpiry.After(maxExpiry) {
		newExpiry = maxExpiry
	}

	if err := h.store.Sessions.ExtendExpiry(r.Context(), id, newExpiry, maxExpiry); err != nil {
		respondError(w, http.StatusInternalServerError, "extend_failed")
		return
	}

	// Update the cache TTL
	remaining := time.Until(newExpiry)
	if remaining > 0 {
		applySessionTTL(r.Context(), h.cache, fmt.Sprintf("session:%s", session.ID.String()), remaining)
		applySessionTTL(r.Context(), h.cache, fmt.Sprintf("session:%s:transport", session.ID.String()), remaining)
		applySessionTTL(r.Context(), h.cache, controlTokenKey(session.ID.String()), remaining)
	}

	remainingMin := int(remaining.Minutes())
	if remainingMin < 0 {
		remainingMin = 0
	}

	respondJSON(w, http.StatusOK, models.ExtendSessionResponse{
		Status:          string(session.Status),
		ExpiresAt:       newExpiry.Format(time.RFC3339),
		MaxExpiresAt:    maxExpiry.Format(time.RFC3339),
		RemainingMin:    remainingMin,
		ExtendAvailable: newExpiry.Before(maxExpiry),
		ViewerCount:     getViewerCount(r.Context(), h.cache, session.ID.String()),
	})
}

// ClampResizeRequest clamps a requested (width, height) to the session's
// launch resolution, returning the effective values plus a "clamped" flag.
//
// Exposed so it can be unit-tested independently of HTTP plumbing.
func ClampResizeRequest(reqWidth, reqHeight int, sessionResolution string) (int, int, bool, error) {
	if reqWidth < 64 || reqHeight < 64 {
		return 0, 0, false, fmt.Errorf("width and height must be at least 64")
	}

	maxWidth, maxHeight := reqWidth, reqHeight
	if sessionResolution == "" {
		return 0, 0, false, fmt.Errorf("session has no launch resolution recorded")
	}
	if _, err := fmt.Sscanf(sessionResolution, "%dx%d", &maxWidth, &maxHeight); err != nil {
		return 0, 0, false, fmt.Errorf("invalid session resolution %q", sessionResolution)
	}

	clamped := false
	if reqWidth > maxWidth {
		reqWidth = maxWidth
		clamped = true
	}
	if reqHeight > maxHeight {
		reqHeight = maxHeight
		clamped = true
	}
	return reqWidth, reqHeight, clamped, nil
}

// ResizeSession handles POST /api/sessions/{id}/resize.
func (h *Handler) ResizeSession(w http.ResponseWriter, r *http.Request) {
	session, ok := h.ownedRunningSession(w, r)
	if !ok {
		return
	}
	if session.ContainerHost == "" || session.ContainerPort == 0 {
		respondError(w, http.StatusConflict, "container_not_ready")
		return
	}

	var req struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_request_body")
		return
	}

	effectiveW, effectiveH, clamped, clampErr := ClampResizeRequest(req.Width, req.Height, session.Resolution)
	if clampErr != nil {
		respondErrorMessage(w, http.StatusBadRequest, "invalid_resolution", clampErr.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.orch.ResizeSessionDisplay(ctx, session.ContainerHost, session.ContainerPort, effectiveW, effectiveH); err != nil {
		log.Printf("resize session: orchestrator error: %v", err)
		respondError(w, http.StatusInternalServerError, "resize_failed")
		return
	}

	if clamped {
		log.Printf("resize session: clamped %dx%d -> %dx%d (session launch resolution %s)",
			req.Width, req.Height, effectiveW, effectiveH, session.Resolution)
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"requested_width":  req.Width,
		"requested_height": req.Height,
		"width":            effectiveW,
		"height":           effectiveH,
		"clamped":          clamped,
	})
}

// GetResourceGraph handles GET /api/sessions/{id}/graph.png.
//
// The graph is rendered by the streamer, which holds the network log, and this
// handler only proxies it: the control plane never has the captured requests.
// Query parameters (theme, direction, expand, type, failed, scale, width,
// height, origin, caption) are passed through untouched; the streamer owns
// their validation and returns 400 with a reason when one is invalid.
func (h *Handler) GetResourceGraph(w http.ResponseWriter, r *http.Request) {
	session, ok := h.ownedRunningSession(w, r)
	if !ok {
		return
	}

	addr, err := h.cache.Get(r.Context(), fmt.Sprintf("session:%s", session.ID.String()))
	if err != nil || addr == "" {
		respondError(w, http.StatusConflict, "container_not_ready")
		return
	}

	upstream := "http://" + addr + "/graph.png"
	if r.URL.RawQuery != "" {
		upstream += "?" + r.URL.RawQuery
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("resource graph: streamer request: %v", err)
		respondError(w, http.StatusBadGateway, "graph_render_failed")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		reason, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		respondErrorMessage(w, http.StatusBadRequest, "invalid_graph_options", strings.TrimSpace(string(reason)))
		return
	}

	// The image is a snapshot of a live capture: never cache it.
	w.Header().Set("Content-Type", "image/png")
	setSensitiveNoStore(w)
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, resp.Body); err != nil {
		log.Printf("resource graph: copy body: %v", err)
	}
}

// ToggleAudio handles POST /api/sessions/{id}/audio to toggle Opus audio capture on/off.
func (h *Handler) ToggleAudio(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		log.Printf("audio toggle: bad session id: %v", err)
		respondError(w, http.StatusBadRequest, "invalid_session_id")
		return
	}

	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil || session == nil || session.Status != models.SessionStatusRunning {
		ss := "nil"
		if session != nil {
			ss = string(session.Status)
		}
		log.Printf("audio toggle: session not found or not running (err=%v, nil=%v, status=%s)", err, session == nil, ss)
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}

	// Populate transport from the cache (mirrors GetSession's lookup — the
	// Transport field is stored in the cache at session activation time, not in
	// the DB row).
	if session.Transport == "" {
		transportKey := fmt.Sprintf("session:%s:transport", session.ID.String())
		if t, err := h.cache.Get(r.Context(), transportKey); err == nil {
			session.Transport = t
		}
	}
	ownerAuthorized, err := h.authorizeSessionOwner(r, session)
	if err != nil {
		log.Printf("audio toggle: authorizeSessionOwner error: %v", err)
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !ownerAuthorized {
		log.Printf("audio toggle: owner not authorized for session %v", session.UserID)
		respondError(w, http.StatusForbidden, "session_does_not_belong_to_you")
		return
	}
	if session.Transport != "" && session.Transport != "isola" {
		log.Printf("audio toggle: transport is %q, not isola", session.Transport)
		respondError(w, http.StatusBadRequest, "audio_unsupported")
		return
	}
	if session.ContainerHost == "" || session.ContainerPort == 0 {
		log.Printf("audio toggle: container not ready (host=%q, port=%d)", session.ContainerHost, session.ContainerPort)
		respondError(w, http.StatusConflict, "container_not_ready")
		return
	}

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Printf("audio toggle: json decode error: %v", err)
		respondError(w, http.StatusBadRequest, "invalid_request_body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := h.orch.AudioToggle(ctx, session.ContainerHost, session.ContainerPort, req.Enabled); err != nil {
		log.Printf("audio toggle: orchestrator error: %v", err)
		respondError(w, http.StatusInternalServerError, "audio_toggle_failed")
		return
	}

	log.Printf("audio toggle: session %s audio %s", id.String()[:8], map[bool]string{true: "enabled", false: "disabled"}[req.Enabled])
	respondJSON(w, http.StatusOK, map[string]any{"audio_enabled": req.Enabled})
}

// GetOpenAPISpec serves the OpenAPI 3.0 specification for the API.
func (h *Handler) GetOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	spec := getOpenAPISpec()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	w.Write(spec)
}

// CreateShare generates a secure share token for a view-only session link.
func (h *Handler) CreateShare(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_session_id")
		return
	}

	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil || session == nil || session.Status != models.SessionStatusRunning {
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}

	ownerAuthorized, err := h.authorizeSessionOwner(r, session)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !ownerAuthorized {
		respondError(w, http.StatusForbidden, "session_does_not_belong_to_you")
		return
	}

	// Generate random 16-byte token (32 hex chars)
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		respondError(w, http.StatusInternalServerError, "failed_to_generate_share_token")
		return
	}
	token := hex.EncodeToString(tokenBytes)

	// Store in the cache: share:{token} → sessionID, TTL = remaining session time
	remaining := shareTTL(session)
	now := time.Now()
	key := shareTokenKey(token)
	h.cache.Set(r.Context(), key, session.ID.String(), remaining)

	// Store share metadata in a hash so we can list/revoke shares per session
	shareMetaKey := shareMetaKey(token)
	h.cache.HSet(r.Context(), shareMetaKey,
		"created_at", now.Format(time.RFC3339),
		"expires_at", now.Add(remaining).Format(time.RFC3339),
	)
	h.cache.Expire(r.Context(), shareMetaKey, remaining)

	// Add token to session's shares set for listing
	sessionSharesKey := fmt.Sprintf("session:%s:shares", session.ID.String())
	h.cache.SAdd(r.Context(), sessionSharesKey, token)
	h.cache.Expire(r.Context(), sessionSharesKey, remaining)

	shareURL := fmt.Sprintf("%s/share/%s", publicAppBaseURL(r, h.cfg.CORSOrigin), token)

	setSensitiveNoStore(w)
	respondJSON(w, http.StatusCreated, models.ShareResponse{
		Token:    token,
		ShareURL: shareURL,
	})
}

// ListShares returns all active share tokens for a session.
func (h *Handler) ListShares(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_session_id")
		return
	}

	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil || session == nil {
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}

	ownerAuthorized, err := h.authorizeSessionOwner(r, session)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !ownerAuthorized {
		respondError(w, http.StatusForbidden, "session_does_not_belong_to_you")
		return
	}

	sessionSharesKey := fmt.Sprintf("session:%s:shares", session.ID.String())
	tokens, err := h.cache.SMembers(r.Context(), sessionSharesKey)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	shares := make([]models.ShareInfo, 0, len(tokens))
	baseURL := publicAppBaseURL(r, h.cfg.CORSOrigin)
	for _, token := range tokens {
		si := models.ShareInfo{Token: token, ShareURL: fmt.Sprintf("%s/share/%s", baseURL, token)}
		// Attempt to get metadata
		meta, metaErr := h.cache.HGetAll(r.Context(), shareMetaKey(token))
		if metaErr == nil {
			if t, err := time.Parse(time.RFC3339, meta["created_at"]); err == nil {
				si.CreatedAt = t
			}
			if t, err := time.Parse(time.RFC3339, meta["expires_at"]); err == nil {
				si.ExpiresAt = t
			}
		}
		shares = append(shares, si)
	}

	respondJSON(w, http.StatusOK, models.ListSharesResponse{Shares: shares})
}

// RevokeShare deletes a specific share token for a session.
func (h *Handler) RevokeShare(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_session_id")
		return
	}
	token := chi.URLParam(r, "token")
	if token == "" {
		respondError(w, http.StatusBadRequest, "missing_share_token")
		return
	}

	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil || session == nil {
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}

	ownerAuthorized, err := h.authorizeSessionOwner(r, session)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}
	if !ownerAuthorized {
		respondError(w, http.StatusForbidden, "session_does_not_belong_to_you")
		return
	}

	// Remove share token from the cache
	h.cache.Del(r.Context(), shareTokenKey(token), shareMetaKey(token))
	sessionSharesKey := fmt.Sprintf("session:%s:shares", session.ID.String())
	h.cache.SRem(r.Context(), sessionSharesKey, token)

	respondJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

// GetShare resolves a share token and returns view-only session info.
func (h *Handler) GetShare(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		respondError(w, http.StatusBadRequest, "missing_share_token")
		return
	}

	sessionID, err := h.cache.Get(r.Context(), shareTokenKey(token))
	if store.IsKeyNotFound(err) {
		respondError(w, http.StatusNotFound, "share_invalid")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return
	}

	id, err := uuid.Parse(sessionID)
	if err != nil {
		respondError(w, http.StatusNotFound, "invalid_session")
		return
	}

	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil || session == nil {
		respondError(w, http.StatusNotFound, "session_not_found")
		return
	}

	if session.Status != models.SessionStatusRunning {
		respondError(w, http.StatusGone, "session_gone")
		return
	}

	// Look up transport from the cache (set at session activation time)
	transport := "isola"
	if t, err := h.cache.Get(r.Context(), fmt.Sprintf("session:%s:transport", session.ID.String())); err == nil && t != "" {
		transport = t
	}

	setSensitiveNoStore(w)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"id":           session.ID.String(),
		"target_url":   session.TargetURL,
		"vnc_password": "",
		"status":       session.Status,
		"expires_at":   session.ExpiresAt,
		"transport":    transport,
	})
}

// extractSessionToken reads the control token from the X-Session-Token header.
// Query-string tokens are deliberately NOT accepted for HTTP API calls: URLs
// leak through proxy access logs, browser history and Referer headers. (The
// WebSocket handshake is the one exception — browsers cannot set custom
// headers on WebSocket() — and is handled separately in the ws proxy.)
func extractSessionToken(r *http.Request) string {
	return strings.TrimSpace(r.Header.Get("X-Session-Token"))
}

func normalizeOrSearch(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return trimmed
	}
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed
	}
	if strings.ContainsAny(trimmed, " \t\n\r") {
		return "https://duckduckgo.com/?q=" + url.QueryEscape(trimmed)
	}
	hostOnly := strings.SplitN(trimmed, "/", 2)[0]
	hostOnly = strings.SplitN(hostOnly, "?", 2)[0]
	hostOnly = strings.SplitN(hostOnly, "#", 2)[0]
	if strings.HasSuffix(strings.ToLower(hostOnly), "localhost") ||
		(len(hostOnly) >= 9 && strings.EqualFold(hostOnly[:9], "localhost")) ||
		strings.HasPrefix(strings.ToLower(hostOnly), "localhost:") {
		return trimmed
	}
	if net.ParseIP(hostOnly) != nil {
		return trimmed
	}
	if idx := strings.LastIndex(hostOnly, ":"); idx > 0 {
		if ip := net.ParseIP(hostOnly[:idx]); ip != nil {
			return trimmed
		}
	}
	lastDot := strings.LastIndex(hostOnly, ".")
	if lastDot > 0 && lastDot < len(hostOnly)-2 {
		tld := hostOnly[lastDot+1:]
		hasDigit := false
		for _, r := range tld {
			if r >= '0' && r <= '9' {
				hasDigit = true
				break
			}
		}
		if !hasDigit && len(tld) >= 2 {
			allLetter := true
			for _, r := range tld {
				if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
					allLetter = false
					break
				}
			}
			if allLetter {
				return trimmed
			}
		}
	}
	return "https://duckduckgo.com/?q=" + url.QueryEscape(trimmed)
}

// validateSessionURL checks that a URL is safe to open in a remote browser session.
func validateSessionURL(rawURL string) (*validatedSessionTarget, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL format")
	}
	// Only allow http and https
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("only http and https URLs are allowed")
	}
	// Reject empty host
	if parsed.Host == "" {
		return nil, fmt.Errorf("URL must include a hostname")
	}

	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		if isInternalIP(ip) {
			return nil, fmt.Errorf("internal IP addresses are not allowed")
		}
		return &validatedSessionTarget{}, nil
	}

	addrs, err := net.LookupIP(host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("hostname must resolve to a public IP address")
	}
	var pinnedIP string
	for _, addr := range addrs {
		if isInternalIP(addr) {
			return nil, fmt.Errorf("URL resolves to an internal IP address")
		}
		if pinnedIP == "" {
			pinnedIP = addr.String()
		}
	}
	if pinnedIP == "" {
		return nil, fmt.Errorf("hostname must resolve to a public IP address")
	}
	return &validatedSessionTarget{PinnedHost: host, PinnedIP: pinnedIP}, nil
}

// passwordAlphabet is the character set for generated secrets. Alphanumeric
// only so tokens are URL- and header-safe. A 32-character session token
// carries ~190 bits of entropy.
const passwordAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// generatePassword returns a cryptographically random string of the requested
// length drawn from passwordAlphabet (unbiased rejection sampling via rand.Int).
func generatePassword(length int) (string, error) {
	if length <= 0 {
		return "", fmt.Errorf("invalid password length %d", length)
	}
	alphabetSize := big.NewInt(int64(len(passwordAlphabet)))
	out := make([]byte, length)
	for i := range out {
		n, err := rand.Int(rand.Reader, alphabetSize)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}

type validatedSessionTarget struct {
	PinnedHost string
	PinnedIP   string
}

func controlTokenKey(sessionID string) string {
	return fmt.Sprintf("session:%s:control_token", sessionID)
}

func shareTokenKey(token string) string {
	return "share:" + token
}

func shareMetaKey(token string) string {
	return "share:" + token + ":meta"
}

func sessionSharesKey(sessionID string) string {
	return fmt.Sprintf("session:%s:shares", sessionID)
}

// sessionDuration returns the initial session lifetime. With session.timer
// set to false it is 0 (unlimited) regardless of default_min.
func (h *Handler) sessionDuration() time.Duration {
	if !h.cfg.SessionTimer {
		return 0
	}
	return time.Duration(h.cfg.SessionDefaultMin) * time.Minute
}

// provisioningTokenTTL is how long control/view tokens live: the session
// lifetime plus a provisioning buffer. Minimum 10 minutes — the token must
// survive the provisioning window.
func provisioningTokenTTL(sessionDuration time.Duration) time.Duration {
	if sessionDuration <= 0 {
		return 10 * time.Minute
	}
	return sessionDuration + 10*time.Minute
}

func applySessionTTL(ctx context.Context, cache *store.MemCache, key string, ttl time.Duration) {
	if ttl <= 0 {
		cache.Persist(ctx, key)
		return
	}
	cache.Expire(ctx, key, ttl)
}

// unlimitedShareTTL bounds share lifetime for sessions without an expiry
// (session.timer: false or default_min = 0): the share dies with its host
// session, but since that has no expiry of its own the keys need a ceiling.
const unlimitedShareTTL = 24 * time.Hour

// shareTTL is how long share keys live: the session's remaining life (floored
// at a minute), or a fixed ceiling when the session itself is unlimited.
func shareTTL(session *models.Session) time.Duration {
	if session.ExpiresAt == nil {
		return unlimitedShareTTL
	}
	remaining := time.Until(*session.ExpiresAt)
	if remaining < time.Minute {
		return time.Minute
	}
	return remaining
}

func publicAppBaseURL(r *http.Request, configuredOrigin string) string {
	configured := config.Origins(configuredOrigin)
	if len(configured) > 0 {
		if reqOrigin := requestOrigin(r); reqOrigin != "" {
			for _, origin := range configured {
				if origin == reqOrigin {
					return origin
				}
			}
		}
		return configured[0]
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s", scheme, r.Host)
}

func (h *Handler) ensureShareURL(ctx context.Context, r *http.Request, session *models.Session) (string, error) {
	if session == nil || session.Status != models.SessionStatusRunning {
		return "", nil
	}

	baseURL := publicAppBaseURL(r, h.cfg.CORSOrigin)
	tokens, err := h.cache.SMembers(ctx, sessionSharesKey(session.ID.String()))
	if err == nil {
		now := time.Now()
		for _, token := range tokens {
			if strings.TrimSpace(token) == "" {
				continue
			}
			sessionID, getErr := h.cache.Get(ctx, shareTokenKey(token))
			if getErr != nil || sessionID != session.ID.String() {
				continue
			}
			meta, metaErr := h.cache.HGetAll(ctx, shareMetaKey(token))
			if metaErr == nil {
				if expiresAt, parseErr := time.Parse(time.RFC3339, meta["expires_at"]); parseErr == nil && expiresAt.After(now) {
					return fmt.Sprintf("%s/share/%s", baseURL, token), nil
				}
			}
			return fmt.Sprintf("%s/share/%s", baseURL, token), nil
		}
	}

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tokenBytes)

	remaining := shareTTL(session)
	now := time.Now()
	h.cache.Set(ctx, shareTokenKey(token), session.ID.String(), remaining)
	metaKey := shareMetaKey(token)
	h.cache.HSet(ctx, metaKey,
		"created_at", now.Format(time.RFC3339),
		"expires_at", now.Add(remaining).Format(time.RFC3339),
	)
	h.cache.Expire(ctx, metaKey, remaining)
	sharesKey := sessionSharesKey(session.ID.String())
	h.cache.SAdd(ctx, sharesKey, token)
	h.cache.Expire(ctx, sharesKey, remaining)

	return fmt.Sprintf("%s/share/%s", baseURL, token), nil
}

func requestOrigin(r *http.Request) string {
	if origin := config.NormalizeOrigin(r.Header.Get("Origin")); origin != "" {
		return origin
	}
	if ref := strings.TrimSpace(r.Referer()); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
			return config.NormalizeOrigin(u.Scheme + "://" + u.Host)
		}
	}
	return ""
}

func isInternalIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func (h *Handler) getControlToken(ctx context.Context, sessionID string) (string, error) {
	token, err := h.cache.Get(ctx, controlTokenKey(sessionID))
	if store.IsKeyNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return token, nil
}

// ensureControlToken returns a valid control token, creating one if the cache
// entry expired while the session is still running.
func (h *Handler) ensureControlToken(ctx context.Context, session *models.Session) (string, error) {
	token, err := h.getControlToken(ctx, session.ID.String())
	if err != nil {
		return "", err
	}
	if token != "" {
		return token, nil
	}
	if session.UserID != uuid.Nil || session.Status != models.SessionStatusRunning {
		return "", nil
	}
	token, err = generatePassword(32)
	if err != nil {
		return "", err
	}
	// Unlimited session: persist the token — it must remain valid for the
	// whole session, which ends only when the owner leaves (or an admin or
	// reaper finalizes it, deleting the key).
	ttl := time.Duration(0) // 0 = no expiry for Cache.Set
	if session.ExpiresAt != nil {
		ttl = time.Until(*session.ExpiresAt) + 10*time.Minute
		if ttl < 10*time.Minute {
			ttl = 10 * time.Minute
		}
	}
	if err := h.cache.Set(ctx, controlTokenKey(session.ID.String()), token, ttl); err != nil {
		return "", err
	}
	return token, nil
}

// ownedRunningSession resolves the {id} path parameter to a running session the
// caller owns. It writes the error response itself, so a handler only has to
// return when ok is false.
func (h *Handler) ownedRunningSession(w http.ResponseWriter, r *http.Request) (*models.Session, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid_session_id")
		return nil, false
	}
	session, err := h.store.Sessions.GetSession(r.Context(), id)
	if err != nil || session == nil || session.Status != models.SessionStatusRunning {
		respondError(w, http.StatusNotFound, "session_not_found")
		return nil, false
	}
	ownerAuthorized, err := h.authorizeSessionOwner(r, session)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "internal_error")
		return nil, false
	}
	if !ownerAuthorized {
		respondError(w, http.StatusForbidden, "session_does_not_belong_to_you")
		return nil, false
	}
	return session, true
}

func (h *Handler) authorizeSessionOwner(r *http.Request, session *models.Session) (bool, error) {
	token := extractSessionToken(r)
	if token == "" {
		return false, nil
	}

	expected, err := h.getControlToken(r.Context(), session.ID.String())
	if err != nil {
		return false, err
	}
	if expected == "" {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1, nil
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("encode error: %v", err)
	}
}

func setSensitiveNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func respondError(w http.ResponseWriter, status int, code string) {
	respondErrorMessage(w, status, code, errorMessageForCode(code))
}

// respondErrorMessage is respondError for a code whose message must carry detail
// the code cannot express (a validation reason, a guard's own text). The code
// stays a stable snake_case token clients match on; only the message varies.
func respondErrorMessage(w http.ResponseWriter, status int, code, message string) {
	respondJSON(w, status, models.ErrorResponse{
		Error: models.APIError{
			Code:    code,
			Message: message,
		},
	})
}

// errorMessageForCode returns a human-readable message for an error code.
// Falls back to the code itself if no mapping exists.
func errorMessageForCode(code string) string {
	switch code {
	case "internal_error":
		return "An internal error occurred"
	case "invalid_session_id":
		return "Invalid session ID format"
	case "session_not_found":
		return "Session not found"
	case "invalid_request_body":
		return "Invalid request body"
	case "missing_authorization":
		return "Missing authorization header"
	case "invalid_token":
		return "Invalid or expired token"
	case "session_does_not_belong_to_you":
		return "Session does not belong to you"
	case "failed_to_generate_share_token":
		return "Failed to generate share token"
	case "missing_share_token":
		return "Missing share token parameter"
	case "url_is_required":
		return "URL is required"
	case "unsupported_browser_type":
		return "Unsupported or disabled browser type"
	case "too_many_requests":
		return "Too many requests"
	case "max_sessions_reached":
		return "Maximum concurrent sessions reached"
	case "unauthorized":
		return "Unauthorized"
	case "forbidden":
		return "Forbidden"
	case "invalid_url":
		return "The requested URL is not allowed"
	case "invalid_resolution":
		return "Resolution is invalid or exceeds the session's launch resolution"
	case "invalid_tor_country":
		return "Tor exit country is invalid"
	case "session_unlimited":
		return "Session is unlimited — nothing to extend"
	case "extend_failed":
		return "Failed to extend session"
	case "resize_failed":
		return "Failed to resize session"
	case "container_not_ready":
		return "Session container is not ready"
	case "graph_render_failed":
		return "Failed to render the resource graph"
	case "invalid_graph_options":
		return "Invalid resource graph options"
	case "audio_toggle_failed":
		return "Failed to toggle audio"
	case "audio_unsupported":
		return "Audio toggle is only supported for isola sessions"
	case "share_invalid":
		return "Share link expired or invalid"
	case "invalid_session":
		return "Invalid session"
	case "session_gone":
		return "Session no longer active"
	default:
		return code
	}
}

// authorizedForSessionCreate gates container spawn when api.token is configured.
func (h *Handler) authorizedForSessionCreate(r *http.Request) error {
	if len(h.cfg.APITokens) == 0 {
		return nil
	}
	if isomw.CheckBearerAny(r, h.cfg.APITokens) {
		return nil
	}
	return errors.New("unauthorized")
}

// pluginConfig delegates to the extension point if set, otherwise returns nil.
func (h *Handler) pluginConfig() []map[string]interface{} {
	if h.ext != nil && h.ext.PluginConfig != nil {
		return h.ext.PluginConfig()
	}
	return nil
}

func getViewerCount(ctx context.Context, cache *store.MemCache, sessionID string) int {
	val, err := store.GetInt(cache, ctx, fmt.Sprintf("viewer_count:%s", sessionID))
	if err != nil {
		return 0
	}
	return val
}
