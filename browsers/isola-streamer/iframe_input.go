package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Cross-origin iframe input.
//
// Input.dispatchMouseEvent on the page target stays in that frame. Electron's
// offscreen widget does not route those events into out-of-process iframes, so
// a click on an embedded player (a video embed inside a search-results pane,
// for one) hits the parent <iframe> element and the player's own controls never
// see it. The picture still updates, because paint composites the child frame;
// only the hit testing is wrong.
//
// When the pointer is over a cross-origin iframe we translate into that
// frame's viewport and dispatch on the iframe's own CDP target. Same-origin
// frames are walked in one page-side evaluate — the parent renderer can see
// into them — and the walk stops at the first cross-origin frame, which is
// the one /json/list exposes as its own target.

const (
	// iframeCacheTTL is how long a probed iframe rectangle may be reused.
	// The embed's box is stable while the pointer stays on it; a scroll or
	// layout change is picked up on the next probe.
	iframeCacheTTL = 400 * time.Millisecond
	// iframeProbeGap caps how often a miss probes the page. A hit is cached,
	// so the steady state over a player does not evaluate on every move.
	iframeProbeGap     = 90 * time.Millisecond
	iframeProbeTimeout = 150 * time.Millisecond
)

// iframePlan is the routing decision for one pointer event before any CDP
// call: reuse the cached frame, ask the page what is under the pointer, or
// leave the event on the parent frame.
type iframePlan int

const (
	planParent iframePlan = iota
	planCached
	planProbe
)

type iframeHit struct {
	Hit bool    `json:"hit"`
	Src string  `json:"src"`
	L   float64 `json:"l"`
	T   float64 `json:"t"`
	W   float64 `json:"w"`
	H   float64 `json:"h"`
}

func (h iframeHit) contains(x, y int) bool {
	if !h.Hit || h.W < 2 || h.H < 2 {
		return false
	}
	fx, fy := float64(x), float64(y)
	return fx >= h.L && fx < h.L+h.W && fy >= h.T && fy < h.T+h.H
}

func (h iframeHit) local(x, y int) (int, int) {
	return int(math.Round(float64(x) - h.L)), int(math.Round(float64(y) - h.T))
}

// frameURLKey identifies a frame target independent of query strings.
// A player's embed URL grows parameters that the CDP target may not share
// verbatim; host and path (which include the media id) are the stable part.
func frameURLKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return host + path
}

func parseIframeHit(raw string) (iframeHit, bool) {
	var h iframeHit
	if json.Unmarshal([]byte(raw), &h) != nil || !h.Hit || h.Src == "" || h.W < 2 || h.H < 2 {
		return iframeHit{}, false
	}
	return h, true
}

// iframeRoutePlan decides where a pointer event goes.
//
// captured is set for the whole press-move-release gesture that began on an
// iframe, so a seek drag keeps updating that frame after the pointer leaves
// its box. mouseup outside a gesture stays on the parent: a release with no
// matching press must not be delivered to a different frame.
func iframeRoutePlan(now time.Time, cache iframeHit, cacheAt, lastProbe time.Time, captured bool, evType string, x, y int) iframePlan {
	switch evType {
	case "mouseup":
		if captured && cache.Hit {
			return planCached
		}
		return planParent
	case "mousemove":
		if captured && cache.Hit {
			return planCached
		}
		if cache.Hit && now.Sub(cacheAt) < iframeCacheTTL && cache.contains(x, y) {
			return planCached
		}
		if now.Sub(lastProbe) < iframeProbeGap {
			return planParent
		}
		return planProbe
	case "mousedown", "mousewheel":
		if cache.Hit && now.Sub(cacheAt) < iframeCacheTTL && cache.contains(x, y) {
			return planCached
		}
		return planProbe
	default:
		return planParent
	}
}

func iframeProbeExpr(x, y int) string {
	// Walk same-origin frames. contentDocument throws for a cross-origin
	// child; that child is the frame whose own CDP target must receive the
	// event, and its rectangle is accumulated in the parent's coordinate
	// space (the same space viewer clicks already use).
	return fmt.Sprintf(`(function(x,y){
		var doc=document, ox=0, oy=0;
		for (var i=0;i<6;i++){
			var el=doc.elementFromPoint(x-ox,y-oy);
			if(!el||el.tagName!=="IFRAME") return JSON.stringify({hit:false});
			var r=el.getBoundingClientRect();
			var absL=ox+r.left, absT=oy+r.top;
			var child=null;
			try { child=el.contentDocument; } catch(e) {}
			if(!child){
				return JSON.stringify({hit:true,src:el.src||"",l:absL,t:absT,w:r.width,h:r.height});
			}
			ox=absL; oy=absT; doc=child;
		}
		return JSON.stringify({hit:false});
	})(%d,%d)`, x, y)
}

