package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestHashableType(t *testing.T) {
	hashed := []string{"Document", "Script", "Stylesheet", "Font", "Image", "XHR", "Fetch", "Manifest"}
	for _, typ := range hashed {
		if !hashableType(typ) {
			t.Errorf("%s should be hashable", typ)
		}
	}
	// Streams and large media are deliberately excluded.
	for _, typ := range []string{"Media", "WebSocket", "EventSource", ""} {
		if hashableType(typ) {
			t.Errorf("%q must not be hashable", typ)
		}
	}
}

func TestShouldHashBody(t *testing.T) {
	prev := activeCapture
	activeCapture = &captureClient{}
	defer func() { activeCapture = prev }()

	small := func() *netRequest {
		return &netRequest{typ: "Script", url: "https://a.example/app.js", bytes: 1024}
	}

	if !shouldHashBody(small(), "sess") {
		t.Fatal("a small http script should be hashed")
	}
	if shouldHashBody(&netRequest{typ: "Media", url: "https://a/v.mp4", bytes: 10}, "sess") {
		t.Fatal("media must not be hashed")
	}
	if shouldHashBody(&netRequest{typ: "Script", url: "https://a/b.js", bytes: netBodyHashMaxBytes + 1}, "sess") {
		t.Fatal("an oversized body must not be hashed")
	}
	if shouldHashBody(&netRequest{typ: "Script", url: "https://a/b.js", bytes: 10, sha1: "deadbeef"}, "sess") {
		t.Fatal("an already-hashed entry must not be re-hashed")
	}
	if shouldHashBody(&netRequest{typ: "Script", url: "https://a/b.js", bytes: 10, failed: true}, "sess") {
		t.Fatal("a failed request must not be hashed")
	}
	if shouldHashBody(&netRequest{typ: "Script", url: "data:text/javascript,x", bytes: 10}, "sess") {
		t.Fatal("a non-http url must not be hashed")
	}
	if shouldHashBody(small(), "") {
		t.Fatal("without a session id the target cannot be commanded")
	}
	activeCapture = nil
	if shouldHashBody(small(), "sess") {
		t.Fatal("without a capture client no body can be fetched")
	}
}

