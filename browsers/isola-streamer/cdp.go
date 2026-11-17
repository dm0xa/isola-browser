package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// preferredRetryWindow is how long connect() keeps retrying to find the
// preferred (active-tab) target after a tab switch before falling back to
// the first page target. Covers /json/list races and short redirects.
const preferredRetryWindow = 1500 * time.Millisecond

type cdpTarget struct {
	Type                 string `json:"type"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	URL                  string `json:"url"`
}

type cdpResponse struct {
	ID     int             `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type CDPClient struct {
	baseURL string

	// preferredURL (when set) makes connect() attach to the page target
	// matching this URL prefix — the active tab in multi-tab mode.
	preferredURL string
	// preferredSetAt records when preferredURL was last set. For a short
	// window after a tab switch, connect() retries instead of falling back
	// to the first page target (which could be the WRONG tab when the
	// tab's URL prefix went stale due to a redirect).
	preferredSetAt time.Time

	wakeCh chan struct{}

	mu        sync.Mutex
	conn      *websocket.Conn
	connected bool
	nextID    int
	pending   map[int]chan cdpResponse

	// attachedURL is the URL of the page target input is currently bound to,
	// recorded at attach time. Streaming video does not follow this binding
	// (frames come from the Electron window's offscreen producer, which always
	// shows the *active* tab), so a stale binding looks to the viewer like a
	// live page whose mouse and keyboard do nothing. See input_target.go for
	// the watchdog that repairs it.
	attachedURL string

	lastMouseX float64
	lastMouseY float64

	// suppressRightUp: set when a right press was swallowed because it landed
	// inside the current text selection (a synthetic right press collapses the
	// selection the select_range path created). The matching release — and
	// right-pinned moves — are swallowed too so the page never sees a
	// half right-click.
	suppressRightUp bool

	// frames routes pointer and key events into cross-origin iframes. The
	// page-target socket this client is attached to cannot deliver input
	// there (see iframe_input.go).
	frames iframeState
}

func newCDPClient() *CDPClient {
	baseURL := os.Getenv("CDP_URL")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:9222"
	}
	return &CDPClient{
		baseURL: baseURL,
		pending: make(map[int]chan cdpResponse),
		wakeCh:  make(chan struct{}, 1),
		frames:  newIframeState(),
	}
}

// SetPreferredTarget points future reconnects at the page whose URL starts
// with the given prefix (the active tab). Takes effect on the next
// (re)connect; call ForceReconnect to apply immediately.
func (c *CDPClient) SetPreferredTarget(urlPrefix string) {
	c.mu.Lock()
	if c.preferredURL == urlPrefix {
		// Unchanged: leave preferredSetAt alone. The watchdog calls this every
		// tick while the browser is unreachable, and re-stamping it each time
		// would keep connectRetryDelay at its 100 ms "tab switch is racing us"
		// value forever instead of backing off.
		c.mu.Unlock()
		return
	}
	c.preferredURL = urlPrefix
	c.preferredSetAt = time.Now()
	c.mu.Unlock()
}

// ForceReconnect drops the current CDP connection and nudges the Start loop
// to reattach immediately (honouring SetPreferredTarget).
func (c *CDPClient) ForceReconnect() {
	c.mu.Lock()
	c.connected = false
	if c.conn != nil {
		c.conn.Close()
	}
	c.mu.Unlock()
	c.notifyWake()
}

// notifyWake unblocks an in-progress reconnect delay.
func (c *CDPClient) notifyWake() {
	select {
	case c.wakeCh <- struct{}{}:
	default:
	}
}

// waitReconnectDelay sleeps for d, but returns early when a reconnect was
// requested (ForceReconnect), so tab switches reattach in ~ms instead of
// leaving a ~1s input blackout.
func (c *CDPClient) waitReconnectDelay(d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-c.wakeCh:
	}
}

// connectRetryDelay picks the backoff for a failed connect: quick retries
// while a preferred target is still expected (tab switch race), slower
// when the browser is genuinely unreachable.
func (c *CDPClient) connectRetryDelay() time.Duration {
	c.mu.Lock()
	preferred := c.preferredURL
	setAt := c.preferredSetAt
	c.mu.Unlock()
	if preferred != "" && time.Since(setAt) < preferredRetryWindow {
		return 100 * time.Millisecond
	}
	return 2 * time.Second
}

func (c *CDPClient) Start() {
	for {
		if err := c.connect(); err != nil {
			delay := c.connectRetryDelay()
			log.Printf("cdp: connect: %v (retry %s)", err, delay)
			c.waitReconnectDelay(delay)
			continue
		}
		log.Printf("cdp: connected")
		c.readPump()
		c.mu.Lock()
		c.connected = false
		if c.conn != nil {
			c.conn.Close()
			c.conn = nil
		}
		c.mu.Unlock()
		c.frames.close()
		log.Printf("cdp: reconnecting")
		c.waitReconnectDelay(250 * time.Millisecond)
	}
}

func (c *CDPClient) Available() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected && c.conn != nil
}

