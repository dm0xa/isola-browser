package models

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Session status constants.
const (
	SessionStatusProvisioning = "provisioning"
	SessionStatusRunning      = "running"
	SessionStatusTerminated   = "terminated"
	SessionStatusError        = "error"
)

// Session represents a remote browser session.
type Session struct {
	ID             uuid.UUID  `json:"id"`
	UserID         uuid.UUID  `json:"user_id"`
	Status         string     `json:"status"`
	ContainerID    string     `json:"container_id,omitempty"`
	ContainerHost  string     `json:"container_host,omitempty"`
	ContainerPort  int        `json:"container_port,omitempty"`
	TargetURL      string     `json:"target_url"`
	Resolution     string     `json:"resolution"`
	BrowserType    string     `json:"browser_type"`
	Transport      string     `json:"transport,omitempty"`  // always "isola"
	FrameRate      int        `json:"frame_rate,omitempty"` // capture framerate the session was created with
	VncPassword    string     `json:"vnc_password"`
	TorEnabled     bool       `json:"tor_enabled"`           // session-level Tor proxy flag (not a browser type)
	TorCountry     string     `json:"tor_country,omitempty"` // pinned Tor exit country/countries, when tor_enabled
	Timezone       string     `json:"timezone,omitempty"`    // page-visible IANA zone the session launched with
	Locale         string     `json:"locale,omitempty"`      // page-visible BCP-47 tag the session launched with
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	TerminatedAt   *time.Time `json:"terminated_at,omitempty"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
	// ErrorCode is the stable snake_case reason a launch failed, set only when
	// Status is error (sandbox_publish_unreachable, stream_not_ready,
	// create_failed). Absent on healthy sessions.
	ErrorCode string `json:"error_code,omitempty"`
}

// CreateSessionRequest is the payload for POST /api/sessions.
type CreateSessionRequest struct {
	URL            string `json:"url"`
	Resolution     string `json:"resolution"`
	BrowserType    string `json:"browser_type"`
	BrowserTheme   string `json:"browser_theme,omitempty"`
	Transport      string `json:"transport,omitempty"`      // ignored; always isola
	AudioEnabled   *bool  `json:"audio_enabled,omitempty"`  // Opus audio capture
	BandwidthKbps  int    `json:"bandwidth_kbps,omitempty"` // encoder bitrate cap (kbps)
	FrameRate      int    `json:"frame_rate,omitempty"`     // capture framerate (default 30)
	TorEnabled     bool   `json:"tor_enabled"`
	TorCountry     string `json:"tor_country,omitempty"`     // pinned Tor exit country/countries (ISO 3166-1 alpha-2); requires tor_enabled
	Timezone       string `json:"timezone,omitempty"`        // page-visible IANA zone (e.g. Europe/Stockholm); empty = deployment default or UTC
	Locale         string `json:"locale,omitempty"`          // page-visible BCP-47 tag (e.g. sv-SE); empty = deployment default or en-US
	NetworkMonitor bool   `json:"network_monitor,omitempty"` // isola: stream CDP network events to the viewer
	Fingerprint    string `json:"fingerprint,omitempty"`     // client fingerprint for rate limiting
}

// SessionSeed is custom data pushed into a running session's page: cookies,
// extra request headers and web storage. It is a request-only type — never
// persisted on the Session row, never echoed back.
type SessionSeed struct {
	Cookies []SeedCookie  `json:"cookies,omitempty"`
	Headers []SeedHeader  `json:"headers,omitempty"`
	Storage []SeedStorage `json:"storage,omitempty"`
	// Reload reloads the page after applying so a cookie-dependent SPA boot or
	// a header change takes effect in the same call. Never implied.
	Reload bool `json:"reload,omitempty"`
}

// SeedCookie follows CDP's cookie shape. At least one of Domain or URL is
// required; the backend derives URL from the session target when both are
// empty, and Domain wins when both are set. Cookies are always set as domain
// cookies, so __Host-/__Secure- names are rejected.
type SeedCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	URL      string `json:"url,omitempty"`
	Path     string `json:"path,omitempty"`
	SameSite string `json:"same_site,omitempty"` // strict | lax | none, case-insensitive
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"http_only,omitempty"`
	Expires  int64  `json:"expires,omitempty"` // seconds since epoch; 0 = session cookie
}

