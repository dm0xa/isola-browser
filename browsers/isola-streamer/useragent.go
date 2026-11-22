package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Page-visible identity: the browser identity every target must share.
//
// The Electron runner's UA fallback (user_agent.js) names Chrome, but Chromium
// derives the UA-CH brands from the product name, so the sandbox reports
// userAgentData.brands = [Not:A-Brand, Chromium] with no Google Chrome entry.
// The UA string and the client hints then contradict each other, which bot
// gates read as a spoof (and the HTTP Sec-CH-UA header carries the same lie).
// userAgentMetadata is settable only over CDP, and Electron's in-process
// debugger closes the target when that command is sent before the first
// navigation (verified), so the override is applied here at target creation —
// before the page's first script runs.
//
// The capture client (cdp_capture.go) is also browser-level, but it only starts
// when the viewer asked for the analysis panel (NET_MONITOR=1). Identity is not
// optional, so it gets its own auto-attach with no Network/Runtime events.
// The entrypoint starts this process before the Electron runner, so the attach
// is already armed when the first page target is created.

const uaPlatform = "Linux x86_64"

// uaAcceptLanguage matches Chrome's default Linux value. No q-values here:
// Chromium appends its own, and "en-US,en;q=0.9" would go out as
// "en-US,en;q=0.9;q=0.9" and leak a third navigator.languages entry.
const uaAcceptLanguage = "en-US,en"

var chromeVersionRe = regexp.MustCompile(`Chrome/(\d+\.\d+\.\d+\.\d+)`)

type identityClient struct {
	baseURL string
	// timezone / locale are the session's declared page identity, read once
	// from the container env (ISOLA_SESSION_TZ / ISOLA_SESSION_LOCALE). Empty
	// keeps Chromium's own values — today that is UTC and en-US — so a session
	// that declares nothing behaves exactly as before.
	timezone string
	locale   string

	mu      sync.Mutex
	conn    *websocket.Conn
	nextID  int
	version string
	warned  bool
	// sessions are the targets currently attached to this browser-level
	// connection. A live identity change has to reach them too, not only the
	// targets that attach later.
	sessions map[string]bool
}

func newIdentityClient() *identityClient {
	baseURL := os.Getenv("CDP_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:9222"
	}
	return &identityClient{
		baseURL:  baseURL,
		nextID:   1,
		timezone: strings.TrimSpace(os.Getenv("ISOLA_SESSION_TZ")),
		locale:   strings.TrimSpace(os.Getenv("ISOLA_SESSION_LOCALE")),
		sessions: map[string]bool{},
	}
}

// acceptLanguage is what the UA override advertises in navigator.languages and
// Accept-Language. A declared locale wins; otherwise Chrome's Linux default.
func (c *identityClient) acceptLanguage() string {
	c.mu.Lock()
	locale := c.locale
	c.mu.Unlock()
	if locale == "" {
		return uaAcceptLanguage
	}
	return acceptLanguageFor(locale)
}

