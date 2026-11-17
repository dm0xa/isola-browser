package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Multi-target CDP capture: attaches to the browser endpoint with
// Target.setAutoAttach (flatten), so every page target — including later tabs —
// gets a session and per-tab attribution. Input stays on its own single-target client.

type captureTarget struct {
	sessionID string
	targetID  string
	url       string
	title     string
	typ       string
}

type captureClient struct {
	baseURL string

	mu      sync.Mutex
	conn    *websocket.Conn
	nextID  int
	targets map[string]*captureTarget // sessionId → attached page target
	// Command id → method name, so a browser-side rejection is logged with the command that failed.
	pending map[int]string
	// One-shot result handlers (e.g. Network.getResponseBody — bodies are per-request asks).
	callbacks map[int]func(json.RawMessage)
	// Reload the first real page once so the START_URL navigation lands under capture.
	reloadPending bool
}

// activeCapture is the running browser-level capture client, published so the
// network tap (netlog.go) can ask it for response bodies: bodies ride no event
// stream, so they can only be fetched by request id on the owning session.
var activeCapture *captureClient

func newCaptureClient() *captureClient {
	baseURL := os.Getenv("CDP_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:9222"
	}
	return &captureClient{
		baseURL:       baseURL,
		nextID:        1,
		targets:       make(map[string]*captureTarget),
		pending:       make(map[int]string),
		callbacks:     make(map[int]func(json.RawMessage)),
		reloadPending: true,
	}
}

func (c *captureClient) Start() {
	for {
		if err := c.connect(); err != nil {
			log.Printf("capture: connect: %v (retry 2s)", err)
			time.Sleep(2 * time.Second)
			continue
		}
		log.Printf("capture: auto-attach")
		c.readPump()
		c.dropConn()
		log.Printf("capture: reconnecting")
		time.Sleep(2 * time.Second)
	}
}

