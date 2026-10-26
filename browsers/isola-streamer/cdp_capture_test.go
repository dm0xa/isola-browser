package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// withTabs installs a tab strip state and returns a restore func, so tests
// don't leak state into each other.
func withTabs(tabs []tabInfo, active int) func() {
	tabMu.Lock()
	prevTabs, prevActive := runnerTabs, activeTabID
	runnerTabs = tabs
	activeTabID = active
	tabMu.Unlock()
	return func() {
		tabMu.Lock()
		runnerTabs, activeTabID = prevTabs, prevActive
		tabMu.Unlock()
	}
}

func TestTabForTarget(t *testing.T) {
	tabs := []tabInfo{
		{ID: 1, URL: "https://example.com/", Title: "Home"},
		{ID: 2, URL: "https://example.com/search", Title: "Search"},
		{ID: 3, URL: "https://news.example/", Title: "News"},
	}

	tests := []struct {
		name        string
		tabs        []tabInfo
		active      int
		targetURL   string
		targetTitle string
		want        int
	}{
		{
			name:        "exact url match",
			tabs:        tabs,
			active:      1,
			targetURL:   "https://news.example/",
			targetTitle: "News",
			want:        3,
		},
		{
			name:        "prefix match for a redirect",
			tabs:        tabs,
			active:      1,
			targetURL:   "https://example.com/search?q=weather",
			targetTitle: "Search",
			want:        2,
		},
		{
			name:        "same url on two tabs, title disambiguates",
			tabs:        []tabInfo{{ID: 1, URL: "https://other.example/a", Title: "One"}, {ID: 2, URL: "https://other.example/a", Title: "Two"}},
			active:      1,
			targetURL:   "https://other.example/a",
			targetTitle: "Two",
			want:        2,
		},
		{
			name:        "same url on two tabs, no title match falls back to active",
			tabs:        []tabInfo{{ID: 1, URL: "https://other.example/a", Title: "One"}, {ID: 2, URL: "https://other.example/a", Title: "Two"}},
			active:      2,
			targetURL:   "https://other.example/a",
			targetTitle: "Something else",
			want:        2,
		},
		{
			name:        "unknown target falls back to active, not unattributed",
			tabs:        tabs,
			active:      2,
			targetURL:   "https://unrelated.example/deep/path",
			targetTitle: "?",
			want:        2,
		},
		{
			name:        "blank target never matches a tab url",
			tabs:        tabs,
			active:      1,
			targetURL:   "about:blank",
			targetTitle: "",
			want:        1,
		},
		{
			name:        "no runner tabs falls back to active",
			tabs:        nil,
			active:      -1,
			targetURL:   "https://example.com/",
			targetTitle: "Home",
			want:        -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := withTabs(tt.tabs, tt.active)
			defer restore()

			if got := tabForTarget(tt.targetURL, tt.targetTitle); got != tt.want {
				t.Fatalf("tabForTarget(%q, %q) = %d, want %d", tt.targetURL, tt.targetTitle, got, tt.want)
			}
		})
	}
}

func resetNetCapture() func() {
	netMu.Lock()
	prevRequests, prevOrder, prevOrdered, prevSeq := netRequests, netOrder, netOrdered, netSeenSeq
	prevOrigins, prevHeadered := netOriginByTab, netHeadered
	prevFrames := netMainFrameByTarget
	netRequests = map[string]*netRequest{}
	netOrder = nil
	netOrdered = nil
	netSeenSeq = 0
	netOriginByTab = map[int]string{}
	netHeadered = 0
	netMainFrameByTarget = map[string]string{}
	netMu.Unlock()
	return func() {
		netMu.Lock()
		netRequests, netOrder, netOrdered, netSeenSeq = prevRequests, prevOrder, prevOrdered, prevSeq
		netOriginByTab, netHeadered = prevOrigins, prevHeadered
		netMainFrameByTarget = prevFrames
		netMu.Unlock()
	}
}

// CDP request ids are per target: two tabs can both have a request "1".
// Without scoping by target they would overwrite each other in the map.
func TestNetMonitorTapScopesRequestsByTarget(t *testing.T) {
	defer resetNetCapture()()

	req := func(url string) json.RawMessage {
		return json.RawMessage(`{"requestId":"1","type":"Document","request":{"url":"` + url + `","method":"GET"}}`)
	}
	netMonitorTap("target-aaaa", "", "Network.requestWillBeSent", req("https://a.example/x"), 1)
	netMonitorTap("target-bbbb", "", "Network.requestWillBeSent", req("https://b.example/y"), 2)

	netMu.Lock()
	defer netMu.Unlock()
	if len(netRequests) != 2 {
		t.Fatalf("got %d entries, want 2 (request ids must be scoped per target)", len(netRequests))
	}
	for key, r := range netRequests {
		if r.tab == 0 {
			t.Fatalf("entry %q has no tab attribution", key)
		}
	}
	if len(netOrder) != 2 {
		t.Fatalf("replay order has %d keys, want 2", len(netOrder))
	}
}

// A redirect reuses the target's requestId and must update the same entry.
func TestNetMonitorTapRedirectUpdatesSameEntry(t *testing.T) {
	defer resetNetCapture()()

	const scope = "target-aaaa"
	netMonitorTap(scope, "", "Network.requestWillBeSent",
		json.RawMessage(`{"requestId":"7","type":"Document","request":{"url":"https://a.example/start"}}`), 1)
	netMonitorTap(scope, "", "Network.requestWillBeSent",
		json.RawMessage(`{"requestId":"7","type":"Document","request":{"url":"https://a.example/redirected"}}`), 1)

	netMu.Lock()
	defer netMu.Unlock()
	if len(netRequests) != 1 {
		t.Fatalf("got %d entries, want 1 (redirects reuse the requestId)", len(netRequests))
	}
	for _, r := range netRequests {
		if r.url != "https://a.example/redirected" {
			t.Fatalf("redirect did not update the entry: %q", r.url)
		}
	}
}