// AttachedURL reports the URL of the page target input is bound to, or ""
// while detached. Exposed to the debug overlay so "clicks do nothing" can be
// told apart from "input never arrived".
func (c *CDPClient) AttachedURL() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.conn == nil {
		return ""
	}
	return c.attachedURL
}

// AlignTarget makes the input binding follow the active tab, and reports
// whether it had to reattach. A binding that already serves activeURL is left
// alone (a tab navigating within its origin keeps the same target, so
// reattaching per navigation would drop input for no reason); a stale or
// foreign binding is repaired here instead of waiting for the user to switch
// tabs by hand. Called from the watchdog in input_target.go.
func (c *CDPClient) AlignTarget(activeURL string) bool {
	if activeURL == "" {
		return false
	}
	if matchTabURL(c.AttachedURL(), activeURL) {
		return false
	}
	if !c.hasTargetMatching(activeURL) {
		// Nothing attachable serves the active tab (it is mid-navigation, the
		// runner's URL has drifted from /json/list, ...). Keep the preference
		// armed so the next real reconnect binds correctly, but leave the
		// connection alone: reattaching now would only bounce input without
		// ever converging.
		c.SetPreferredTarget(activeURL)
		return false
	}
	// Re-point before reconnecting, so the reconnect cannot pick the old target
	// again.
	c.SetPreferredTarget(activeURL)
	c.ForceReconnect()
	return true
}

// readPump is the single reader for the CDP websocket: it routes command
// replies to waiting callers and discards events.
func (c *CDPClient) readPump() {
	c.mu.Lock()
	conn := c.conn
	pending := c.pending
	c.mu.Unlock()
	if conn == nil {
		return
	}
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     int             `json:"id"`
		}
		json.Unmarshal(raw, &envelope)
		if envelope.ID == 0 {
			continue // event
		}
		var resp cdpResponse
		if json.Unmarshal(raw, &resp) != nil {
			continue
		}
		c.mu.Lock()
		ch, ok := pending[resp.ID]
		if ok {
			delete(pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
			close(ch)
		}
	}
}

// cdpHTTPClient bounds the /json/list probe. connect() and the input-target
// watchdog both run it (the watchdog on a timer), so an unbounded request could
// park self-heal indefinitely.
var cdpHTTPClient = &http.Client{Timeout: 2 * time.Second}

// listTargets fetches Chromium's page-target table.
func (c *CDPClient) listTargets() ([]cdpTarget, error) {
	resp, err := cdpHTTPClient.Get(c.baseURL + "/json/list")
	if err != nil {
		return nil, fmt.Errorf("json/list: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read json/list: %w", err)
	}
	var targets []cdpTarget
	if err := json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("parse json/list: %w", err)
	}
	return targets, nil
}

// hasTargetMatching reports whether /json/list currently exposes a page target
// whose URL starts with prefix — i.e. whether re-attaching could actually reach
// that page. AlignTarget uses it to tell "bound to the wrong page" (repair it)
// apart from "the tab the runner reports has no attachable target yet"
// (reconnecting would only bounce input without ever converging).
func (c *CDPClient) hasTargetMatching(prefix string) bool {
	if prefix == "" {
		return false
	}
	targets, err := c.listTargets()
	if err != nil {
		return false
	}
	for _, t := range targets {
		if t.WebSocketDebuggerURL == "" || (t.Type != "page" && t.Type != "webview") {
			continue
		}
		if strings.HasPrefix(t.URL, prefix) {
			return true
		}
	}
	return false
}

func (c *CDPClient) connect() error {
	targets, err := c.listTargets()
	if err != nil {
		return err
	}

	c.mu.Lock()
	preferred := c.preferredURL
	setAt := c.preferredSetAt
	c.mu.Unlock()

	var wsURL, fallback string
	// targetURL is the URL of the target this attach ends up bound to, so
	// AttachedURL() can report which page input actually drives.
	var targetURL string
	for _, t := range targets {
		if t.WebSocketDebuggerURL == "" {
			continue
		}
		if t.Type != "page" && t.Type != "webview" {
			continue
		}
		if strings.HasPrefix(t.URL, "devtools://") {
			continue
		}
		if preferred != "" && strings.HasPrefix(t.URL, preferred) {
			wsURL, targetURL = t.WebSocketDebuggerURL, t.URL
			break
		}
		if fallback == "" && t.URL != "" && t.URL != "about:blank" {
			fallback, targetURL = t.WebSocketDebuggerURL, t.URL
		}
	}
	if wsURL == "" {
		if preferred != "" && time.Since(setAt) < preferredRetryWindow {
			// A tab switch just happened but the expected target isn't in
			// /json/list yet (or its URL prefix went stale). Retrying is
			// better than attaching to an arbitrary page target and sending
			// the user's clicks to the wrong tab.
			return fmt.Errorf("preferred target %q not in /json/list yet, retrying", preferred)
		}
		wsURL = fallback
	}
	if wsURL == "" {
		return fmt.Errorf("no page target in json/list")
	}
	if preferred != "" && !strings.HasPrefix(targetURL, preferred) {
		// The preferred (active-tab) target is not in /json/list. We are about
		// to bind somewhere else, and input_target.go must notice — otherwise
		// clicks and keystrokes go to a page the viewer cannot see.
		log.Printf("cdp: preferred %q missing, fallback %q", preferred, targetURL)
	}
	log.Printf("cdp: attach %s url=%q", wsURL, targetURL)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial page ws: %w", err)
	}

	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.nextID = 1
	c.attachedURL = targetURL
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()

	// NOTE: Input.enable is intentionally NOT called — it's not needed for
	// Input.dispatchMouseEvent (which works without it) and on some Electron/
	// Chromium builds it puts the input dispatch pipeline into a state where
	// drag-selection via mouseMoved+buttons silently fails.
	_ = c.call("Page.enable", nil)
	// Network/Runtime capture deliberately does NOT happen on this connection:
	// it only ever saw the active tab. The multi-target capture client
	// (cdp_capture.go) attaches to every target instead — enabling the domains
	// here too would deliver every captured event twice.
	_ = c.ensureFocus()
	return nil
}