// connect dials the browser-level websocket (not a page target) and turns on
// auto-attach for every current and future target.
func (c *captureClient) connect() error {
	resp, err := http.Get(c.baseURL + "/json/version")
	if err != nil {
		return fmt.Errorf("json/version: %w", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read json/version: %w", err)
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(body, &v) != nil || v.WebSocketDebuggerURL == "" {
		return fmt.Errorf("no browser websocket in /json/version")
	}

	// Chromium echoes the bind address it was launched with, which may be
	// 0.0.0.0. Dial the same host the HTTP probe just succeeded on.
	ws, err := url.Parse(v.WebSocketDebuggerURL)
	if err != nil {
		return fmt.Errorf("parse browser ws url: %w", err)
	}
	if base, err := url.Parse(c.baseURL); err == nil && base.Host != "" {
		ws.Host = base.Host
	}

	conn, _, err := websocket.DefaultDialer.Dial(ws.String(), nil)
	if err != nil {
		return fmt.Errorf("dial browser ws: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.nextID = 1
	c.targets = make(map[string]*captureTarget)
	c.callbacks = make(map[int]func(json.RawMessage))
	c.mu.Unlock()

	// flatten: sessions ride this socket (events carry sessionId, commands can
	// address one), instead of the nested sendMessageToTarget protocol.
	return c.send("Target.setAutoAttach", map[string]interface{}{
		"autoAttach":             true,
		"waitForDebuggerOnStart": false,
		"flatten":                true,
	})
}

func (c *captureClient) readPump() {
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
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		if env.Method == "" {
			// No method ⇒ a command response. Log rejections with the
			// command's method name; results are dropped.
			c.onResponse(raw)
			continue
		}
		switch env.Method {
		case "Target.attachedToTarget":
			// The session id is in params here — the envelope's top-level one is empty,
			// and reading it there silently disables all capture.
			c.onAttached(env.Params)
		case "Target.detachedFromTarget":
			c.onDetached(env.Params)
		case "Target.targetInfoChanged":
			c.onInfoChanged(env.SessionID, env.Params)
		default:
			c.dispatch(env.Method, env.SessionID, env.Params)
		}
	}
}

func (c *captureClient) onAttached(params json.RawMessage) {
	var p struct {
		SessionID  string `json:"sessionId"`
		TargetInfo struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
			Title    string `json:"title"`
		} `json:"targetInfo"`
	}
	if json.Unmarshal(params, &p) != nil || p.TargetInfo.TargetID == "" || p.SessionID == "" {
		return
	}
	// Only pages carry the traffic we capture; workers, iframes and the
	// browser target would just add sessions and noise.
	if (p.TargetInfo.Type != "page" && p.TargetInfo.Type != "webview") ||
		strings.HasPrefix(p.TargetInfo.URL, "devtools://") {
		_ = c.sendSession("Target.detachFromTarget", map[string]interface{}{"sessionId": p.SessionID}, p.SessionID)
		return
	}

	c.mu.Lock()
	c.targets[p.SessionID] = &captureTarget{
		sessionID: p.SessionID,
		targetID:  p.TargetInfo.TargetID,
		url:       p.TargetInfo.URL,
		title:     p.TargetInfo.Title,
		typ:       p.TargetInfo.Type,
	}
	reload := false
	if c.reloadPending && !isBlankTargetURL(p.TargetInfo.URL) {
		c.reloadPending = false
		reload = true
	}
	c.mu.Unlock()

	if err := c.sendSession("Network.enable", map[string]interface{}{}, p.SessionID); err != nil {
		log.Printf("capture: Network.enable: %v", err)
	}
	if err := c.sendSession("Runtime.enable", map[string]interface{}{}, p.SessionID); err != nil {
		log.Printf("capture: Runtime.enable: %v", err)
	}
	log.Printf("capture: attached %s (%s)", shortTargetID(p.TargetInfo.TargetID), p.TargetInfo.URL)
	if reload {
		if err := c.sendSession("Page.reload", nil, p.SessionID); err != nil {
			log.Printf("capture: Page.reload: %v", err)
		} else {
			log.Printf("capture: reloaded")
		}
	}
}

func (c *captureClient) onDetached(params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(params, &p) != nil || p.SessionID == "" {
		return
	}
	c.mu.Lock()
	delete(c.targets, p.SessionID)
	c.mu.Unlock()
}

func (c *captureClient) onInfoChanged(sessionID string, params json.RawMessage) {
	var p struct {
		TargetInfo struct {
			TargetID string `json:"targetId"`
			URL      string `json:"url"`
			Title    string `json:"title"`
		} `json:"targetInfo"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	c.mu.Lock()
	t := c.targets[sessionID]
	if t == nil && p.TargetInfo.TargetID != "" {
		// Browser-level targetInfoChanged carries no envelope sessionId —
		// fall back to matching a target we already track.
		for _, cand := range c.targets {
			if cand.targetID == p.TargetInfo.TargetID {
				t = cand
				break
			}
		}
	}
	if t != nil {
		if p.TargetInfo.URL != "" {
			t.url = p.TargetInfo.URL
		}
		t.title = p.TargetInfo.Title
	}
	c.mu.Unlock()
}

// dispatch routes one capture event to the matching tap. Events arrive with
// the sessionId of the target they belong to, which is what makes per-tab
// attribution possible at all.
func (c *captureClient) dispatch(method, sessionID string, params json.RawMessage) {
	if !isCaptureEventType(method) {
		return
	}
	c.mu.Lock()
	t := c.targets[sessionID]
	c.mu.Unlock()
	if t == nil {
		return
	}
	tab := tabForTarget(t.url, t.title)
	routeCaptureEvent(method, params, t.targetID, sessionID, tab)
}

// routeCaptureEvent: ExtraInfo is both a Network.* event and the cookie source, so
// independent ifs, not a switch. sessionID passes through so a tap can command the target.
func routeCaptureEvent(method string, params json.RawMessage, targetID, sessionID string, tab int) {
	if isNetEventType(method) {
		netMonitorTap(targetID, sessionID, method, params, tab)
	}
	if isCookieEventType(method) {
		cookieTap(params)
	}
	if isConsoleEventType(method) {
		consoleTap(method, params, tab)
	}
}

func (c *captureClient) send(method string, params map[string]interface{}) error {
	return c.sendSession(method, params, "")
}

func (c *captureClient) sendSession(method string, params map[string]interface{}, sessionID string) error {
	return c.sendSessionCallback(method, params, sessionID, nil)
}

// sendSessionCallback: sendSession plus a one-shot result handler, consumed exactly once.
func (c *captureClient) sendSessionCallback(method string, params map[string]interface{}, sessionID string, cb func(json.RawMessage)) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("capture not connected")
	}
	id := c.nextID
	msg := map[string]interface{}{
		"id":     id,
		"method": method,
	}
	c.pending[id] = method
	if cb != nil {
		c.callbacks[id] = cb
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

// requestResponseBody: fetch one response body for hashing; must be called while the
// request is still in the browser's response cache (right after loadingFinished).
func (c *captureClient) requestResponseBody(sessionID, requestID string, cb func([]byte)) error {
	return c.sendSessionCallback(
		"Network.getResponseBody",
		map[string]interface{}{"requestId": requestID},
		sessionID,
		func(raw json.RawMessage) {
			var res struct {
				Body          string `json:"body"`
				Base64Encoded bool   `json:"base64Encoded"`
			}
			if len(raw) == 0 || json.Unmarshal(raw, &res) != nil || res.Body == "" {
				cb(nil)
				return
			}
			if res.Base64Encoded {
				b, err := base64.StdEncoding.DecodeString(res.Body)
				if err != nil {
					cb(nil)
					return
				}
				cb(b)
				return
			}
			cb([]byte(res.Body))
		},
	)
}

// onResponse handles a command response (no method). Browser-side rejections
// (e.g. "'Network.enable' wasn't found") used to vanish silently here; log
// them with the originating command so capture failures are diagnosable.
func (c *captureClient) onResponse(raw []byte) {
	var r struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &r) != nil || r.ID == 0 {
		return
	}
	c.mu.Lock()
	cb := c.callbacks[r.ID]
	delete(c.callbacks, r.ID)
	method := c.pending[r.ID]
	delete(c.pending, r.ID)
	c.mu.Unlock()
	if cb != nil {
		// nil result when the command was rejected.
		cb(r.Result)
		return
	}
	if r.Error != nil {
		log.Printf("capture: cdp error %s: %s", method, r.Error.Message)
	}
}

func (c *captureClient) dropConn() {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.targets = make(map[string]*captureTarget)
	c.pending = make(map[int]string)
	c.callbacks = make(map[int]func(json.RawMessage))
	c.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

// isCaptureEventType is the cheap prefix check before any JSON work.
func isCaptureEventType(method string) bool {
	return isNetEventType(method) || isCookieEventType(method) || isConsoleEventType(method)
}

func isBlankTargetURL(u string) bool {
	return u == "" || u == "about:blank" || u == "chrome://newtab/" || u == "edge://newtab/"
}

// shortTargetID keeps CDP request ids readable in the UI while staying unique
// per target: request ids are per-target, so two tabs can legitimately both
// have a request "1".
func shortTargetID(targetID string) string {
	if len(targetID) > 8 {
		return targetID[:8]
	}
	return targetID
}
