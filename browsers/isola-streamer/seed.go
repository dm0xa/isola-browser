package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Live custom data ("seed") injection: cookies, extra request headers and
// web storage pushed into an already-running page.
//
// Cookies go through Storage.setCookies on a one-shot BROWSER-level CDP
// connection: the jar is browser-wide, so every tab (including ones opened
// later) inherits them, and it never enables Network/Runtime on the input
// client or interferes with the multi-target capture client's attribution.
// Headers and storage are per page target, so they use the input client's
// page session (Page.enable is already on, and only Network/Runtime are the
// domains that would duplicate capture events).
//
// Everything here is reachable only on the loopback-published streamer port;
// the backend's session token is the authentication boundary (see
// api/handler_seed.go). Values are never logged.

const (
	seedMaxCookies        = 50
	seedMaxHeaders        = 20
	seedMaxStorageOrigins = 10
	seedMaxStorageItems   = 64
	seedMaxBodyBytes      = 64 << 10
	seedMaxValueBytes     = 4096
	seedMaxNameBytes      = 256
	seedCDPTimeout        = 5 * time.Second
)

type seedCookie struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	URL      string `json:"url,omitempty"`
	Path     string `json:"path,omitempty"`
	SameSite string `json:"same_site,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"http_only,omitempty"`
	Expires  int64  `json:"expires,omitempty"`
}

type seedHeader struct {
	Name    string   `json:"name"`
	Value   string   `json:"value"`
	Domains []string `json:"domains,omitempty"`
}

// seedStorage seeds one origin's web storage. Kind is "local" (default) or
// "session". An empty Origin means "wherever the page is now" — the common
// live case; an explicit origin must match the page's origin exactly.
type seedStorage struct {
	Kind   string            `json:"kind,omitempty"`
	Origin string            `json:"origin"`
	Items  map[string]string `json:"items"`
}

type seedPayload struct {
	Cookies []seedCookie  `json:"cookies,omitempty"`
	Headers []seedHeader  `json:"headers,omitempty"`
	Storage []seedStorage `json:"storage,omitempty"`
	// Reload reloads the page after applying so cookie-dependent SPA boot and
	// header changes take effect in one call. Never implied.
	Reload bool `json:"reload,omitempty"`
}

type seedResult struct {
	Cookies  int  `json:"cookies"`
	Headers  int  `json:"headers"`
	Storage  int  `json:"storage"`
	Reloaded bool `json:"reloaded"`
}

var (
	errSeedOriginMismatch = errors.New("page origin does not match")
	errSeedNoOrigin       = errors.New("page has no origin")
)

// seedScripts: init scripts registered for "session" storage entries (the only
// kind a reload would otherwise lose), keyed by the origin they apply to.
// Re-seeding one origin replaces that origin's scripts instead of accumulating
// them, and seeding a second origin does not remove the first one's.
var (
	seedScriptsMu sync.Mutex
	seedScripts   = map[string][]string{}

	// seedSessionItems is what sessionStorage should hold on every new document
	// of an origin: the union of every call that seeded it. Session storage is
	// per tab and wiped by a reload, so its init script is the only thing that
	// carries it forward — replacing the script with just the newest call's
	// keys would silently drop earlier ones.
	seedSessionItems = map[string]map[string]string{}
)

