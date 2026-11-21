package models

import (
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
