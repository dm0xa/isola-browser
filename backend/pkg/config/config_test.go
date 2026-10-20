package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOrigins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "single origin", raw: "https://app.example.se", want: []string{"https://app.example.se"}},
		{name: "trailing slash dropped", raw: "https://app.example.se/", want: []string{"https://app.example.se"}},
		{name: "path dropped", raw: "https://app.example.se/foo/bar", want: []string{"https://app.example.se"}},
		{name: "comma separated", raw: "https://a.se,https://b.se", want: []string{"https://a.se", "https://b.se"}},
		{name: "whitespace separated and deduped", raw: "https://a.se  https://a.se\nhttps://b.se", want: []string{"https://a.se", "https://b.se"}},
		{name: "wildcard dropped beside a real origin", raw: "*,https://a.se", want: []string{"https://a.se"}},
		{name: "non-origin entry dropped", raw: "not-an-origin,https://a.se", want: []string{"https://a.se"}},
		{name: "empty yields nil", raw: "", want: nil},
		{name: "whitespace-only yields nil", raw: "   ", want: nil},
		{name: "comma-only yields nil", raw: ",", want: nil},
		{name: "wildcard-only yields nil", raw: "*", want: nil},
		{name: "non-origin-only yields nil", raw: "not-an-origin", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Origins(tt.raw); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Origins(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestCORSOriginsNeverEmpty pins the security-relevant invariant: the CORS
// middleware must never receive an empty list, because go-chi/cors turns that
// into allow-all, which is unsafe with AllowCredentials. An unusable value must
// deny instead.
func TestCORSOriginsNeverEmpty(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"", "   ", "*", "not-an-origin", ","} {
		got := (&Config{CORSOrigin: raw}).CORSOrigins()
		if len(got) == 0 {
			t.Fatalf("CORSOrigins() for %q returned an empty list; must deny, not allow all", raw)
		}
	}
	if got := (*Config)(nil).CORSOrigins(); len(got) == 0 {
		t.Fatal("nil Config.CORSOrigins() returned an empty list")
	}
}

func TestClampResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		max      string
		request  string
		expected string
	}{
		{name: "within cap is untouched", max: "1920x1080", request: "1280x720", expected: "1280x720"},
		{name: "exact cap is untouched", max: "1920x1080", request: "1920x1080", expected: "1920x1080"},
		{
			// 3456x2234 (Retina Mac) is capped by height: 1080/2234 < 1920/3456,
			// so the height lands exactly on the cap and the width follows the
			// aspect ratio. This is the case that OOM-killed the sandbox.
			name:     "oversized height-bound request keeps aspect",
			max:      "1920x1080",
			request:  "3456x2234",
			expected: "1670x1080",
		},
		{
			// 2560x1440 is capped by width (1920/2560 < 1080/1440).
			name:     "oversized width-bound request keeps aspect",
			max:      "1920x1080",
			request:  "2560x1440",
			expected: "1920x1080",
		},
		{name: "width over cap only", max: "1920x1080", request: "3840x1080", expected: "1920x540"},
		{name: "unparsable request passes through", max: "1920x1080", request: "", expected: ""},
		{name: "unparsable cap disables clamping", max: "", request: "3456x2234", expected: "3456x2234"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{ResolutionMax: tt.max}
			if got := cfg.ClampResolution(tt.request); got != tt.expected {
				t.Errorf("ClampResolution(%q) with max %q = %q, want %q",
					tt.request, tt.max, got, tt.expected)
			}
		})
	}
}

func TestLoadFromWarmPool(t *testing.T) {
	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("defaults are disabled with size 1", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WarmPoolEnabled {
			t.Errorf("WarmPoolEnabled = true, want false (opt-in)")
		}
		if cfg.WarmPoolSize != 1 {
			t.Errorf("WarmPoolSize = %d, want 1", cfg.WarmPoolSize)
		}
	})

	t.Run("yaml values are parsed", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "warm_pool:\n  enabled: true\n  size: 3\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.WarmPoolEnabled || cfg.WarmPoolSize != 3 {
			t.Errorf("WarmPool = (%v, %d), want (true, 3)", cfg.WarmPoolEnabled, cfg.WarmPoolSize)
		}
	})

	t.Run("size below 1 is clamped", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "warm_pool:\n  enabled: true\n  size: 0\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.WarmPoolSize != 1 {
			t.Errorf("WarmPoolSize = %d, want 1", cfg.WarmPoolSize)
		}
	})
}

