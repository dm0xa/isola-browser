package config

import (
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// isolaEnvPrefix is prepended to every isola-owned environment variable. A
// variable resolves as ISOLA_<NAME> first, then falls back to the legacy
// unprefixed <NAME>, so existing deployments, compose files and installers keep
// working while they migrate to the prefixed names.
const isolaEnvPrefix = "ISOLA_"

// getenv resolves an isola environment variable, preferring the ISOLA_-prefixed
// name over the legacy unprefixed one.
func getenv(name string) string {
	if v := os.Getenv(isolaEnvPrefix + name); v != "" {
		return v
	}
	return os.Getenv(name)
}

// DefaultBrowserImage is the session browser image built by the Compose stack.
// A pull-based install overrides it via ISOLA_BROWSER_IMAGE to point at a
// published image instead of building locally.
const DefaultBrowserImage = "isola-browser:electron"

// SessionImage returns the Docker image for session containers: the configured
// override when set, otherwise DefaultBrowserImage. All sessions use the
// Electron offscreen image (Isola transport).
func (c *Config) SessionImage() string {
	if c != nil && c.BrowserImage != "" {
		return c.BrowserImage
	}
	return DefaultBrowserImage
}

// Origins splits a configured origin value into a normalized, de-duplicated
// list of browser origins. Entries may be separated by commas or whitespace.
// Each is reduced to scheme://host (a trailing slash or path is dropped) so it
// matches the Origin header a browser actually sends; entries that are not a
// valid absolute origin, or the wildcard "*" (invalid alongside credentials),
// are dropped. The result is nil when nothing usable remains — CORSOrigins
// guards that case.
func Origins(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
	if len(parts) == 0 {
		return nil
	}
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		origin := NormalizeOrigin(part)
		if origin == "" || origin == "*" {
			continue
		}
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		out = append(out, origin)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeOrigin reduces a value to its scheme://host form, or "" when it is
// not a valid absolute origin. It lets a configured origin be compared against
// the Origin/Referer headers a request carries, which never include a trailing
// slash or path.
func NormalizeOrigin(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// unusableCORSOrigin is a syntactically valid origin under the reserved .invalid
// TLD (RFC 2606) that no browser page can ever present as its Origin.
const unusableCORSOrigin = "https://cors.invalid"

// maxTorExitCountries bounds a tor exit-country list. Tor's own limit is far
// higher; this only rejects absurd input before it reaches a container.
const maxTorExitCountries = 10

// NetworkLogMaxEntriesDefault is the network capture window used when none is
// configured. The streamer (browsers/isola-streamer/netlog.go) keeps the same
// default independently, so the two modules stay in agreement without an
// import between them.
const NetworkLogMaxEntriesDefault = 1000

// NetworkLogMaxEntriesMax bounds a configured window. Entries are metadata
// only, so a high cap is cheap, but an unbounded value would still let one
// long-lived session pin arbitrarily much memory.
const NetworkLogMaxEntriesMax = 10000

// NormalizeTorCountry validates and canonicalizes a Tor exit-country selection:
// a comma-separated list of ISO 3166-1 alpha-2 codes ("se" or "se,no"). The
// result is lowercased and de-duplicated so the container env and the `tor`
// command line always agree. An empty result means "no pinning" (any exit is
// allowed), and blank entries are ignored.
func NormalizeTorCountry(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	codes := make([]string, 0, 4)
	seen := make(map[string]bool, 4)
	for _, part := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		code := strings.ToLower(trimmed)
		if len(code) != 2 || code[0] < 'a' || code[0] > 'z' || code[1] < 'a' || code[1] > 'z' {
			return "", fmt.Errorf("invalid country code %q (want an ISO 3166-1 alpha-2 code, e.g. se)", trimmed)
		}
		if seen[code] {
			continue
		}
		seen[code] = true
		codes = append(codes, code)
	}
	if len(codes) == 0 {
		return "", nil
	}
	if len(codes) > maxTorExitCountries {
		return "", fmt.Errorf("too many country codes (max %d)", maxTorExitCountries)
	}
	return strings.Join(codes, ","), nil
}

// CORSOrigins returns the browser origins allowed to make cross-origin requests.
// It never returns an empty list: an unset or unusable CORS_ORIGIN yields a
// single non-matching origin so cross-origin access is denied. This matters
// because go-chi/cors treats an empty AllowedOrigins as "allow every origin",
// which with AllowCredentials would be unsafe.
func (c *Config) CORSOrigins() []string {
	if c == nil {
		return []string{unusableCORSOrigin}
	}
	if origins := Origins(c.CORSOrigin); len(origins) > 0 {
		return origins
	}
	return []string{unusableCORSOrigin}
}

// BrowserEntry holds per-browser configuration.
type BrowserEntry struct {
	Enabled bool   `yaml:"enabled"`
	Flags   string `yaml:"flags"`
}

// OptionEntry holds a single option with value and label.
type OptionEntry struct {
	Value string `yaml:"value"`
	Label string `yaml:"label"`
}

// Config holds all configuration for the backend service.
type Config struct {
	Port string `yaml:"-"`
	// DatabaseURL and RedisURL are optional external store connections
	// (PostgreSQL + Redis). This build keeps sessions in memory and never reads
	// them directly; they are retained on the shared struct for downstream builds
	// that need a persistent store.
	DatabaseURL string `yaml:"-"`
	RedisURL    string `yaml:"-"`
	DockerHost  string `yaml:"-"`
	// DNS resolution for browser containers. Mode "plain" passes DNSServers
	// straight to `docker run --dns` (IPs). "tls"/"https" start a local
	// forwarder on the browser-net and point containers at it; entries there may
	// be IPs, hostnames, or full tls:// / https:// URLs.
	DNSServers []string `yaml:"-"`
	DNSMode    string   `yaml:"-"` // plain | tls | https
	// TorExitCountry pins Tor exit nodes to one or more countries (a
	// comma-separated list of ISO 3166-1 alpha-2 codes) for sessions created with
	// tor_enabled. Empty (default) allows any exit. A per-session tor_country
	// overrides it; see NormalizeTorCountry for the canonical form.
	TorExitCountry    string `yaml:"-"`
	CORSOrigin        string `yaml:"-"`
	JWTSecret         string `yaml:"-"`
	SessionDefaultMin int    `yaml:"-"` // default session duration in minutes
	SessionMaxMin     int    `yaml:"-"` // maximum session duration in minutes
	SessionExtendMin  int    `yaml:"-"` // minutes added per extend action
	SessionTimer      bool   `yaml:"-"` // when false: sessions are unlimited and close when the owner leaves the page
	InactivityMin     int    `yaml:"-"` // minutes of inactivity before auto-termination
	// MaxSessions caps concurrently active anonymous sessions (provisioning +
	// running) across the whole instance. 0 (default) = unlimited. Container RAM
	// is the real cost, so size it against ISOLA_SESSION_MEMORY_MB and the
	// host's memory.
	MaxSessions int `yaml:"-"`
	// NetworkLogMaxEntries is how many CDP network requests one session keeps
	// in its capture window. The streamer drops the oldest entries past this
	// count, the viewer trims its buffers to the same number, and MCP
	// get_network sees the same window. Metadata only; no bodies or headers.
	NetworkLogMaxEntries int                     `yaml:"-"`
	Resolution           string                  `yaml:"-"`
	ResolutionMax        string                  `yaml:"-"` // hard cap for requested resolutions (WxH)
	WarmPoolEnabled      bool                    `yaml:"-"` // keep pre-started containers so sessions start instantly
	WarmPoolSize         int                     `yaml:"-"` // number of containers kept warm when enabled
	BrowserType          string                  `yaml:"-"` // default browser type
	BrowserTheme         string                  `yaml:"-"`
	BrowserImage         string                  `yaml:"-"` // session container image; empty = DefaultBrowserImage
	Browsers             map[string]BrowserEntry `yaml:"-"` // per-browser config (keyed by type: chromium)
	AuthEnabled          bool                    `yaml:"-"`
	ResolutionVisible    bool                    `yaml:"-"`
	Resolutions          []OptionEntry           `yaml:"-"`
	BrowserVisible       bool                    `yaml:"-"`
	BrowserOptions       []OptionEntry           `yaml:"-"`
	TransportVisible     bool                    `yaml:"-"`
	TransportOptions     []OptionEntry           `yaml:"-"`
	DefaultTransport     string                  `yaml:"-"`
	IsolaTabs            *bool                   `yaml:"-"` // tab strip + address bar for isola transport (nil = enabled)
	DisableURLValidation bool                    `yaml:"-"`
	LogLevel             string                  `yaml:"-"`
	RateLimitingEnabled  bool                    `yaml:"-"`
	MCPEnabled           bool                    `yaml:"-"`
	// MCPTokens are accepted Bearer secrets for POST /mcp. Empty means MCP
	// cannot be enabled.
	MCPTokens []string `yaml:"-"`
	// APITokens optionally gate POST /api/sessions (container spawn). Empty =
	// anonymous creation allowed (default).
	APITokens []string `yaml:"-"`

	// ModulesDir is the root for out-of-process plugin modules (one package
	// subdirectory each). Empty (default) runs with no modules, no sockets and
	// no child processes. Env only: ISOLA_MODULES_DIR.
	ModulesDir string `yaml:"-"`

	// Extension fields. These code paths never read or set them — they exist so
	// a persistent config store can round-trip module and licensing state on the
	// same struct. Deliberately absent from YAML/env parsing and from the public
	// /api/config response, so they leave no trace at runtime.
	InstalledModules         map[string]string `json:"installed_modules,omitempty"`
	LicenseServerURL         string            `json:"license_server_url,omitempty"`
	NetworkMonitoringEnabled bool              `json:"network_monitoring_enabled,omitempty"`
	SIEMWebhookURL           string            `json:"siem_webhook_url,omitempty"`
	DLPClipboardMode         string            `json:"dlp_clipboard_mode,omitempty"` // "" | "block_out" | "block_in" | "block_all"
}

// IsBrowserEnabled returns whether a given browser type is enabled.
func (c *Config) IsBrowserEnabled(browserType string) bool {
	entry, ok := c.Browsers[browserType]
	return ok && entry.Enabled
}

// BrowserFlagsFor returns the flags for the given browser type.
func (c *Config) BrowserFlagsFor(browserType string) string {
	if entry, ok := c.Browsers[browserType]; ok {
		return entry.Flags
	}
	return ""
}

// ClampResolution fits a "WxH" launch resolution inside the configured
// maximum box, preserving the aspect ratio. The viewer maps remote
// coordinates 1:1, so a distorted framebuffer would break every click;
// scaling down is the only safe way to bound the capture size.
//
// Anything unparsable or within the cap is returned unchanged.
func (c *Config) ClampResolution(resolution string) string {
	maxW, maxH := 0, 0
	if _, err := fmt.Sscanf(c.ResolutionMax, "%dx%d", &maxW, &maxH); err != nil || maxW <= 0 || maxH <= 0 {
		return resolution
	}
	w, h := 0, 0
	if _, err := fmt.Sscanf(resolution, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		// Unparsable or out-of-range (e.g. an integer that overflows int):
		// fail closed to the configured default instead of passing the raw
		// client string through, which would bypass the framebuffer cap.
		return c.Resolution
	}
	if w <= maxW && h <= maxH {
		return resolution
	}
	scale := math.Min(float64(maxW)/float64(w), float64(maxH)/float64(h))
	scaledW := int(float64(w) * scale)
	scaledH := int(float64(h) * scale)
	if scaledW < 2 {
		scaledW = 2
	}
	if scaledH < 2 {
		scaledH = 2
	}
	return fmt.Sprintf("%dx%d", scaledW, scaledH)
}

type configFile struct {
	Server struct {
		Port string `yaml:"port"`
	} `yaml:"server"`
	Database struct {
		URL string `yaml:"url"`
	} `yaml:"database"`
	Redis struct {
		URL string `yaml:"url"`
	} `yaml:"redis"`
	Docker struct {
		Host string `yaml:"host"`
	} `yaml:"docker"`
	DNS struct {
		Mode    string   `yaml:"mode"`
		Servers []string `yaml:"servers"`
	} `yaml:"dns"`
	Tor struct {
		// ExitCountry pins Tor exits for tor_enabled sessions (ISO 3166-1
		// alpha-2 codes, comma-separated). Empty = any exit.
		ExitCountry string `yaml:"exit_country"`
	} `yaml:"tor"`
	CORS struct {
		Origin string `yaml:"origin"`
	} `yaml:"cors"`
	Session struct {
		DefaultMin    int    `yaml:"default_min"`
		MaxMin        int    `yaml:"max_min"`
		ExtendMin     int    `yaml:"extend_min"`
		Timer         bool   `yaml:"timer"` // false = no countdown; sandbox closes when the owner leaves the page
		InactivityMin int    `yaml:"inactivity_min"`
		Resolution    string `yaml:"resolution"`
		MaxResolution string `yaml:"max_resolution"` // hard cap applied to every requested resolution
		// NetworkLogMaxEntries is the number of network requests a session's
		// capture window retains (default 1000).
		NetworkLogMaxEntries int `yaml:"network_log_max_entries"`
	} `yaml:"session"`
	WarmPool struct {
		Enabled bool `yaml:"enabled"`
		Size    int  `yaml:"size"`
	} `yaml:"warm_pool"`
	Browser struct {
		Type     string       `yaml:"type"`
		Theme    string       `yaml:"theme"`
		Image    string       `yaml:"image"`
		Chromium BrowserEntry `yaml:"chromium"`
	} `yaml:"browser"`
	Settings struct {
		ResolutionVisible bool          `yaml:"resolution_visible"`
		Resolutions       []OptionEntry `yaml:"resolutions"`
		BrowserVisible    bool          `yaml:"browser_visible"`
		Browsers          []OptionEntry `yaml:"browsers"`
		TransportVisible  bool          `yaml:"transport_visible"`
		Transports        []OptionEntry `yaml:"transports"`
		DefaultTransport  string        `yaml:"default_transport"`
		// IsolaTabs toggles the in-viewer tab strip + address bar for the
		// isola transport. Nil (unset) defaults to enabled.
		IsolaTabs *bool `yaml:"isola_tabs"`
	} `yaml:"settings"`
	Logging struct {
		Level string `yaml:"level"`
	} `yaml:"logging"`
	Modules struct {
		RateLimiting bool `yaml:"rate_limiting"`
	} `yaml:"modules"`
	MCP struct {
		Enabled bool      `yaml:"enabled"`
		Token   TokenList `yaml:"token"`
	} `yaml:"mcp"`
	API struct {
		Token TokenList `yaml:"token"`
	} `yaml:"api"`
}

// TokenList is one or more secrets. YAML may be a scalar (optionally
// comma-separated) or a sequence of scalars.
type TokenList []string

func (t *TokenList) UnmarshalYAML(value *yaml.Node) error {
	if value == nil {
		*t = nil
		return nil
	}
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!null" || value.Value == "" {
			*t = nil
			return nil
		}
		*t = splitCSV(value.Value)
		return nil
	case yaml.SequenceNode:
		var out []string
		for _, n := range value.Content {
			if n == nil {
				continue
			}
			if n.Kind != yaml.ScalarNode {
				return fmt.Errorf("token list entries must be strings")
			}
			out = append(out, splitCSV(n.Value)...)
		}
		*t = out
		return nil
	case yaml.AliasNode:
		return t.UnmarshalYAML(value.Alias)
	default:
		return fmt.Errorf("token must be a string or a list of strings")
	}
}