// Start keeps a browser-level auto-attach alive for the process lifetime.
func (c *identityClient) Start() {
	for {
		if err := c.connect(); err != nil {
			log.Printf("identity: connect: %v (retry 2s)", err)
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("identity: auto-attach (%s)", c.describe())
		c.readPump()
		c.dropConn()
		log.Printf("identity: reconnecting")
		time.Sleep(2 * time.Second)
	}
}

// describe summarizes the overrides for the one attach log line.
func (c *identityClient) describe() string {
	c.mu.Lock()
	timezone, locale := c.timezone, c.locale
	c.mu.Unlock()
	out := "ua override " + c.versionOf()
	if timezone != "" {
		out += ", timezone " + timezone
	}
	if locale != "" {
		out += ", locale " + locale
	}
	return out
}

// connect dials the browser endpoint and arms auto-attach for current and
// future targets. The browser version is read from the same endpoint so the
// override cannot advertise a version the engine is not.
func (c *identityClient) connect() error {
	version, wsURL, err := browserIdentity(c.baseURL)
	if err != nil {
		return err
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial browser ws: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.nextID = 1
	c.version = version
	c.warned = false
	c.mu.Unlock()

	return c.send("Target.setAutoAttach", map[string]interface{}{
		"autoAttach":             true,
		"waitForDebuggerOnStart": false,
		"flatten":                true,
	})
}

func (c *identityClient) dropConn() {
	c.mu.Lock()
	if c.conn != nil {
		c.conn.Close()
	}
	c.conn = nil
	c.mu.Unlock()
}

func (c *identityClient) versionOf() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.version
}

func (c *identityClient) readPump() {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var env struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		switch env.Method {
		case "Target.detachedFromTarget":
			var p struct {
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(env.Params, &p) == nil && p.SessionID != "" {
				c.mu.Lock()
				delete(c.sessions, p.SessionID)
				c.mu.Unlock()
			}
			continue
		case "Target.attachedToTarget":
		default:
			continue
		}
		var p struct {
			SessionID  string `json:"sessionId"`
			TargetInfo struct {
				Type string `json:"type"`
			} `json:"targetInfo"`
		}
		if json.Unmarshal(env.Params, &p) != nil || p.SessionID == "" || !uaTargetType(p.TargetInfo.Type) {
			continue
		}
		c.mu.Lock()
		c.sessions[p.SessionID] = true
		c.mu.Unlock()
		if err := c.applyTo(p.SessionID, false, false); err != nil {
			c.warnOnce(err)
		}
	}
}

// applyTo writes the current identity onto one target session. clearTimezone /
// clearLocale send the empty override CDP needs to *remove* a value that was
// set earlier; attach never clears (a new target has nothing to clear).
// Order matters only in that the UA carries the locale's language list; the
// timezone/locale overrides are independent Emulation state.
func (c *identityClient) applyTo(sessionID string, clearTimezone, clearLocale bool) error {
	c.mu.Lock()
	version, timezone, locale := c.version, c.timezone, c.locale
	c.mu.Unlock()
	if version == "" {
		return fmt.Errorf("identity not connected")
	}
	acceptLanguage := uaAcceptLanguage
	if locale != "" {
		acceptLanguage = acceptLanguageFor(locale)
	}
	if err := c.sendSession("Emulation.setUserAgentOverride", userAgentParams(version, acceptLanguage), sessionID); err != nil {
		return err
	}
	if timezone != "" || clearTimezone {
		if err := c.sendSession("Emulation.setTimezoneOverride", timezoneOverrideParams(timezone), sessionID); err != nil {
			return err
		}
	}
	if locale != "" || clearLocale {
		if err := c.sendSession("Emulation.setLocaleOverride", localeOverrideParams(locale), sessionID); err != nil {
			return err
		}
	}
	return nil
}

// SetIdentity changes the page-visible identity of a live session and applies
// it to every target already attached, then to every target that attaches
// later. A warm container is created with the pool's identity, so this is what
// lets a claim serve any zone/locale without a cold start. An empty value
// resets that dimension to Chromium's own default.
func (c *identityClient) SetIdentity(timezone, locale string) (int, error) {
	timezone = strings.TrimSpace(timezone)
	locale = strings.TrimSpace(locale)

	c.mu.Lock()
	if c.conn == nil || c.version == "" {
		c.mu.Unlock()
		return 0, fmt.Errorf("identity not connected")
	}
	clearTimezone := c.timezone != "" && timezone == ""
	clearLocale := c.locale != "" && locale == ""
	c.timezone, c.locale = timezone, locale
	sessions := make([]string, 0, len(c.sessions))
	for id := range c.sessions {
		sessions = append(sessions, id)
	}
	c.mu.Unlock()

	for _, id := range sessions {
		if err := c.applyTo(id, clearTimezone, clearLocale); err != nil {
			return 0, err
		}
	}
	return len(sessions), nil
}

// warnOnce logs the first rejected override per connection: an unsupported
// target type failing once is a build quirk, not a reason to log per tab.
func (c *identityClient) warnOnce(err error) {
	c.mu.Lock()
	warned := c.warned
	c.warned = true
	c.mu.Unlock()
	if !warned {
		log.Printf("identity: ua override: %v", err)
	}
}

// uaTargetType: iframes get their own target when they are cross-origin and
// out-of-process, and they read the same navigator surfaces.
func uaTargetType(typ string) bool {
	return typ == "page" || typ == "webview" || typ == "iframe"
}

func (c *identityClient) send(method string, params map[string]interface{}) error {
	return c.sendSession(method, params, "")
}

func (c *identityClient) sendSession(method string, params map[string]interface{}, sessionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("identity not connected")
	}
	id := c.nextID
	msg := map[string]interface{}{
		"id":     id,
		"method": method,
	}
	c.nextID++
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	return c.conn.WriteJSON(msg)
}