func TestLoadFromDNSServers(t *testing.T) {
	// Not parallel: the env-override subtest uses t.Setenv, which forbids
	// parallel ancestors.

	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("defaults to public resolvers", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"1.1.1.1", "8.8.8.8", "9.9.9.9"}
		if !reflect.DeepEqual(cfg.DNSServers, want) {
			t.Errorf("DNSServers = %v, want %v", cfg.DNSServers, want)
		}
	})

	t.Run("yaml values are parsed", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "dns:\n  servers:\n    - \"10.0.0.53\"\n    - \"10.0.0.54\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"10.0.0.53", "10.0.0.54"}
		if !reflect.DeepEqual(cfg.DNSServers, want) {
			t.Errorf("DNSServers = %v, want %v", cfg.DNSServers, want)
		}
	})

	t.Run("env overrides yaml", func(t *testing.T) {
		// Not parallel: t.Setenv mutates process-global state.
		t.Setenv("DNS_SERVERS", "9.9.9.9, 1.1.1.1")
		cfg, err := LoadFrom(writeCfg(t, "dns:\n  servers:\n    - \"10.0.0.53\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"9.9.9.9", "1.1.1.1"}
		if !reflect.DeepEqual(cfg.DNSServers, want) {
			t.Errorf("DNSServers = %v, want %v", cfg.DNSServers, want)
		}
	})

	t.Run("invalid resolver is rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadFrom(writeCfg(t, "dns:\n  servers:\n    - \"not-an-ip\"\n")); err == nil {
			t.Error("LoadFrom accepted a non-IP DNS server, want error")
		}
	})
}

func TestLoadFromDNSMode(t *testing.T) {
	// Not parallel: the env-override subtest uses t.Setenv.

	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("defaults to plain", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DNSMode != "plain" {
			t.Errorf("DNSMode = %q, want plain", cfg.DNSMode)
		}
	})

	t.Run("tls mode parses and keeps servers", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "dns:\n  mode: tls\n  servers:\n    - \"1.1.1.1\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DNSMode != "tls" {
			t.Errorf("DNSMode = %q, want tls", cfg.DNSMode)
		}
		if !reflect.DeepEqual(cfg.DNSServers, []string{"1.1.1.1"}) {
			t.Errorf("DNSServers = %v", cfg.DNSServers)
		}
	})

	t.Run("tls mode allows hostnames in servers", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "dns:\n  mode: tls\n  servers:\n    - dns.google\n"))
		if err != nil {
			t.Fatalf("hostname upstream rejected: %v", err)
		}
		if !reflect.DeepEqual(cfg.DNSServers, []string{"dns.google"}) {
			t.Errorf("DNSServers = %v", cfg.DNSServers)
		}
	})

	t.Run("https mode accepts url entries in servers", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "dns:\n  mode: https\n  servers:\n    - \"https://dns.google/dns-query\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DNSMode != "https" {
			t.Errorf("DNSMode = %q, want https", cfg.DNSMode)
		}
		if !reflect.DeepEqual(cfg.DNSServers, []string{"https://dns.google/dns-query"}) {
			t.Errorf("DNSServers = %v", cfg.DNSServers)
		}
	})

	t.Run("env overrides mode and servers", func(t *testing.T) {
		// Not parallel: t.Setenv mutates process-global state.
		t.Setenv("DNS_MODE", "https")
		t.Setenv("DNS_SERVERS", "https://1.1.1.1/dns-query, https://8.8.8.8/dns-query")
		cfg, err := LoadFrom(writeCfg(t, "dns:\n  mode: plain\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DNSMode != "https" {
			t.Errorf("DNSMode = %q, want https", cfg.DNSMode)
		}
		want := []string{"https://1.1.1.1/dns-query", "https://8.8.8.8/dns-query"}
		if !reflect.DeepEqual(cfg.DNSServers, want) {
			t.Errorf("DNSServers = %v, want %v", cfg.DNSServers, want)
		}
	})

	t.Run("encrypted mode without servers is rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadFrom(writeCfg(t, "dns:\n  mode: tls\n  servers: []\n")); err == nil {
			t.Error("LoadFrom accepted dns.mode=tls with no servers, want error")
		}
	})

	t.Run("unknown mode is rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadFrom(writeCfg(t, "dns:\n  mode: quic\n")); err == nil {
			t.Error("LoadFrom accepted dns.mode=quic, want error")
		}
	})
}