// normalizeSeed validates and canonicalizes a payload. Errors name the field
// and index only — never a cookie/header value.
func normalizeSeed(p seedPayload) (seedPayload, error) {
	out := seedPayload{Reload: p.Reload}

	if len(p.Cookies) > seedMaxCookies {
		return out, fmt.Errorf("cookies: at most %d allowed", seedMaxCookies)
	}
	for i, c := range p.Cookies {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return out, fmt.Errorf("cookies[%d]: name is required", i)
		}
		if len(name) > seedMaxNameBytes || !validHeaderName(name) {
			return out, fmt.Errorf("cookies[%d]: invalid name", i)
		}
		// Chromium rejects __Host-/__Secure- cookies that carry a Domain
		// attribute, and cookies here are always domain cookies. Rejecting the
		// name keeps a silent no-op from being reported as applied.
		if strings.HasPrefix(name, "__Host-") || strings.HasPrefix(name, "__Secure-") {
			return out, fmt.Errorf("cookies[%d]: __Host-/__Secure- cookies cannot be set as domain cookies", i)
		}
		if len(c.Value) > seedMaxValueBytes || hasControl(c.Value) {
			return out, fmt.Errorf("cookies[%d]: invalid value", i)
		}
		nc := c
		nc.Name = name
		nc.Value = c.Value
		if strings.TrimSpace(c.Domain) != "" && strings.TrimSpace(c.URL) != "" {
			// Both is ambiguous: the URL's path would be silently ignored.
			return out, fmt.Errorf("cookies[%d]: set domain or url, not both", i)
		}
		switch {
		case strings.TrimSpace(c.Domain) != "":
			d, err := normalizeHost(c.Domain)
			if err != nil {
				return out, fmt.Errorf("cookies[%d]: invalid domain", i)
			}
			nc.Domain = d
		case strings.TrimSpace(c.URL) != "":
			u, err := url.Parse(strings.TrimSpace(c.URL))
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
				return out, fmt.Errorf("cookies[%d]: invalid url", i)
			}
			d, err := normalizeHost(u.Hostname())
			if err != nil {
				return out, fmt.Errorf("cookies[%d]: invalid url host", i)
			}
			nc.Domain = d
			if strings.TrimSpace(c.Path) == "" {
				nc.Path = "/"
				if u.Path != "" {
					nc.Path = u.Path
				}
			}
			nc.URL = ""
		default:
			return out, fmt.Errorf("cookies[%d]: domain or url is required", i)
		}
		if strings.TrimSpace(c.Path) != "" {
			pth := strings.TrimSpace(c.Path)
			if !strings.HasPrefix(pth, "/") || strings.ContainsAny(pth, ";\r\n") {
				return out, fmt.Errorf("cookies[%d]: invalid path", i)
			}
			nc.Path = pth
		}
		if nc.Path == "" {
			nc.Path = "/"
		}
		if strings.TrimSpace(c.SameSite) != "" {
			ss, ok := canonicalSameSite(c.SameSite)
			if !ok {
				return out, fmt.Errorf("cookies[%d]: same_site must be strict, lax or none", i)
			}
			if ss == "None" && !c.Secure {
				return out, fmt.Errorf("cookies[%d]: same_site none requires secure", i)
			}
			nc.SameSite = ss
		}
		if c.Expires < 0 {
			nc.Expires = 0
		}
		out.Cookies = append(out.Cookies, nc)
	}

	if len(p.Headers) > seedMaxHeaders {
		return out, fmt.Errorf("headers: at most %d allowed", seedMaxHeaders)
	}
	seenHeaders := make(map[string]bool, len(p.Headers))
	for i, h := range p.Headers {
		name := strings.TrimSpace(h.Name)
		if !validHeaderName(name) {
			return out, fmt.Errorf("headers[%d]: invalid name", i)
		}
		// Network.setExtraHTTPHeaders takes a map keyed by header name, so a
		// repeated name would silently keep only one and the applied count
		// would lie. Compare canonical (case-insensitive) names.
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if seenHeaders[canonical] {
			return out, fmt.Errorf("headers[%d]: duplicate name", i)
		}
		seenHeaders[canonical] = true
		if len(h.Value) > seedMaxValueBytes || hasControl(h.Value) {
			return out, fmt.Errorf("headers[%d]: invalid value", i)
		}
		nh := seedHeader{Name: canonical, Value: h.Value}
		for _, d := range h.Domains {
			host, err := normalizeHost(d)
			if err != nil {
				return out, fmt.Errorf("headers[%d]: invalid domain", i)
			}
			nh.Domains = append(nh.Domains, host)
		}
		out.Headers = append(out.Headers, nh)
	}

	if len(p.Storage) > seedMaxStorageOrigins {
		return out, fmt.Errorf("storage: at most %d origins allowed", seedMaxStorageOrigins)
	}
	for i, st := range p.Storage {
		kind := strings.ToLower(strings.TrimSpace(st.Kind))
		switch kind {
		case "":
			kind = "local"
		case "local", "session":
		default:
			return out, fmt.Errorf("storage[%d]: kind must be local or session", i)
		}
		origin := ""
		if strings.TrimSpace(st.Origin) != "" {
			var err error
			origin, err = normalizeOrigin(st.Origin)
			if err != nil {
				return out, fmt.Errorf("storage[%d]: invalid origin", i)
			}
		}
		if len(st.Items) == 0 {
			return out, fmt.Errorf("storage[%d]: items is empty", i)
		}
		if len(st.Items) > seedMaxStorageItems {
			return out, fmt.Errorf("storage[%d]: at most %d items allowed", i, seedMaxStorageItems)
		}
		items := make(map[string]string, len(st.Items))
		for k, v := range st.Items {
			if strings.TrimSpace(k) == "" || hasControl(k) || len(k) > seedMaxNameBytes {
				return out, fmt.Errorf("storage[%d]: invalid key", i)
			}
			if len(v) > seedMaxValueBytes || hasControl(v) {
				return out, fmt.Errorf("storage[%d]: invalid value", i)
			}
			items[k] = v
		}
		out.Storage = append(out.Storage, seedStorage{Kind: kind, Origin: origin, Items: items})
	}

	return out, nil
}