// browserIdentity reads the browser websocket URL and the Chrome version from
// /json/version. The reported ws URL can name the bind address (0.0.0.0), so it
// is rewritten onto the host the HTTP probe succeeded on.
func browserIdentity(baseURL string) (version, wsURL string, err error) {
	resp, err := cdpHTTPClient.Get(baseURL + "/json/version")
	if err != nil {
		return "", "", fmt.Errorf("json/version: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("read json/version: %w", err)
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		UserAgent            string `json:"User-Agent"`
		Browser              string `json:"Browser"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", "", fmt.Errorf("parse json/version: %w", err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", "", fmt.Errorf("no browser websocket in json/version")
	}
	version = parseChromeVersion(v.UserAgent)
	if version == "" {
		version = parseChromeVersion(v.Browser)
	}
	if version == "" {
		return "", "", fmt.Errorf("no Chrome version in json/version")
	}
	ws, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil {
		return "", "", fmt.Errorf("parse browser ws url: %w", err)
	}
	if base, err := url.Parse(baseURL); err == nil && base.Host != "" {
		ws.Host = base.Host
	}
	return version, ws.String(), nil
}

// parseChromeVersion pulls the full Chrome version out of a UA/product string.
func parseChromeVersion(s string) string {
	m := chromeVersionRe.FindStringSubmatch(s)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// acceptLanguageFor derives the language list from a declared locale:
// "<locale>,<primary subtag>" ("sv-SE" → "sv-SE,sv"). No q-values, for the same
// reason as uaAcceptLanguage.
func acceptLanguageFor(locale string) string {
	primary := locale
	if i := strings.IndexByte(locale, '-'); i > 0 {
		primary = locale[:i]
	}
	return locale + "," + primary
}

// timezoneOverrideParams moves the page-visible clock (Date, Intl) to the
// declared IANA zone.
func timezoneOverrideParams(timezone string) map[string]interface{} {
	return map[string]interface{}{"timezoneId": timezone}
}

// localeOverrideParams changes Intl formatting and navigator.language for the
// target.
func localeOverrideParams(locale string) map[string]interface{} {
	return map[string]interface{}{"locale": locale}
}

// userAgentOverrideParams builds the default (en-US) override payload.
func userAgentOverrideParams(version string) map[string]interface{} {
	return userAgentParams(version, uaAcceptLanguage)
}

// userAgentParams builds the Emulation.setUserAgentOverride payload that makes
// the UA string, navigator.platform, navigator.languages and the UA-CH client
// hints describe the same client. Brand order and the GREASE entry match what
// this Chromium version emits itself (the GREASE token is seeded by the build;
// revisit it when the image's Chromium major moves). acceptLanguage carries the
// session's declared locale, or uaAcceptLanguage when there is none.
func userAgentParams(version, acceptLanguage string) map[string]interface{} {
	major := strings.SplitN(version, ".", 2)[0]
	return map[string]interface{}{
		"userAgent":      fmt.Sprintf("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/%s Safari/537.36", version),
		"platform":       uaPlatform,
		"acceptLanguage": acceptLanguage,
		"userAgentMetadata": map[string]interface{}{
			"brands": []map[string]interface{}{
				{"brand": "Not:A-Brand", "version": "8"},
				{"brand": "Chromium", "version": major},
				{"brand": "Google Chrome", "version": major},
			},
			"fullVersionList": []map[string]interface{}{
				{"brand": "Not:A-Brand", "version": "8.0.0.0"},
				{"brand": "Chromium", "version": version},
				{"brand": "Google Chrome", "version": version},
			},
			"fullVersion":     version,
			"platform":        "Linux",
			"platformVersion": "",
			"architecture":    "x86",
			"model":           "",
			"mobile":          false,
			"bitness":         "64",
			"wow64":           false,
		},
	}
}