func TestLoadFromSessionTimer(t *testing.T) {
	// Not parallel: the env-override subtest uses t.Setenv, which forbids
	// parallel ancestors.

	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("defaults to enabled", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.SessionTimer {
			t.Errorf("SessionTimer = false, want true (default preserves existing behavior)")
		}
	})

	t.Run("yaml can disable it", func(t *testing.T) {
		t.Parallel()
		cfg, err := LoadFrom(writeCfg(t, "session:\n  timer: false\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SessionTimer {
			t.Errorf("SessionTimer = true, want false")
		}
	})

	t.Run("env overrides yaml", func(t *testing.T) {
		// Not parallel: t.Setenv mutates process-global state.
		t.Setenv("SESSION_TIMER", "false")
		cfg, err := LoadFrom(writeCfg(t, "session:\n  timer: true\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SessionTimer {
			t.Errorf("SessionTimer = true, want false (SESSION_TIMER=false must win)")
		}
	})
}

func TestLoadFromAPITokenValidation(t *testing.T) {
	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("short API token is rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadFrom(writeCfg(t, "api:\n  token: short\n")); err == nil {
			t.Error("LoadFrom accepted short API_TOKEN, want error")
		}
	})

	t.Run("MCP enabled without long token is rejected", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadFrom(writeCfg(t, "mcp:\n  enabled: true\n  token: tiny\n")); err == nil {
			t.Error("LoadFrom accepted MCP with short token, want error")
		}
	})

	t.Run("YAML list of API tokens is accepted", func(t *testing.T) {
		t.Parallel()
		const a = "api-token-one-32-characters-min!!!"
		const b = "api-token-two-32-characters-min!!!"
		cfg, err := LoadFrom(writeCfg(t, "api:\n  token:\n    - "+a+"\n    - "+b+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{a, b}; !reflect.DeepEqual(cfg.APITokens, want) {
			t.Errorf("APITokens = %v, want %v", cfg.APITokens, want)
		}
	})

	t.Run("comma-separated API token string is accepted", func(t *testing.T) {
		t.Parallel()
		const a = "api-token-one-32-characters-min!!!"
		const b = "api-token-two-32-characters-min!!!"
		cfg, err := LoadFrom(writeCfg(t, "api:\n  token: \""+a+", "+b+"\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{a, b}; !reflect.DeepEqual(cfg.APITokens, want) {
			t.Errorf("APITokens = %v, want %v", cfg.APITokens, want)
		}
	})

	t.Run("one short token in an API list is rejected", func(t *testing.T) {
		t.Parallel()
		const a = "api-token-one-32-characters-min!!!"
		if _, err := LoadFrom(writeCfg(t, "api:\n  token:\n    - "+a+"\n    - short\n")); err == nil {
			t.Error("LoadFrom accepted a short API token in a list, want error")
		}
	})

	t.Run("YAML list of MCP tokens is accepted when enabled", func(t *testing.T) {
		t.Parallel()
		const a = "mcp-token-one-32-characters-min!!!"
		const b = "mcp-token-two-32-characters-min!!!"
		cfg, err := LoadFrom(writeCfg(t, "mcp:\n  enabled: true\n  token:\n    - "+a+"\n    - "+b+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{a, b}; !reflect.DeepEqual(cfg.MCPTokens, want) {
			t.Errorf("MCPTokens = %v, want %v", cfg.MCPTokens, want)
		}
	})
}

func TestLoadFromAPITokenEnvList(t *testing.T) {
	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	const a = "env-token-one-32-characters-min!!!"
	const b = "env-token-two-32-characters-min!!!"
	t.Setenv("ISOLA_API_TOKEN", a+", "+b)
	cfg, err := LoadFrom(writeCfg(t, "api:\n  token: yaml-token-32-characters-minimum!!\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{a, b}; !reflect.DeepEqual(cfg.APITokens, want) {
		t.Errorf("APITokens = %v, want %v (env list must replace yaml)", cfg.APITokens, want)
	}
}

func TestLoadFromMaxSessions(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.
	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	if cfg, err := LoadFrom(writeCfg(t, "")); err != nil {
		t.Fatal(err)
	} else if cfg.MaxSessions != 0 {
		t.Errorf("MaxSessions default = %d, want 0 (unlimited)", cfg.MaxSessions)
	}

	t.Setenv("ISOLA_MAX_SESSIONS", "3")
	cfg, err := LoadFrom(writeCfg(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSessions != 3 {
		t.Errorf("MaxSessions = %d, want 3 from ISOLA_MAX_SESSIONS", cfg.MaxSessions)
	}
}

func TestLoadFromIsolaEnvPrefix(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.

	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("prefixed name overrides yaml", func(t *testing.T) {
		t.Setenv("ISOLA_PORT", "9090")
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != "9090" {
			t.Errorf("Port = %q, want 9090 (ISOLA_PORT)", cfg.Port)
		}
	})

	t.Run("legacy unprefixed name still works", func(t *testing.T) {
		t.Setenv("PORT", "9191")
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != "9191" {
			t.Errorf("Port = %q, want 9191 (legacy PORT must keep working)", cfg.Port)
		}
	})

	t.Run("prefix wins over legacy", func(t *testing.T) {
		t.Setenv("PORT", "1111")
		t.Setenv("ISOLA_PORT", "2222")
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Port != "2222" {
			t.Errorf("Port = %q, want 2222 (ISOLA_PORT must beat PORT)", cfg.Port)
		}
	})

	t.Run("applies to bool and list helpers", func(t *testing.T) {
		t.Setenv("ISOLA_SESSION_TIMER", "false")
		t.Setenv("ISOLA_DNS_SERVERS", "9.9.9.9, 1.1.1.1")
		cfg, err := LoadFrom(writeCfg(t, "server:\n  port: \"8080\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.SessionTimer {
			t.Error("SessionTimer = true, want false (ISOLA_SESSION_TIMER)")
		}
		if want := []string{"9.9.9.9", "1.1.1.1"}; !reflect.DeepEqual(cfg.DNSServers, want) {
			t.Errorf("DNSServers = %v, want %v (ISOLA_DNS_SERVERS)", cfg.DNSServers, want)
		}
	})
}

func TestLoadFromBrowserImage(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.
	writeCfg := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("default is the locally built image", func(t *testing.T) {
		cfg, err := LoadFrom(writeCfg(t, ""))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.SessionImage(); got != DefaultBrowserImage {
			t.Errorf("SessionImage() = %q, want %q", got, DefaultBrowserImage)
		}
	})

	t.Run("yaml override", func(t *testing.T) {
		cfg, err := LoadFrom(writeCfg(t, "browser:\n  image: \"ghcr.io/acme/isola-browser:electron\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.SessionImage(); got != "ghcr.io/acme/isola-browser:electron" {
			t.Errorf("SessionImage() = %q, want the yaml override", got)
		}
	})

	t.Run("ISOLA_BROWSER_IMAGE wins over yaml", func(t *testing.T) {
		t.Setenv("ISOLA_BROWSER_IMAGE", "registry.example/isola-browser:electron")
		cfg, err := LoadFrom(writeCfg(t, "browser:\n  image: \"ghcr.io/acme/isola-browser:electron\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.SessionImage(); got != "registry.example/isola-browser:electron" {
			t.Errorf("SessionImage() = %q, want the ISOLA_BROWSER_IMAGE override", got)
		}
	})

	t.Run("nil config falls back to the default", func(t *testing.T) {
		var cfg *Config
		if got := cfg.SessionImage(); got != DefaultBrowserImage {
			t.Errorf("nil SessionImage() = %q, want %q", got, DefaultBrowserImage)
		}
	})
}

// loadDefaultConfig parses a missing YAML file, so only built-in defaults and
// env overrides apply.
func loadDefaultConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := LoadFrom(filepath.Join(t.TempDir(), "config.yaml"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return cfg
}

func TestNetworkLogMaxEntriesDefaults(t *testing.T) {
	t.Setenv("ISOLA_NETWORK_LOG_MAX_ENTRIES", "")
	t.Setenv("NETWORK_LOG_MAX_ENTRIES", "")

	cfg := loadDefaultConfig(t)
	if cfg.NetworkLogMaxEntries != NetworkLogMaxEntriesDefault {
		t.Fatalf("NetworkLogMaxEntries = %d, want %d", cfg.NetworkLogMaxEntries, NetworkLogMaxEntriesDefault)
	}
}

func TestNetworkLogMaxEntriesEnv(t *testing.T) {
	t.Setenv("ISOLA_NETWORK_LOG_MAX_ENTRIES", "2500")
	t.Setenv("NETWORK_LOG_MAX_ENTRIES", "")

	cfg := loadDefaultConfig(t)
	if cfg.NetworkLogMaxEntries != 2500 {
		t.Fatalf("NetworkLogMaxEntries = %d, want 2500", cfg.NetworkLogMaxEntries)
	}
}

func TestNetworkLogMaxEntriesLegacyEnv(t *testing.T) {
	t.Setenv("ISOLA_NETWORK_LOG_MAX_ENTRIES", "")
	t.Setenv("NETWORK_LOG_MAX_ENTRIES", "320")

	cfg := loadDefaultConfig(t)
	if cfg.NetworkLogMaxEntries != 320 {
		t.Fatalf("NetworkLogMaxEntries = %d, want 320", cfg.NetworkLogMaxEntries)
	}
}

func TestNetworkLogMaxEntriesNormalized(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"0", NetworkLogMaxEntriesDefault},
		{"-5", NetworkLogMaxEntriesDefault},
		{"not-a-number", NetworkLogMaxEntriesDefault},
		{"999999", NetworkLogMaxEntriesMax},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("ISOLA_NETWORK_LOG_MAX_ENTRIES", tc.env)
			t.Setenv("NETWORK_LOG_MAX_ENTRIES", "")

			cfg := loadDefaultConfig(t)
			if cfg.NetworkLogMaxEntries != tc.want {
				t.Fatalf("NetworkLogMaxEntries = %d, want %d", cfg.NetworkLogMaxEntries, tc.want)
			}
		})
	}
}