func hasControl(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func canonicalSameSite(v string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "strict":
		return "Strict", true
	case "lax":
		return "Lax", true
	case "none":
		return "None", true
	}
	return "", false
}

// validHeaderName is RFC 7230's token set; anything else could smuggle a
// second header through the CDP header map.
func validHeaderName(s string) bool {
	if s == "" || len(s) > seedMaxNameBytes {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// normalizeHost validates a cookie/header domain. Single-label hosts are
// allowed on purpose: a sandbox can legitimately be pointed at an intranet name
// or a Docker service name.
func normalizeHost(v string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(v))
	h = strings.TrimPrefix(h, ".")
	if h == "" || len(h) > 253 || strings.ContainsAny(h, "/:@ \t\r\n") {
		return "", fmt.Errorf("invalid host")
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return "", fmt.Errorf("invalid host")
		}
		for i := range len(label) {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("invalid host")
			}
		}
	}
	return h, nil
}

// normalizeOrigin reduces "https://Host:443/app?x" to "https://host" — the
// value location.origin reports, so mismatch detection is exact.
func normalizeOrigin(v string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(v))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("invalid origin")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("invalid origin")
	}
	origin := u.Scheme + "://" + host
	if port := u.Port(); port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") {
		origin += ":" + port
	}
	return origin, nil
}