func matchFrameTarget(targets []cdpTarget, src string) (string, bool) {
	key := frameURLKey(src)
	if key == "" {
		return "", false
	}
	fallback := ""
	for _, t := range targets {
		if t.WebSocketDebuggerURL == "" || frameURLKey(t.URL) != key {
			continue
		}
		if t.Type == "iframe" {
			return t.WebSocketDebuggerURL, true
		}
		if fallback == "" && t.Type != "browser" {
			fallback = t.WebSocketDebuggerURL
		}
	}
	if fallback != "" {
		return fallback, true
	}
	return "", false
}

// iframeConn is a CDP session on one cross-origin frame target. Commands are
// fire-and-forget (input does not read the result) but the socket is still
// read, or Chromium's replies would stall the TCP window.
type iframeConn struct {
	mu     sync.Mutex
	conn   *websocket.Conn
	nextID int
	dead   bool
}

func dialIframeConn(wsURL string) (*iframeConn, error) {
	d := websocket.Dialer{HandshakeTimeout: 500 * time.Millisecond}
	conn, _, err := d.Dial(wsURL, nil)
	if err != nil {
		return nil, err
	}
	s := &iframeConn{conn: conn}
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				s.mu.Lock()
				s.dead = true
				s.mu.Unlock()
				return
			}
		}
	}()
	return s, nil
}

func (s *iframeConn) alive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil && !s.dead
}

func (s *iframeConn) send(method string, params map[string]interface{}) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil || s.dead {
		return fmt.Errorf("iframe cdp closed")
	}
	s.nextID++
	msg := map[string]interface{}{"id": s.nextID, "method": method}
	if params != nil {
		msg["params"] = params
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	if err := s.conn.WriteJSON(msg); err != nil {
		s.dead = true
		s.conn.Close()
		return err
	}
	return nil
}

func (s *iframeConn) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = true
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
}

type iframeState struct {
	mu           sync.Mutex
	cache        iframeHit
	cacheAt      time.Time
	lastProbe    time.Time
	probeBackoff time.Time
	captured     bool
	key          *iframeConn
	conns        map[string]*iframeConn
}

func newIframeState() iframeState {
	return iframeState{conns: map[string]*iframeConn{}}
}

func (s *iframeState) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = iframeHit{}
	s.captured = false
	s.key = nil
	for k, c := range s.conns {
		c.close()
		delete(s.conns, k)
	}
}

func (c *CDPClient) iframeHasKeyFocus() bool {
	c.frames.mu.Lock()
	conn := c.frames.key
	c.frames.mu.Unlock()
	return conn != nil && conn.alive()
}

// sendKeys delivers a key or text command to the cross-origin frame the
// pointer last pressed, falling back to the page target when that frame is
// gone. Mouse dispatch does not use this.
func (c *CDPClient) sendKeys(method string, params map[string]interface{}) error {
	c.frames.mu.Lock()
	conn := c.frames.key
	c.frames.mu.Unlock()
	if conn != nil {
		if err := conn.send(method, params); err == nil {
			return nil
		}
		c.frames.mu.Lock()
		if c.frames.key == conn {
			c.frames.key = nil
		}
		c.frames.mu.Unlock()
	}
	return c.send(method, params)
}

func (c *CDPClient) iframeSession(src string) (*iframeConn, error) {
	key := frameURLKey(src)
	c.frames.mu.Lock()
	existing := c.frames.conns[key]
	c.frames.mu.Unlock()
	if existing != nil && existing.alive() {
		return existing, nil
	}
	targets, err := c.listTargets()
	if err != nil {
		return nil, err
	}
	wsURL, ok := matchFrameTarget(targets, src)
	if !ok {
		return nil, fmt.Errorf("no cdp target for %s", key)
	}
	conn, err := dialIframeConn(wsURL)
	if err != nil {
		return nil, err
	}
	log.Printf("input: iframe target %s", key)
	c.frames.mu.Lock()
	if prev := c.frames.conns[key]; prev != nil && prev != conn {
		prev.close()
	}
	c.frames.conns[key] = conn
	c.frames.mu.Unlock()
	return conn, nil
}

func (c *CDPClient) probeIframe(x, y int) (iframeHit, bool) {
	raw, err := c.evaluateStringTimeout(iframeProbeExpr(x, y), iframeProbeTimeout)
	c.frames.mu.Lock()
	c.frames.lastProbe = time.Now()
	c.frames.mu.Unlock()
	if err != nil {
		c.frames.mu.Lock()
		c.frames.probeBackoff = time.Now().Add(500 * time.Millisecond)
		c.frames.mu.Unlock()
		return iframeHit{}, false
	}
	h, ok := parseIframeHit(raw)
	if !ok {
		c.frames.mu.Lock()
		c.frames.cache = iframeHit{}
		c.frames.cacheAt = time.Now()
		c.frames.mu.Unlock()
		return iframeHit{}, false
	}
	c.frames.mu.Lock()
	c.frames.cache = h
	c.frames.cacheAt = time.Now()
	c.frames.mu.Unlock()
	return h, true
}