// SeedHeader is an extra request header. Domains limits it to matching hosts
// (suffix match, compared against the page's current main-frame host); empty
// applies it to whichever host the session is on. The header set itself is
// tab-wide: Chromium sends it on every request that tab makes, including
// third-party subresources.
type SeedHeader struct {
	Name    string   `json:"name"`
	Value   string   `json:"value"`
	Domains []string `json:"domains,omitempty"`
}

// SeedStorage seeds web storage for one origin. Kind is "local" (default,
// survives reloads and new tabs) or "session" (this tab, survives a reload
// via an injected init script).
type SeedStorage struct {
	Kind   string            `json:"kind,omitempty"`
	Origin string            `json:"origin"`
	Items  map[string]string `json:"items"`
}

// FillDefaults supplies what a caller may omit: a cookie with neither domain
// nor url gets the session target's origin, so "set this cookie on the site I
// opened" needs only name and value. A missing name is caught here so the
// error is indexed before the round trip; every other rule — caps, control
// characters, same_site/secure consistency — is the streamer's validation.
// A seed with nothing to apply and no reload is rejected because it would be
// a wasted request; a reload on its own is a legitimate call (the page's state
// is already seeded from an earlier call).
//
// Both the REST handler and the MCP tool call this so the two surfaces accept
// exactly the same payloads.
func (s *SessionSeed) FillDefaults(targetURL string) error {
	if s == nil {
		return fmt.Errorf("seed is required")
	}
	var target *url.URL
	if targetURL != "" {
		if u, err := url.Parse(targetURL); err == nil && u.Host != "" {
			target = u
		}
	}
	for i := range s.Cookies {
		c := &s.Cookies[i]
		if strings.TrimSpace(c.Name) == "" {
			return fmt.Errorf("cookies[%d]: name is required", i)
		}
		if strings.TrimSpace(c.Domain) == "" && strings.TrimSpace(c.URL) == "" {
			if target == nil {
				return fmt.Errorf("cookies[%d]: domain or url is required", i)
			}
			c.URL = target.Scheme + "://" + target.Host
		}
	}
	if len(s.Cookies) == 0 && len(s.Headers) == 0 && len(s.Storage) == 0 && !s.Reload {
		return fmt.Errorf("seed is empty")
	}
	return nil
}

// SeedResult reports what the streamer applied.
type SeedResult struct {
	Cookies  int  `json:"cookies"`
	Headers  int  `json:"headers"`
	Storage  int  `json:"storage"`
	Reloaded bool `json:"reloaded"`
}

// CreateSessionResponse is returned after successful session creation.
type CreateSessionResponse struct {
	ID              string `json:"id"`
	SessionToken    string `json:"session_token,omitempty"`
	ShareURL        string `json:"share_url,omitempty"`
	WsURL           string `json:"ws_url"`
	CdpURL          string `json:"cdp_url,omitempty"` // CDP WebSocket URL when CDP port available
	BandwidthKbps   int    `json:"bandwidth_kbps,omitempty"`
	Transport       string `json:"transport,omitempty"` // always "isola"
	ExpiresAt       string `json:"expires_at,omitempty"`
	MaxExpiresAt    string `json:"max_expires_at,omitempty"`
	Status          string `json:"status"`
	RemainingMin    int    `json:"remaining_min"`
	ExtendAvailable bool   `json:"extend_available"`
	ShareToken      string `json:"share_token,omitempty"`
}

// ShareInfo represents a share token with metadata.
type ShareInfo struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	ShareURL  string    `json:"share_url,omitempty"`
}

// ListSharesResponse is returned when listing shares for a session.
type ListSharesResponse struct {
	Shares []ShareInfo `json:"shares"`
}

// ShareResponse is returned after creating a share link.
type ShareResponse struct {
	Token    string `json:"token"`
	ShareURL string `json:"share_url"`
}

// PaginatedSessions wraps a session list with pagination metadata.
type PaginatedSessions struct {
	Sessions []Session `json:"sessions"`
	Total    int       `json:"total"`
	Limit    int       `json:"limit"`
	Offset   int       `json:"offset"`
}

// ErrorResponse is the standard API error format.
type ErrorResponse struct {
	Error APIError `json:"error"`
}

// APIError holds error code and human-readable message.
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ExtendSessionResponse is returned after extending a session.
type ExtendSessionResponse struct {
	Status          string `json:"status"`
	ExpiresAt       string `json:"expires_at"`
	MaxExpiresAt    string `json:"max_expires_at"`
	RemainingMin    int    `json:"remaining_min"`
	ExtendAvailable bool   `json:"extend_available"`
	ViewerCount     int    `json:"viewer_count"`
}