// applySeed pushes a validated payload into the running page. Partial success
// is an error: the caller gets the CDP failure, never a silently missing
// cookie. Counts describe what was attempted by the failing step.
func applySeed(p seedPayload) (seedResult, error) {
	var res seedResult

	// Resolve and check the page origin BEFORE mutating anything: a storage
	// entry for another origin must not leave cookies or headers behind while
	// the call reports a 409.
	pageOriginValue := ""
	if len(p.Storage) > 0 {
		origin, err := pageOrigin()
		if err != nil {
			return res, err
		}
		if origin == "" || origin == "null" {
			return res, fmt.Errorf("%w: navigate to the target origin first", errSeedNoOrigin)
		}
		for _, st := range p.Storage {
			if st.Origin != "" && st.Origin != origin {
				return res, fmt.Errorf("%w: page is on %s", errSeedOriginMismatch, origin)
			}
		}
		pageOriginValue = origin
	}

	if len(p.Cookies) > 0 {
		if err := setBrowserCookies(p.Cookies); err != nil {
			return res, err
		}
		res.Cookies = len(p.Cookies)
	}

	if len(p.Headers) > 0 {
		n, err := applyHeaders(p.Headers)
		res.Headers = n
		if err != nil {
			return res, err
		}
	}

	if len(p.Storage) > 0 {
		n, err := applyStorage(p.Storage, pageOriginValue)
		res.Storage = n
		if err != nil {
			return res, err
		}
	}

	if p.Reload {
		if _, err := cdpClient.callWithResult("Page.reload", nil, seedCDPTimeout); err != nil {
			return res, fmt.Errorf("Page.reload: %w", err)
		}
		res.Reloaded = true
	}
	return res, nil
}

func cdpPageAvailable() bool {
	return cdpClient != nil && cdpClient.Available()
}

func pageOrigin() (string, error) {
	if !cdpPageAvailable() {
		return "", fmt.Errorf("cdp not available")
	}
	return cdpClient.evaluateString("location.origin")
}

func pageHostname() (string, error) {
	if !cdpPageAvailable() {
		return "", fmt.Errorf("cdp not available")
	}
	return cdpClient.evaluateString("location.hostname")
}

// applyHeaders sets the page target's extra request headers. The set is
// target-wide: Chromium applies it to every request the tab issues, including
// subresources and third-party hosts, so `domains` only decides whether a
// header is included based on the current main-frame host — it is not a
// per-URL scope. Network.setExtraHTTPHeaders replaces the whole set, so a
// second call supersedes the first (never merges two sets the caller believed
// were both active), and the set is lost when the input client reconnects.
// Returns how many headers were applied.
func applyHeaders(headers []seedHeader) (int, error) {
	host, err := pageHostname()
	if err != nil {
		return 0, err
	}
	host = strings.ToLower(host)
	out := make(map[string]string, len(headers))
	for _, h := range headers {
		if len(h.Domains) == 0 || hostMatches(host, h.Domains) {
			out[h.Name] = h.Value
		}
	}
	if len(out) == 0 {
		return 0, nil
	}
	// Chromium only applies extra headers through the DevTools instrumentation
	// hook, which Network.enable installs on this session. Without it the
	// command succeeds and the headers never reach the wire (observed live).
	// Enabling Network here is otherwise harmless: CDPClient.readPump drops
	// every event (only the multi-target capture client taps Network), so this
	// cannot double-count traffic.
	if _, err := cdpClient.callWithResult("Network.enable", map[string]interface{}{}, seedCDPTimeout); err != nil {
		return 0, fmt.Errorf("Network.enable: %w", err)
	}
	if _, err := cdpClient.callWithResult("Network.setExtraHTTPHeaders",
		map[string]interface{}{"headers": out}, seedCDPTimeout); err != nil {
		return 0, fmt.Errorf("Network.setExtraHTTPHeaders: %w", err)
	}
	return len(out), nil
}