func (c *CDPClient) dispatchIframeMouse(conn *iframeConn, ev inputEvent, lx, ly int) bool {
	var err error
	switch ev.Type {
	case "mousemove":
		err = conn.send("Input.dispatchMouseEvent", mouseMovedParams(lx, ly, ev.Buttons))
	case "mousedown":
		err = conn.send("Input.dispatchMouseEvent", mouseButtonParams("mousePressed", ev.Button, lx, ly, ev.ClickCount))
	case "mouseup":
		err = conn.send("Input.dispatchMouseEvent", mouseButtonParams("mouseReleased", ev.Button, lx, ly, ev.ClickCount))
	case "mousewheel":
		err = conn.send("Input.dispatchMouseEvent", mouseWheelParams(ev.DeltaX, ev.DeltaY, lx, ly))
	default:
		return false
	}
	return err == nil
}

// dispatchViaIframe reports whether the event was delivered to a cross-origin
// iframe. False means the caller should dispatch it to the page target as usual.
func (c *CDPClient) dispatchViaIframe(ev inputEvent) bool {
	now := time.Now()
	c.frames.mu.Lock()
	plan := iframeRoutePlan(now, c.frames.cache, c.frames.cacheAt, c.frames.lastProbe, c.frames.captured, ev.Type, ev.X, ev.Y)
	cache := c.frames.cache
	c.frames.mu.Unlock()

	if plan == planProbe {
		c.frames.mu.Lock()
		backingOff := now.Before(c.frames.probeBackoff)
		c.frames.mu.Unlock()
		if backingOff {
			// A wedged renderer already failed a probe. Waiting out another
			// timeout on every click would just stall the parent frame too.
			plan = planParent
		}
	}

	hit := cache
	ok := plan == planCached && cache.Hit
	if plan == planProbe {
		hit, ok = c.probeIframe(ev.X, ev.Y)
		if !ok && cache.Hit && cache.contains(ev.X, ev.Y) {
			// The page didn't answer in time. A stale box we already trusted
			// is a better aim than dropping the event on the parent frame.
			hit, ok = cache, true
		}
	}
	if !ok {
		if ev.Type == "mousedown" {
			c.frames.mu.Lock()
			c.frames.captured = false
			c.frames.key = nil
			c.frames.mu.Unlock()
		}
		return false
	}

	conn, err := c.iframeSession(hit.Src)
	if err != nil {
		if ev.Type == "mousedown" {
			c.frames.mu.Lock()
			c.frames.captured = false
			c.frames.key = nil
			c.frames.mu.Unlock()
		}
		return false
	}
	lx, ly := hit.local(ev.X, ev.Y)
	if !c.dispatchIframeMouse(conn, ev, lx, ly) {
		return false
	}
	c.frames.mu.Lock()
	switch ev.Type {
	case "mousedown":
		c.frames.captured = true
		c.frames.key = conn
	case "mouseup":
		c.frames.captured = false
	}
	c.frames.mu.Unlock()
	return true
}

func mouseMovedParams(x, y, buttons int) map[string]interface{} {
	held := "none"
	if buttons&1 != 0 {
		held = "left"
	} else if buttons&2 != 0 {
		held = "right"
	} else if buttons&4 != 0 {
		held = "middle"
	}
	params := map[string]interface{}{
		"type":       "mouseMoved",
		"x":          x,
		"y":          y,
		"modifiers":  0,
		"button":     "none",
		"buttons":    buttons,
		"clickCount": 0,
	}
	if held != "none" {
		params["button"] = held
	}
	return params
}

func mouseButtonParams(eventType, button string, x, y, clickCount int) map[string]interface{} {
	if clickCount < 1 {
		clickCount = 1
	}
	return map[string]interface{}{
		"type":       eventType,
		"x":          x,
		"y":          y,
		"modifiers":  0,
		"button":     cdpButtonName(button),
		"buttons":    cdpButtonsMask(button, eventType),
		"clickCount": clickCount,
	}
}

func mouseWheelParams(deltaX, deltaY, x, y int) map[string]interface{} {
	return map[string]interface{}{
		"type":      "mouseWheel",
		"x":         x,
		"y":         y,
		"modifiers": 0,
		"deltaX":    deltaX,
		"deltaY":    deltaY,
	}
}