// netMonitorEnabled reports the NET_MONITOR env (set per session when the
// viewer opted into the network panel before launch).
func netMonitorEnabled() bool {
	return os.Getenv("NET_MONITOR") == "1"
}

func (c *CDPClient) ensureFocus() error {
	_ = c.send("Emulation.setFocusEmulationEnabled", map[string]interface{}{
		"enabled": true,
	})
	return c.send("Page.bringToFront", nil)
}

func (c *CDPClient) call(method string, params map[string]interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return fmt.Errorf("cdp not connected")
	}
	id := c.nextID
	c.nextID++
	msg := map[string]interface{}{
		"id":     id,
		"method": method,
	}
	if params != nil {
		msg["params"] = params
	}
	return c.conn.WriteJSON(msg)
}

// callWithResult sends a command and waits for Chromium's reply so callers
// can read return values (Runtime.evaluate etc.). Input dispatch on
// loopback answers in well under a millisecond; the timeout only guards a
// wedged browser.
func (c *CDPClient) callWithResult(method string, params map[string]interface{}, timeout time.Duration) (json.RawMessage, error) {
	c.mu.Lock()
	if c.conn == nil || !c.connected {
		c.mu.Unlock()
		return nil, fmt.Errorf("cdp not connected")
	}
	id := c.nextID
	c.nextID++
	ch := make(chan cdpResponse, 1)
	c.pending[id] = ch
	msg := map[string]interface{}{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.conn.WriteJSON(msg); err != nil {
		delete(c.pending, id)
		c.connected = false
		c.mu.Unlock()
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	c.mu.Unlock()

	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("%s: connection closed", method)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: cdp error %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-time.After(timeout):
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("%s: timed out after %v", method, timeout)
	}
}

// evaluateString runs an expression that returns a string value.
func (c *CDPClient) evaluateString(expression string) (string, error) {
	return c.evaluateStringTimeout(expression, 3*time.Second)
}

// evaluateStringTimeout is evaluateString with a caller-chosen timeout —
// used by input-path probes (selectionContainsPoint) that must never stall
// the input loop for seconds on a busy page.
func (c *CDPClient) evaluateStringTimeout(expression string, timeout time.Duration) (string, error) {
	result, err := c.callWithResult("Runtime.evaluate", map[string]interface{}{
		"expression":    expression,
		"returnByValue": true,
	}, timeout)
	if err != nil {
		return "", err
	}
	return parseEvaluateString(result)
}

// evaluateStringAwaitTimeout is evaluateStringTimeout for expressions that
// return a promise (navigator.clipboard.readText, for one). Without
// awaitPromise Chromium hands back the pending promise object, which
// returnByValue renders as a value-less remote object — i.e. "".
func (c *CDPClient) evaluateStringAwaitTimeout(expression string, timeout time.Duration) (string, error) {
	result, err := c.callWithResult("Runtime.evaluate", map[string]interface{}{
		"expression":    expression,
		"returnByValue": true,
		"awaitPromise":  true,
	}, timeout)
	if err != nil {
		return "", err
	}
	return parseEvaluateString(result)
}

// errBadResultShape: Runtime.evaluate answered, but not with the string this
// helper promises. The renderer is alive — the expression (or the parser) is
// what is wrong, so callers that use an answered probe as liveness evidence
// must not read this as "no answer".
var errBadResultShape = errors.New("evaluate: bad result shape")

func parseEvaluateString(result json.RawMessage) (string, error) {
	var rr struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	if json.Unmarshal(result, &rr) != nil {
		return "", errBadResultShape
	}
	return rr.Result.Value, nil
}

func (c *CDPClient) send(method string, params map[string]interface{}) error {
	err := c.call(method, params)
	if err != nil {
		c.mu.Lock()
		c.connected = false
		c.mu.Unlock()
	}
	return err
}

func (c *CDPClient) setMousePosition(x, y int) {
	c.mu.Lock()
	c.lastMouseX = float64(x)
	c.lastMouseY = float64(y)
	c.mu.Unlock()
}

func (c *CDPClient) mousePosition() (float64, float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastMouseX, c.lastMouseY
}