// Load reads configuration from config.yaml and overrides with environment variables.
func Load() (*Config, error) {
	return LoadFrom("config.yaml")
}

// LoadFrom reads configuration from a YAML file and overrides with environment variables.
func LoadFrom(path string) (*Config, error) {
	cfg := configFile{}
	cfg.Server.Port = "8080"
	cfg.Database.URL = "" // must be set via config.yaml or DATABASE_URL env
	cfg.Redis.URL = "redis://localhost:6379/0"
	cfg.Docker.Host = "unix:///var/run/docker.sock"
	// Public resolvers by default so name resolution does not depend on the
	// container runtime's embedded DNS. Validated below; an empty list keeps
	// Docker/Podman's embedded resolver.
	cfg.DNS.Mode = "plain"
	cfg.DNS.Servers = []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}
	cfg.CORS.Origin = "http://localhost:5173"
	cfg.Session.DefaultMin = 10
	cfg.Session.MaxMin = 30
	cfg.Session.ExtendMin = 5
	// Timer is on by default: every existing deployment keeps countdown,
	// extend and max-duration behavior unless the operator opts out.
	cfg.Session.Timer = true
	cfg.Session.InactivityMin = 5
	cfg.Session.Resolution = "1280x720"
	cfg.Session.NetworkLogMaxEntries = NetworkLogMaxEntriesDefault
	// Hard cap for launch resolutions. Bigger framebuffers are what kill the
	// sandbox: a BGRA frame is w*h*4 bytes, and on a 2 GB host (Podman VM,
	// small CI box) a 3456x2234 capture (30 MB/frame) OOM-kills the container
	// within seconds of the first viewer connecting. Requests above the cap
	// are scaled down, aspect ratio preserved.
	cfg.Session.MaxResolution = "1920x1080"
	// Warm pool is opt-in: idle containers cost real RAM (2 GB limit each),
	// so operators turn it on deliberately.
	cfg.WarmPool.Enabled = false
	cfg.WarmPool.Size = 1
	cfg.Browser.Type = "chromium"
	cfg.Browser.Theme = "dark"
	cfg.Browser.Image = DefaultBrowserImage
	cfg.Logging.Level = "info"

	cfg.Browser.Chromium = BrowserEntry{Enabled: true}

	// Settings defaults
	cfg.Settings.ResolutionVisible = true
	cfg.Settings.BrowserVisible = true
	cfg.Settings.TransportVisible = true
	cfg.Settings.DefaultTransport = "isola"
	cfg.Settings.Transports = []OptionEntry{
		{Value: "isola", Label: "Isola"},
	}
	cfg.Settings.Resolutions = []OptionEntry{
		{Value: "1920x1080", Label: "1080p"},
		{Value: "1280x720", Label: "720p"},
	}

	// Load from YAML file
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("config: failed to parse %s: %w", path, err)
		}
	}

	// Environment variable overrides for per-browser enabled flags
	// Must apply before browserOptions is built below
	cfg.Browser.Chromium.Enabled = envOrDefaultBool("BROWSER_CHROMIUM_ENABLED", cfg.Browser.Chromium.Enabled)

	// Auto-generate browser options from enabled browser entries
	browserLabels := map[string]string{
		"chromium": "Chromium",
	}
	var browserOptions []OptionEntry
	entries := map[string]BrowserEntry{
		"chromium": cfg.Browser.Chromium,
	}
	for key, entry := range entries {
		if entry.Enabled {
			label := browserLabels[key]
			browserOptions = append(browserOptions, OptionEntry{Value: key, Label: label})
		}
	}

	cfg2 := &Config{
		Port:              envOrDefault("PORT", cfg.Server.Port),
		DatabaseURL:       envOrDefault("DATABASE_URL", cfg.Database.URL),
		RedisURL:          envOrDefault("REDIS_URL", cfg.Redis.URL),
		DockerHost:        envOrDefault("DOCKER_HOST", cfg.Docker.Host),
		DNSServers:        envOrDefaultList("DNS_SERVERS", cfg.DNS.Servers),
		DNSMode:           strings.ToLower(envOrDefault("DNS_MODE", cfg.DNS.Mode)),
		CORSOrigin:        envOrDefault("CORS_ORIGIN", cfg.CORS.Origin),
		JWTSecret:         envOrDefault("JWT_SECRET", ""),
		SessionDefaultMin: envOrDefaultInt("SESSION_DEFAULT_MIN", cfg.Session.DefaultMin),
		SessionMaxMin:     envOrDefaultInt("SESSION_MAX_MIN", cfg.Session.MaxMin),
		SessionExtendMin:  envOrDefaultInt("SESSION_EXTEND_MIN", cfg.Session.ExtendMin),
		SessionTimer:      envOrDefaultBool("SESSION_TIMER", cfg.Session.Timer),
		InactivityMin:     envOrDefaultInt("INACTIVITY_MIN", cfg.Session.InactivityMin),
		MaxSessions:       envOrDefaultInt("MAX_SESSIONS", 0),
		NetworkLogMaxEntries: normalizeNetworkLogMaxEntries(
			envOrDefaultInt("NETWORK_LOG_MAX_ENTRIES", cfg.Session.NetworkLogMaxEntries),
		),
		Resolution:      envOrDefault("RESOLUTION", cfg.Session.Resolution),
		ResolutionMax:   envOrDefault("MAX_RESOLUTION", cfg.Session.MaxResolution),
		WarmPoolEnabled: envOrDefaultBool("WARM_POOL_ENABLED", cfg.WarmPool.Enabled),
		WarmPoolSize: func() int {
			sz := envOrDefaultInt("WARM_POOL_SIZE", cfg.WarmPool.Size)
			if sz < 1 {
				sz = 1
			}
			return sz
		}(),
		BrowserType:  envOrDefault("BROWSER_TYPE", cfg.Browser.Type),
		BrowserTheme: envOrDefault("BROWSER_THEME", cfg.Browser.Theme),
		BrowserImage: envOrDefault("BROWSER_IMAGE", cfg.Browser.Image),
		Browsers: map[string]BrowserEntry{
			"chromium": cfg.Browser.Chromium,
		},
		AuthEnabled:          envOrDefaultBool("AUTH_ENABLED", false),
		ResolutionVisible:    cfg.Settings.ResolutionVisible,
		Resolutions:          cfg.Settings.Resolutions,
		BrowserVisible:       cfg.Settings.BrowserVisible,
		BrowserOptions:       browserOptions,
		TransportVisible:     cfg.Settings.TransportVisible,
		TransportOptions:     cfg.Settings.Transports,
		DefaultTransport:     envOrDefault("DEFAULT_TRANSPORT", cfg.Settings.DefaultTransport),
		IsolaTabs:            cfg.Settings.IsolaTabs,
		DisableURLValidation: envOrDefaultBool("DISABLE_URL_VALIDATION", false),
		LogLevel:             envOrDefault("LOG_LEVEL", cfg.Logging.Level),
		RateLimitingEnabled:  envOrDefaultBool("RATE_LIMITING_ENABLED", cfg.Modules.RateLimiting),
		MCPEnabled:           envOrDefaultBool("MCP_ENABLED", cfg.MCP.Enabled),
		MCPTokens:            envOrDefaultList("MCP_TOKEN", []string(cfg.MCP.Token)),
		APITokens:            envOrDefaultList("API_TOKEN", []string(cfg.API.Token)),
		ModulesDir:           envOrDefault("MODULES_DIR", ""),
	}

	// Fail closed: authentication cannot be enabled with a missing or weak
	// signing secret. This prevents shipping a default/guessable JWT secret.
	if cfg2.AuthEnabled && len(cfg2.JWTSecret) < 32 {
		return nil, fmt.Errorf("config: AUTH_ENABLED=true requires JWT_SECRET of at least 32 characters (generate with: openssl rand -hex 32)")
	}

	if cfg2.MCPEnabled {
		if err := validateSecrets("MCP_TOKEN", cfg2.MCPTokens, true); err != nil {
			return nil, err
		}
	}
	if err := validateSecrets("API_TOKEN", cfg2.APITokens, false); err != nil {
		return nil, err
	}

	// Reject malformed resolvers up front: `docker run --dns` would otherwise
	// fail every session at container-create time with an opaque error.
	switch cfg2.DNSMode {
	case "plain":
		for _, server := range cfg2.DNSServers {
			if net.ParseIP(server) == nil {
				return nil, fmt.Errorf("config: invalid DNS server %q (dns.servers / DNS_SERVERS must contain IP addresses in plain mode)", server)
			}
		}
	case "tls", "https":
		// Server entries are validated (as IPs, hostnames, or URLs) when the
		// forwarder is built at startup.
		if len(cfg2.DNSServers) == 0 {
			return nil, fmt.Errorf("config: dns.mode=%s requires at least one dns.servers entry", cfg2.DNSMode)
		}
	default:
		return nil, fmt.Errorf("config: invalid dns.mode %q (want plain, tls, or https)", cfg2.DNSMode)
	}

	// Tor exit-country default for tor_enabled sessions. Validated here so a typo
	// fails startup instead of every Tor session launch (the same normalizer
	// guards the per-session tor_country and the orchestrator args).
	torCountry, err := NormalizeTorCountry(envOrDefault("TOR_EXIT_COUNTRY", cfg.Tor.ExitCountry))
	if err != nil {
		return nil, fmt.Errorf("config: tor.exit_country / TOR_EXIT_COUNTRY: %w", err)
	}
	cfg2.TorExitCountry = torCountry

	return cfg2, nil
}

