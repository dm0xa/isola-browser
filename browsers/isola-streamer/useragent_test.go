package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestParseChromeVersion(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"ua", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.8078.49 Safari/537.36", "156.0.8078.49"},
		{"product", "Chrome/151.0.7223.12", "151.0.7223.12"},
		{"electron token is not a version source", "Chrome/156.0.8078.49 Electron/45.1.0", "156.0.8078.49"},
		{"no chrome", "Mozilla/5.0 (X11; Linux x86_64) Electron/45.1.0", ""},
		{"short version rejected", "Chrome/156", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseChromeVersion(tt.in); got != tt.want {
				t.Fatalf("parseChromeVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestUserAgentOverrideParams(t *testing.T) {
	p := userAgentOverrideParams("156.0.8078.49")

	ua, _ := p["userAgent"].(string)
	if !strings.Contains(ua, "Chrome/156.0.8078.49") || !strings.Contains(ua, "Linux x86_64") {
		t.Fatalf("userAgent = %q", ua)
	}
	if strings.Contains(strings.ToLower(ua), "electron") {
		t.Fatalf("userAgent names the embedder: %q", ua)
	}
	if p["platform"] != "Linux x86_64" {
		t.Fatalf("platform = %v", p["platform"])
	}
	if p["acceptLanguage"] != "en-US,en" {
		t.Fatalf("acceptLanguage = %v", p["acceptLanguage"])
	}

	md, ok := p["userAgentMetadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("userAgentMetadata missing: %T", p["userAgentMetadata"])
	}
	brands, _ := md["brands"].([]map[string]interface{})
	want := []struct{ brand, version string }{
		{"Not:A-Brand", "8"},
		{"Chromium", "156"},
		{"Google Chrome", "156"},
	}
	if len(brands) != len(want) {
		t.Fatalf("brands = %v", brands)
	}
	for i, w := range want {
		if brands[i]["brand"] != w.brand || brands[i]["version"] != w.version {
			t.Fatalf("brands[%d] = %v, want %s/%s", i, brands[i], w.brand, w.version)
		}
	}
	full, _ := md["fullVersionList"].([]map[string]interface{})
	if len(full) != 3 || full[2]["brand"] != "Google Chrome" || full[2]["version"] != "156.0.8078.49" {
		t.Fatalf("fullVersionList = %v", full)
	}
	// navigator.platform is "Linux x86_64" on every Linux Chromium build, so the
	// metadata must not claim another architecture.
	if md["architecture"] != "x86" || md["bitness"] != "64" || md["platform"] != "Linux" {
		t.Fatalf("metadata = %v", md)
	}
}

func TestUATargetType(t *testing.T) {
	for _, typ := range []string{"page", "webview", "iframe"} {
		if !uaTargetType(typ) {
			t.Fatalf("%s should get the override", typ)
		}
	}
	for _, typ := range []string{"worker", "service_worker", "browser", ""} {
		if uaTargetType(typ) {
			t.Fatalf("%s must not get the override", typ)
		}
	}
}

func TestAcceptLanguageFor(t *testing.T) {
	tests := []struct {
		locale string
		want   string
	}{
		{"sv-SE", "sv-SE,sv,en-US,en"},
		{"de-DE", "de-DE,de,en-US,en"},
		{"en-US", "en-US,en"},
		{"en-GB", "en-GB,en"},
		{"en", "en"},
		{"sv", "sv,en-US,en"},
		{"zh-Hant-TW", "zh-Hant-TW,zh,en-US,en"},
	}
	for _, tt := range tests {
		if got := acceptLanguageFor(tt.locale); got != tt.want {
			t.Fatalf("acceptLanguageFor(%q) = %q, want %q", tt.locale, got, tt.want)
		}
	}
}

// TestAcceptLanguageIsATagList pins the two properties a hand-written value
// used to break: no q-values (Chromium appends its own, and a ";" in the input
// goes out doubled) and no repeated tag (a bare locale used to emit "sv,sv",
// which shows up verbatim in navigator.languages).
func TestAcceptLanguageIsATagList(t *testing.T) {
	for _, locale := range []string{"sv-SE", "sv", "de-DE", "en-GB", "en-US", "en", "zh-Hant-TW", "sr-Latn-RS"} {
		got := acceptLanguageFor(locale)
		if strings.Contains(got, ";") {
			t.Fatalf("acceptLanguageFor(%q) = %q carries q-values", locale, got)
		}
		seen := map[string]bool{}
		for _, tag := range strings.Split(got, ",") {
			if tag == "" {
				t.Fatalf("acceptLanguageFor(%q) = %q has an empty tag", locale, got)
			}
			if seen[tag] {
				t.Fatalf("acceptLanguageFor(%q) = %q repeats %q", locale, got, tag)
			}
			seen[tag] = true
		}
		if got != locale && !strings.HasPrefix(got, locale+",") {
			t.Fatalf("acceptLanguageFor(%q) = %q does not lead with the declared tag", locale, got)
		}
	}
}

// TestIdentityOverridesFollowDeclaredIdentity pins that a declared locale
// reaches the UA payload (argument language list) and that the timezone/locale
// payloads carry exactly the CDP parameter names.
func TestIdentityOverridesFollowDeclaredIdentity(t *testing.T) {
	c := &identityClient{}
	if got := c.acceptLanguage(); got != uaAcceptLanguage {
		t.Fatalf("default acceptLanguage = %q, want %q", got, uaAcceptLanguage)
	}
	c.locale = "sv-SE"
	if got := c.acceptLanguage(); got != "sv-SE,sv,en-US,en" {
		t.Fatalf("declared acceptLanguage = %q, want %q", got, "sv-SE,sv,en-US,en")
	}

	p := userAgentParams("156.0.8078.49", "sv-SE,sv,en-US,en")
	if p["acceptLanguage"] != "sv-SE,sv,en-US,en" {
		t.Fatalf("userAgentParams acceptLanguage = %v", p["acceptLanguage"])
	}

	tz := timezoneOverrideParams("Europe/Stockholm")
	if tz["timezoneId"] != "Europe/Stockholm" {
		t.Fatalf("timezoneOverrideParams = %v", tz)
	}
	loc := localeOverrideParams("sv-SE")
	if loc["locale"] != "sv-SE" {
		t.Fatalf("localeOverrideParams = %v", loc)
	}
}

func TestIdentityClientReadsDeclaredIdentityFromEnv(t *testing.T) {
	t.Setenv("ISOLA_SESSION_TZ", " Europe/Stockholm ")
	t.Setenv("ISOLA_SESSION_LOCALE", "sv-SE")
	c := newIdentityClient()
	if c.timezone != "Europe/Stockholm" {
		t.Fatalf("timezone = %q", c.timezone)
	}
	if c.locale != "sv-SE" {
		t.Fatalf("locale = %q", c.locale)
	}
	if got := c.describe(); !strings.Contains(got, "timezone Europe/Stockholm") || !strings.Contains(got, "locale sv-SE") {
		t.Fatalf("describe() = %q", got)
	}

	t.Setenv("ISOLA_SESSION_TZ", "")
	t.Setenv("ISOLA_SESSION_LOCALE", "")
	c = newIdentityClient()
	if c.timezone != "" || c.locale != "" {
		t.Fatalf("unset identity = %q/%q, want empty", c.timezone, c.locale)
	}
	if got := c.acceptLanguage(); got != uaAcceptLanguage {
		t.Fatalf("unset acceptLanguage = %q", got)
	}
}

// TestIdentityClientAppliesOverrideOnAttach drives the real browser endpoint
// shape: /json/version → browser ws → setAutoAttach → attachedToTarget, and
// asserts the UA override is sent before any page script could run.
func TestIdentityClientAppliesOverrideOnAttach(t *testing.T) {
	type command struct {
		method    string
		sessionID string
		params    map[string]interface{}
	}
	commands := make(chan command, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		wsURL := "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/ws"
		ua := "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.8078.49 Safari/537.36"
		_ = json.NewEncoder(w).Encode(map[string]string{
			"webSocketDebuggerUrl": wsURL,
			"User-Agent":           ua,
			"Browser":              "Chrome/156.0.8078.49",
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg struct {
				ID        int                    `json:"id"`
				Method    string                 `json:"method"`
				Params    map[string]interface{} `json:"params"`
				SessionID string                 `json:"sessionId"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			commands <- command{msg.Method, msg.SessionID, msg.Params}
			_ = conn.WriteJSON(map[string]interface{}{"id": msg.ID, "result": map[string]interface{}{}})
			if msg.Method == "Target.setAutoAttach" {
				_ = conn.WriteJSON(map[string]interface{}{
					"method": "Target.attachedToTarget",
					"params": map[string]interface{}{
						"sessionId":  "S1",
						"targetInfo": map[string]interface{}{"targetId": "T1", "type": "page", "url": "https://example.com/"},
					},
				})
			}
		}
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	// The session declares an identity; all three overrides must ride the
	// attach before any page script can run.
	t.Setenv("ISOLA_SESSION_TZ", "Europe/Stockholm")
	t.Setenv("ISOLA_SESSION_LOCALE", "sv-SE")

	c := newIdentityClient()
	c.baseURL = srv.URL
	if err := c.connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.dropConn()
	go c.readPump()

	got := make(map[string]command, 3)
	deadline := time.After(3 * time.Second)
	for len(got) < 3 {
		select {
		case cmd := <-commands:
			switch cmd.method {
			case "Emulation.setUserAgentOverride", "Emulation.setTimezoneOverride", "Emulation.setLocaleOverride":
				got[cmd.method] = cmd
			}
		case <-deadline:
			t.Fatalf("missing overrides after attach: got %d of 3", len(got))
		}
	}
	override, ok := got["Emulation.setUserAgentOverride"]
	if !ok {
		t.Fatal("no user agent override sent after attach")
	}

	if override.sessionID != "S1" {
		t.Fatalf("override sessionId = %q, want S1", override.sessionID)
	}
	if override.params["acceptLanguage"] != "sv-SE,sv,en-US,en" {
		t.Fatalf("acceptLanguage = %v, want sv-SE,sv,en-US,en", override.params["acceptLanguage"])
	}
	if tz := got["Emulation.setTimezoneOverride"]; tz.sessionID != "S1" || tz.params["timezoneId"] != "Europe/Stockholm" {
		t.Fatalf("timezone override = %+v", tz)
	}
	if loc := got["Emulation.setLocaleOverride"]; loc.sessionID != "S1" || loc.params["locale"] != "sv-SE" {
		t.Fatalf("locale override = %+v", loc)
	}
	md, _ := override.params["userAgentMetadata"].(map[string]interface{})
	brands, _ := md["brands"].([]interface{})
	found := false
	for _, b := range brands {
		if bm, ok := b.(map[string]interface{}); ok && bm["brand"] == "Google Chrome" {
			found = true
		}
	}
	if !found {
		t.Fatalf("brands = %v, want a Google Chrome entry", brands)
	}
	if fmt.Sprint(override.params["platform"]) != "Linux x86_64" {
		t.Fatalf("platform = %v", override.params["platform"])
	}
}

// identityCommand is one CDP command the fake browser received.
type identityCommand struct {
	method    string
	sessionID string
	params    map[string]interface{}
}

// fakeIdentityBrowser serves /json/version and one browser websocket that
// records commands and announces a page target on setAutoAttach. The channel is
// buffered generously because a runtime identity change sends three commands
// per attached target.
func fakeIdentityBrowser(t *testing.T, commands chan<- identityCommand) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"webSocketDebuggerUrl": "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/ws",
			"User-Agent":           "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.8078.49 Safari/537.36",
			"Browser":              "Chrome/156.0.8078.49",
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg struct {
				ID        int                    `json:"id"`
				Method    string                 `json:"method"`
				Params    map[string]interface{} `json:"params"`
				SessionID string                 `json:"sessionId"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			commands <- identityCommand{msg.Method, msg.SessionID, msg.Params}
			_ = conn.WriteJSON(map[string]interface{}{"id": msg.ID, "result": map[string]interface{}{}})
			if msg.Method == "Target.setAutoAttach" {
				_ = conn.WriteJSON(map[string]interface{}{
					"method": "Target.attachedToTarget",
					"params": map[string]interface{}{
						"sessionId":  "S1",
						"targetInfo": map[string]interface{}{"targetId": "T1", "type": "page", "url": "https://example.com/"},
					},
				})
			}
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// nextIdentityCommand waits for one command with the given method (and session,
// when set), skipping the unrelated traffic.
func nextIdentityCommand(t *testing.T, commands <-chan identityCommand, method, sessionID string) identityCommand {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case cmd := <-commands:
			if cmd.method != method {
				continue
			}
			if sessionID != "" && cmd.sessionID != sessionID {
				continue
			}
			return cmd
		case <-deadline:
			t.Fatalf("no %s command for session %q", method, sessionID)
		}
	}
}

// A live identity change must reach targets that are already attached: a warm
// pool container is created with the pool's identity, and a claim applies the
// session's own values over this route.
func TestSetIdentityReappliesToAttachedTargets(t *testing.T) {
	t.Setenv("ISOLA_SESSION_TZ", "")
	t.Setenv("ISOLA_SESSION_LOCALE", "")

	commands := make(chan identityCommand, 64)
	srv := fakeIdentityBrowser(t, commands)

	c := newIdentityClient()
	c.baseURL = srv.URL
	if err := c.connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.dropConn()
	go c.readPump()

	// Attach: no declared identity, so only the UA override goes out.
	first := nextIdentityCommand(t, commands, "Emulation.setUserAgentOverride", "S1")
	if first.params["acceptLanguage"] != "en-US,en" {
		t.Fatalf("initial acceptLanguage = %v, want en-US,en", first.params["acceptLanguage"])
	}

	targets, err := c.SetIdentity("Europe/Berlin", "de-DE")
	if err != nil {
		t.Fatalf("SetIdentity: %v", err)
	}
	if targets != 1 {
		t.Fatalf("targets = %d, want the one attached page", targets)
	}
	ua := nextIdentityCommand(t, commands, "Emulation.setUserAgentOverride", "S1")
	if ua.params["acceptLanguage"] != "de-DE,de,en-US,en" {
		t.Fatalf("acceptLanguage after change = %v, want de-DE,de,en-US,en", ua.params["acceptLanguage"])
	}
	tz := nextIdentityCommand(t, commands, "Emulation.setTimezoneOverride", "S1")
	if tz.params["timezoneId"] != "Europe/Berlin" {
		t.Fatalf("timezoneId = %v, want Europe/Berlin", tz.params["timezoneId"])
	}
	loc := nextIdentityCommand(t, commands, "Emulation.setLocaleOverride", "S1")
	if loc.params["locale"] != "de-DE" {
		t.Fatalf("locale = %v, want de-DE", loc.params["locale"])
	}

	// An empty value must remove the override, not leave the old one in place.
	if _, err := c.SetIdentity("", ""); err != nil {
		t.Fatalf("reset: %v", err)
	}
	ua = nextIdentityCommand(t, commands, "Emulation.setUserAgentOverride", "S1")
	if ua.params["acceptLanguage"] != "en-US,en" {
		t.Fatalf("acceptLanguage after reset = %v, want en-US,en", ua.params["acceptLanguage"])
	}
	tz = nextIdentityCommand(t, commands, "Emulation.setTimezoneOverride", "S1")
	if tz.params["timezoneId"] != "" {
		t.Fatalf("timezoneId after reset = %v, want an empty override", tz.params["timezoneId"])
	}
	loc = nextIdentityCommand(t, commands, "Emulation.setLocaleOverride", "S1")
	if loc.params["locale"] != "" {
		t.Fatalf("locale after reset = %v, want an empty override", loc.params["locale"])
	}
}

func TestHandleIdentityContract(t *testing.T) {
	rec := httptest.NewRecorder()
	handleIdentity(rec, httptest.NewRequest(http.MethodGet, "/identity", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /identity = %d, want 405", rec.Code)
	}

	prev := activeIdentity
	t.Cleanup(func() { activeIdentity = prev })
	activeIdentity = nil
	rec = httptest.NewRecorder()
	handleIdentity(rec, httptest.NewRequest(http.MethodPost, "/identity", strings.NewReader(`{"timezone":"Europe/Stockholm","locale":"sv-SE"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no identity client: status = %d, want 503", rec.Code)
	}

	activeIdentity = newIdentityClient()
	for _, body := range []string{
		`{"timezone":"Europe/Stockholm","locale":"sv_SE"}`,
		`{"timezone":"../etc/passwd"}`,
		`{"timezone":"Europe/Stockholm","bogus":1}`,
	} {
		rec = httptest.NewRecorder()
		handleIdentity(rec, httptest.NewRequest(http.MethodPost, "/identity", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400 (%s)", body, rec.Code, rec.Body.String())
		}
	}

	// A well-formed request reaches the (unconnected) client, which reports it
	// cannot apply rather than pretending success.
	rec = httptest.NewRecorder()
	handleIdentity(rec, httptest.NewRequest(http.MethodPost, "/identity", strings.NewReader(`{"timezone":"Europe/Stockholm","locale":"sv-SE"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("disconnected identity: status = %d, want 502 (%s)", rec.Code, rec.Body.String())
	}
}