func TestShortTargetID(t *testing.T) {
	long := "0A1B2C3D4E5F60718293A4B5C6D7E8F9"
	if got := shortTargetID(long); got != "0A1B2C3D" {
		t.Fatalf("shortTargetID(long) = %q", got)
	}
	if got := shortTargetID("abc"); got != "abc" {
		t.Fatalf("shortTargetID(short) = %q", got)
	}
}

// TestCaptureClientAttachesSessionFromParams is the regression test for the
// flatten auto-attach session routing: Target.attachedToTarget carries the
// new session id inside params (the event originates at the browser
// endpoint, which has no top-level sessionId of its own). Reading it from
// the envelope stored every target under "" and made the browser reject
// Network.enable/Page.reload with "wasn't found" — capture silently dead.
func TestCaptureClientAttachesSessionFromParams(t *testing.T) {
	const sessID = "SESS1"
	const targetID = "TGT1"

	up := websocket.Upgrader{}
	sawEnable := make(chan string, 4) // sessionId field of Network.enable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"webSocketDebuggerURL": "ws://" + r.Host + "/browserws",
			})
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		sentAttach := false
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				ID        int    `json:"id"`
				Method    string `json:"method"`
				SessionID string `json:"sessionId"`
			}
			if json.Unmarshal(raw, &req) != nil {
				continue
			}
			if req.Method == "Target.setAutoAttach" {
				_ = conn.WriteJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
				// Real-world shape: sessionId lives in params.
				_ = conn.WriteJSON(map[string]any{
					"method": "Target.attachedToTarget",
					"params": map[string]any{
						"sessionId": sessID,
						"targetInfo": map[string]any{
							"targetId": targetID, "type": "page",
							"url": "https://example.com/", "title": "Example",
						},
					},
				})
				sentAttach = true
				continue
			}
			if req.Method == "Network.enable" {
				select {
				case sawEnable <- req.SessionID:
				default:
				}
			}
			if req.ID != 0 {
				_ = conn.WriteJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
			}
			if sentAttach {
				// Traffic for the attached target arrives with the
				// session id at the TOP level (flatten protocol).
				_ = conn.WriteJSON(map[string]any{
					"method":    "Network.requestWillBeSent",
					"sessionId": sessID,
					"params": map[string]any{
						"requestId": "1",
						"request":   map[string]any{"url": "https://example.com/", "method": "GET"},
					},
				})
			}
		}
	}))
	defer srv.Close()

	// Reset the global capture state this test feeds into.
	netMu.Lock()
	netRequests = map[string]*netRequest{}
	netOrder = nil
	netMu.Unlock()

	c := newCaptureClient()
	c.baseURL = srv.URL
	go c.Start()

	// Network.enable must be addressed to the target's session, not the
	// browser endpoint.
	deadline := time.After(3 * time.Second)
	var got string
	select {
	case got = <-sawEnable:
	case <-deadline:
		t.Fatal("timed out waiting for Network.enable")
	}
	if got != sessID {
		t.Fatalf("Network.enable sessionId = %q, want %q (command must be session-scoped)", got, sessID)
	}

	// The request event must land in the replay buffer.
	deadline2 := time.After(3 * time.Second)
	for {
		netMu.Lock()
		// Buffer keys are scoped by CDP target id, not session id.
		_, ok := netRequests[targetID+"|1"]
		netMu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline2:
			t.Fatal("timed out waiting for captured request to reach the buffer")
		case <-time.After(20 * time.Millisecond):
		}
	}

	c.dropConn()
}

// TestCaptureClientResponseBookkeeping: command responses clear the pending
// entry, and browser-side rejections are surfaced rather than silently
// dropped (a rejected Network.enable used to look identical to silence).
func TestCaptureClientResponseBookkeeping(t *testing.T) {
	c := newCaptureClient()

	c.mu.Lock()
	c.conn = &websocket.Conn{} // non-nil so sendSession registers the command
	c.pending[7] = "Network.enable"
	c.mu.Unlock()

	c.onResponse([]byte(`{"id":7,"error":{"code":-32601,"message":"'Network.enable' wasn't found"}}`))
	c.mu.Lock()
	if _, still := c.pending[7]; still {
		c.mu.Unlock()
		t.Fatal("error response did not clear pending entry")
	}
	c.mu.Unlock()

	c.mu.Lock()
	c.pending[9] = "Page.reload"
	c.mu.Unlock()
	c.onResponse([]byte(`{"id":9,"result":{}}`))
	c.mu.Lock()
	if _, still := c.pending[9]; still {
		c.mu.Unlock()
		t.Fatal("result response did not clear pending entry")
	}
	c.mu.Unlock()

	// Garbage / zero-id payloads must not panic or clear anything.
	c.mu.Lock()
	c.pending[11] = "Runtime.enable"
	c.mu.Unlock()
	c.onResponse([]byte(`not json`))
	c.onResponse([]byte(`{"result":{}}`))
	c.mu.Lock()
	if _, still := c.pending[11]; !still {
		c.mu.Unlock()
		t.Fatal("unrelated response cleared pending entry")
	}
	c.mu.Unlock()
}