// normalizeNetworkLogMaxEntries keeps the capture window in a usable range:
// a missing or non-positive value falls back to the default, and an absurd
// value is capped instead of letting one session pin unbounded memory.
func normalizeNetworkLogMaxEntries(n int) int {
	if n <= 0 {
		return NetworkLogMaxEntriesDefault
	}
	if n > NetworkLogMaxEntriesMax {
		return NetworkLogMaxEntriesMax
	}
	return n
}

func envOrDefault(key, defaultVal string) string {
	if val := getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func envOrDefaultInt(key string, defaultVal int) int {
	if val := getenv(key); val != "" {
		var n int
		if _, err := fmt.Sscanf(val, "%d", &n); err == nil {
			return n
		}
	}
	return defaultVal
}

func envOrDefaultBool(key string, defaultVal bool) bool {
	if val := getenv(key); val != "" {
		return val == "true" || val == "1"
	}
	return defaultVal
}

// envOrDefaultList returns a comma-separated env var split into a trimmed list
// of non-empty entries, or the provided default when the var is unset.
func envOrDefaultList(key string, defaultVal []string) []string {
	val := getenv(key)
	if val == "" {
		return defaultVal
	}
	return splitCSV(val)
}

func splitCSV(val string) []string {
	parts := strings.Split(val, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// validateSecrets checks optional or required bearer secrets. required means
// at least one token must be present (MCP when enabled). Every present token
// must be at least 32 characters; values are never included in the error.
func validateSecrets(name string, tokens []string, required bool) error {
	if required && len(tokens) == 0 {
		return fmt.Errorf("config: %s requires at least one token of at least 32 characters (generate with: openssl rand -hex 32)", name)
	}
	for _, tok := range tokens {
		if len(tok) < 32 {
			return fmt.Errorf("config: each %s must be at least 32 characters when set (generate with: openssl rand -hex 32)", name)
		}
	}
	return nil
}

// firstNonEmpty returns the first non-empty string. Useful for layering
// env var overrides on top of config file values.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// boolPtrOrDefault resolves a *bool from a YAML config file with a default.
// If the env var is set, it wins; otherwise if the config pointer is non-nil
// it wins; otherwise the default is used.
func boolPtrOrDefault(envVal string, cfgPtr *bool, defaultVal bool) bool {
	if envVal != "" {
		return envVal == "true" || envVal == "1"
	}
	if cfgPtr != nil {
		return *cfgPtr
	}
	return defaultVal
}