func hostMatches(host string, domains []string) bool {
	for _, d := range domains {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

// applyStorage writes the entries into the page's current origin. Entries for
// another origin are a hard error: silently applying them to the wrong origin
// would put a token where the target page never sees it.
func applyStorage(entries []seedStorage, origin string) (int, error) {
	sessionItems := map[string]string{}
	applied := 0
	for _, st := range entries {
		if st.Origin != "" && st.Origin != origin {
			return applied, fmt.Errorf("%w: page is on %s", errSeedOriginMismatch, origin)
		}
		storageName := "localStorage"
		if st.Kind == "session" {
			storageName = "sessionStorage"
		}
		items, err := json.Marshal(st.Items)
		if err != nil {
			return applied, fmt.Errorf("storage: encode items")
		}
		expr := fmt.Sprintf(
			`(function(){try{var items=%s;var s=%s;for(var k in items){s.setItem(k,items[k]);}return "ok";}catch(e){return "error:"+(e&&e.name?e.name:"unknown");}})()`,
			items, storageName)
		out, err := cdpClient.evaluateStringTimeout(expr, seedCDPTimeout)
		if err != nil {
			return applied, fmt.Errorf("%s.setItem: %w", storageName, err)
		}
		if strings.HasPrefix(out, "error:") {
			return applied, fmt.Errorf("%s.setItem: %s", storageName, strings.TrimPrefix(out, "error:"))
		}
		applied += len(st.Items)
		if st.Kind == "session" {
			for k, v := range st.Items {
				sessionItems[k] = v
			}
		}
	}
	if len(sessionItems) > 0 {
		// One script per origin, built from the union of every call: session
		// storage is per tab, so the script is what carries it across reloads.
		if err := registerSeedScripts(origin, []string{sessionInitScript(origin, mergeSessionItems(origin, sessionItems))}); err != nil {
			return applied, err
		}
	}
	return applied, nil
}

// mergeSessionItems records this call's keys for the origin and returns the
// whole set, so a later call adds keys instead of dropping the earlier ones.
func mergeSessionItems(origin string, add map[string]string) map[string]string {
	seedScriptsMu.Lock()
	defer seedScriptsMu.Unlock()
	merged := seedSessionItems[origin]
	if merged == nil {
		merged = make(map[string]string, len(add))
		seedSessionItems[origin] = merged
	}
	for k, v := range add {
		merged[k] = v
	}
	out := make(map[string]string, len(merged))
	for k, v := range merged {
		out[k] = v
	}
	return out
}

// sessionInitScript runs before page scripts in the next document. Only
// sessionStorage needs it: localStorage already persists in the origin.
func sessionInitScript(origin string, items map[string]string) string {
	encoded, _ := json.Marshal(items)
	quotedOrigin, _ := json.Marshal(origin)
	return fmt.Sprintf(
		`(function(){try{if(location.origin!==%s)return;var items=%s;for(var k in items){sessionStorage.setItem(k,items[k]);}}catch(e){}})()`,
		quotedOrigin, encoded)
}

// registerSeedScripts replaces the init scripts previously registered for one
// origin. A long-lived session cannot accumulate one set per seed call, and
// seeding a second origin leaves the first origin's scripts in place.
func registerSeedScripts(origin string, sources []string) error {
	seedScriptsMu.Lock()
	previous := seedScripts[origin]
	delete(seedScripts, origin)
	seedScriptsMu.Unlock()

	for _, id := range previous {
		_ = cdpClient.call("Page.removeScriptToEvaluateOnNewDocument", map[string]interface{}{"identifier": id})
	}
	for _, src := range sources {
		raw, err := cdpClient.callWithResult("Page.addScriptToEvaluateOnNewDocument",
			map[string]interface{}{"source": src}, seedCDPTimeout)
		if err != nil {
			return fmt.Errorf("Page.addScriptToEvaluateOnNewDocument: %w", err)
		}
		var res struct {
			Identifier string `json:"identifier"`
		}
		if json.Unmarshal(raw, &res) == nil && res.Identifier != "" {
			seedScriptsMu.Lock()
			seedScripts[origin] = append(seedScripts[origin], res.Identifier)
			seedScriptsMu.Unlock()
		}
	}
	return nil
}

// setBrowserCookies uses a short-lived browser-level CDP connection. Browser
// level because Storage.setCookies is a jar operation that must survive tab
// changes and new tabs, and because it needs no Network.enable anywhere.
func setBrowserCookies(cookies []seedCookie) error {
	wsURL, err := browserDebuggerURL()
	if err != nil {
		return err
	}
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("cdp browser dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(seedCDPTimeout))

	params := make([]map[string]any, 0, len(cookies))
	for _, c := range cookies {
		pc := map[string]any{
			"name":   c.Name,
			"value":  c.Value,
			"domain": c.Domain,
			"path":   c.Path,
		}
		if c.Secure {
			pc["secure"] = true
		}
		if c.HTTPOnly {
			pc["httpOnly"] = true
		}
		if c.SameSite != "" {
			pc["sameSite"] = c.SameSite
		}
		if c.Expires > 0 {
			pc["expires"] = c.Expires
		}
		params = append(params, pc)
	}
	if err := conn.WriteJSON(map[string]any{
		"id":     1,
		"method": "Storage.setCookies",
		"params": map[string]any{"cookies": params},
	}); err != nil {
		return fmt.Errorf("Storage.setCookies: write: %w", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(seedCDPTimeout))
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("Storage.setCookies: read: %w", err)
		}
		var resp cdpResponse
		if json.Unmarshal(raw, &resp) != nil || resp.ID != 1 {
			continue
		}
		if resp.Error != nil {
			// Do not interpolate the browser's message: Chromium can echo the
			// offending value, and cookie values must never reach a log line.
			return fmt.Errorf("Storage.setCookies: cdp error %d", resp.Error.Code)
		}
		return nil
	}
}

func browserDebuggerURL() (string, error) {
	base := os.Getenv("CDP_URL")
	if base == "" {
		base = "http://127.0.0.1:9222"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(base + "/json/version")
	if err != nil {
		return "", fmt.Errorf("cdp version: %w", err)
	}
	defer resp.Body.Close()
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", fmt.Errorf("cdp version: %w", err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("cdp version: no browser websocket")
	}
	return v.WebSocketDebuggerURL, nil
}

// truncateForLog caps a browser-supplied message; Chromium sometimes echoes
// the offending field, and cookie values must never reach a log line.
func truncateForLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// handleSeed applies custom data to the running page. Same trust boundary as
// /click, /type and /navigate (loopback-published, authenticated by the
// backend's session token at the API layer).
func handleSeed(w http.ResponseWriter, r *http.Request) {
	// Counts and errors only, but the endpoint is documented as no-store on
	// every response; set it before anything can write.
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, seedMaxBodyBytes+1))
	if err != nil {
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	if len(body) > seedMaxBodyBytes {
		http.Error(w, fmt.Sprintf("payload exceeds %d bytes", seedMaxBodyBytes), http.StatusRequestEntityTooLarge)
		return
	}
	var payload seedPayload
	dec := json.NewDecoder(bytes.NewReader(body))
	// Unknown fields are rejected: a typo like "httpOnly" would otherwise
	// silently drop a cookie attribute that matters for security.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		http.Error(w, "invalid json: "+truncateForLog(err.Error()), http.StatusBadRequest)
		return
	}
	if dec.More() {
		http.Error(w, "invalid json: trailing data", http.StatusBadRequest)
		return
	}
	norm, err := normalizeSeed(payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(norm.Cookies) == 0 && len(norm.Headers) == 0 && len(norm.Storage) == 0 && !norm.Reload {
		// A no-op would answer 200 with all-zero counts, which reads as success
		// to a caller that sent a typo'd field. Say what happened instead.
		http.Error(w, "nothing to apply", http.StatusBadRequest)
		return
	}
	if !cdpPageAvailable() {
		http.Error(w, "cdp not available", http.StatusServiceUnavailable)
		return
	}
	res, err := applySeed(norm)
	if err != nil {
		switch {
		case errors.Is(err, errSeedOriginMismatch), errors.Is(err, errSeedNoOrigin):
			http.Error(w, err.Error(), http.StatusConflict)
		case strings.Contains(err.Error(), "cdp not available"), strings.Contains(err.Error(), "cdp not connected"):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			log.Printf("seed: apply failed: %v", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(res)
}