// A finished hashable request must be published only once its body hash
// settles: the tap returns a job and withholds the broadcast.
func TestApplyNetEventDefersHashableFinish(t *testing.T) {
	prev := activeCapture
	activeCapture = &captureClient{}
	defer func() { activeCapture = prev }()

	defer resetNetCapture()()

	var inFlight, finished netEvent
	if err := json.Unmarshal([]byte(`{"requestId":"1","type":"Script","request":{"url":"https://a.example/app.js","method":"GET"}}`), &inFlight); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"requestId":"1","encodedDataLength":2048}`), &finished); err != nil {
		t.Fatal(err)
	}

	const key, uiID = "target-aaaa|1", "target:1"
	netMu.Lock()
	_, job, _ := applyNetEvent(key, uiID, "Network.requestWillBeSent", &inFlight, 1, "sess-1")
	if job != nil {
		netMu.Unlock()
		t.Fatal("a request-in-flight event must not schedule a hash job")
	}
	broadcast, job, _ := applyNetEvent(key, uiID, "Network.loadingFinished", &finished, 1, "sess-1")
	netMu.Unlock()

	if job == nil {
		t.Fatal("a finished script with a session must schedule a hash job")
	}
	if broadcast != nil {
		t.Fatal("the finished entry must be withheld until the hash settles")
	}
	if job.sessionID != "sess-1" || job.r == nil {
		t.Fatalf("job not wired to the entry/session: %+v", job)
	}
}

func TestNetEntryWireIncludesHash(t *testing.T) {
	plain := netEntryWire(&netRequest{id: "t:1", url: "https://a/b.js", typ: "Script"})
	if _, ok := plain["h"]; ok {
		t.Fatal("an unknown hash must be omitted from the wire entry")
	}
	hashed := netEntryWire(&netRequest{id: "t:1", url: "https://a/b.js", typ: "Script", sha1: "abc123"})
	if hashed["h"] != "abc123" {
		t.Fatalf("wire hash = %v, want abc123", hashed["h"])
	}
}

// fakeCDPServer stands in for Chromium's browser endpoint: it answers every
// command and returns `body` for Network.getResponseBody. Commands it receives
// are reported on `seen` when non-nil.
func fakeCDPServer(t *testing.T, body string, base64Encoded bool, seen chan<- map[string]any) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/devtools/browser/x"
		_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": ws})
	})
	mux.HandleFunc("/devtools/browser/x", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var req struct {
				ID        int             `json:"id"`
				Method    string          `json:"method"`
				SessionID string          `json:"sessionId"`
				Params    json.RawMessage `json:"params"`
			}
			if conn.ReadJSON(&req) != nil {
				return
			}
			if req.ID == 0 {
				continue
			}
			if seen != nil {
				select {
				case seen <- map[string]any{"method": req.Method, "sessionId": req.SessionID, "params": string(req.Params)}:
				default:
				}
			}
			if req.Method == "Network.getResponseBody" {
				_ = conn.WriteJSON(map[string]any{
					"id":     req.ID,
					"result": map[string]any{"body": body, "base64Encoded": base64Encoded},
				})
				continue
			}
			_ = conn.WriteJSON(map[string]any{"id": req.ID, "result": map[string]any{}})
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func waitCaptureConnected(t *testing.T, c *captureClient) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		c.mu.Lock()
		ready := c.conn != nil
		c.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the capture client to connect")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// End-to-end: a finished script's body is fetched over CDP, hashed, and the
// published entry carries the hex SHA-1 — and the command is session-scoped
// with the CDP request id.
func TestHashNetBodyPublishesSha1(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		base64Encoded bool
		raw           []byte
	}{
		{name: "text body", body: "hello world", raw: []byte("hello world")},
		{name: "base64 body", body: base64.StdEncoding.EncodeToString([]byte("hello world")), base64Encoded: true, raw: []byte("hello world")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer resetNetCapture()()
			prev := activeCapture
			defer func() { activeCapture = prev }()

			seen := make(chan map[string]any, 8)
			srv := fakeCDPServer(t, tc.body, tc.base64Encoded, seen)
			c := newCaptureClient()
			c.baseURL = srv.URL
			go c.Start()
			activeCapture = c
			waitCaptureConnected(t, c)
			defer c.dropConn()

			var mu sync.Mutex
			var last map[string]any
			unreg := registerTabListener(func(msg map[string]any) {
				if msg["type"] != "net" {
					return
				}
				if e, ok := msg["e"].(map[string]any); ok {
					mu.Lock()
					last = e
					mu.Unlock()
				}
			})
			defer unreg()

			netMonitorTap("target-aaaa", "sess-1", "Network.requestWillBeSent",
				json.RawMessage(`{"requestId":"1","type":"Script","request":{"url":"https://a.example/app.js","method":"GET"}}`), 1)
			netMonitorTap("target-aaaa", "sess-1", "Network.loadingFinished",
				json.RawMessage(`{"requestId":"1","encodedDataLength":11}`), 1)

			want := fmt.Sprintf("%x", sha1.Sum(tc.raw))
			deadline := time.After(3 * time.Second)
			for {
				mu.Lock()
				h, _ := last["h"].(string)
				mu.Unlock()
				if h != "" {
					if h != want {
						t.Fatalf("published sha1 = %q, want %q", h, want)
					}
					break
				}
				select {
				case <-deadline:
					t.Fatal("timed out waiting for a hashed entry to be published")
				case <-time.After(20 * time.Millisecond):
				}
			}

			// The body request must be addressed to the target's session and
			// carry the CDP request id.
			deadline2 := time.After(2 * time.Second)
			for {
				select {
				case cmd := <-seen:
					if cmd["method"] != "Network.getResponseBody" {
						continue
					}
					if cmd["sessionId"] != "sess-1" {
						t.Fatalf("getResponseBody sessionId = %v, want sess-1", cmd["sessionId"])
					}
					if !strings.Contains(fmt.Sprint(cmd["params"]), `"requestId":"1"`) {
						t.Fatalf("getResponseBody params = %v, want requestId 1", cmd["params"])
					}
				case <-deadline2:
					t.Fatal("never observed Network.getResponseBody")
				}
				break
			}
		})
	}
}
