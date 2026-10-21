package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

func TestNetMaxEntriesFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"", netMaxEntriesDefault},
		{"250", 250},
		{"1000", 1000},
		{"0", netMaxEntriesDefault},
		{"-5", netMaxEntriesDefault},
		{"nope", netMaxEntriesDefault},
		{"999999", netMaxEntriesCap},
	}
	for _, tc := range cases {
		t.Setenv("NET_LOG_MAX_ENTRIES", tc.env)
		resetNetMaxEntriesForTest()
		if got := netMaxEntries(); got != tc.want {
			t.Errorf("NET_LOG_MAX_ENTRIES=%q: netMaxEntries() = %d, want %d", tc.env, got, tc.want)
		}
	}
	defer resetNetMaxEntriesForTest()
}

// TestRememberOrderEvictsAtConfiguredCap: the capture window is a rolling one —
// the oldest key leaves both the replay order and the request map.
func TestRememberOrderEvictsAtConfiguredCap(t *testing.T) {
	t.Setenv("NET_LOG_MAX_ENTRIES", "2")
	resetNetMaxEntriesForTest()
	defer resetNetMaxEntriesForTest()

	netMu.Lock()
	prevRequests, prevOrder, prevOrdered := netRequests, netOrder, netOrdered
	netRequests = map[string]*netRequest{}
	netOrder = nil
	netOrdered = nil
	for _, key := range []string{"a", "b", "c"} {
		rememberOrder(key)
	}
	gotOrder := append([]string(nil), netOrder...)
	netMu.Unlock()

	defer func() {
		netMu.Lock()
		netRequests, netOrder, netOrdered = prevRequests, prevOrder, prevOrdered
		netMu.Unlock()
	}()

	if len(gotOrder) != 2 || gotOrder[0] != "b" || gotOrder[1] != "c" {
		t.Fatalf("netOrder = %v, want [b c]", gotOrder)
	}
}

// A clipped URL must be marked so the viewer does not present its copy as the
// real address.
func TestNetEntryWireMarksClippedURL(t *testing.T) {
	long := "https://example.com/" + string(make([]byte, netWireURLMax))
	e := netEntryWire(&netRequest{id: "x", url: long})
	if e["ut"] != true {
		t.Fatalf("clipped URL not marked: %v", e)
	}
	if got := len(e["url"].(string)); got != netWireURLMax {
		t.Fatalf("wire URL length = %d, want %d", got, netWireURLMax)
	}

	short := netEntryWire(&netRequest{id: "y", url: "https://example.com/app.js"})
	if _, ok := short["ut"]; ok {
		t.Fatalf("short URL wrongly marked truncated: %v", short)
	}
}

func TestNetMonitorStoresProtocolAndConnectionID(t *testing.T) {
	netMu.Lock()
	netRequests = map[string]*netRequest{}
	netOrder = []string{}
	netOrdered = nil
	netMu.Unlock()

	params := map[string]any{
		"requestId":    "42",
		"type":         "XHR",
		"connectionId": 99,
		"request": map[string]string{
			"url":    "https://example.com/" + string(make([]byte, 400)),
			"method": "GET",
		},
		"response": map[string]any{
			"status":          200,
			"mimeType":        "application/json",
			"protocol":        "h2",
			"remoteIPAddress": "93.184.216.34",
		},
	}
	reqParams := map[string]any{
		"requestId": "42",
		"type":      "XHR",
		"request": map[string]string{
			"url":    "https://example.com/" + string(make([]byte, 400)),
			"method": "GET",
		},
	}
	reqRaw, _ := json.Marshal(reqParams)
	netMonitorTap("target-aaaa", "", "Network.requestWillBeSent", reqRaw, 1)
	raw, _ := json.Marshal(params)
	netMonitorTap("target-aaaa", "", "Network.responseReceived", raw, 1)

	entries := snapshotNetworkLog()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Protocol != "h2" {
		t.Errorf("protocol = %q, want h2", e.Protocol)
	}
	if e.ConnectionID != 99 {
		t.Errorf("connection_id = %d, want 99", e.ConnectionID)
	}
	if e.Mime != "application/json" {
		t.Errorf("mime = %q", e.Mime)
	}
	if len(e.URL) < 300 {
		t.Errorf("expected full URL in dump, got len %d", len(e.URL))
	}
}

// A request first seen at responseReceived (mid-navigation reconnect) must join
// the rolling window immediately. Otherwise a response that never produces
// loadingFinished/loadingFailed stays outside netOrder forever, is never
// evicted, and is never replayed.
func TestResponseReceivedEntersCaptureWindow(t *testing.T) {
	defer resetNetCapture()()
	t.Setenv("NET_LOG_MAX_ENTRIES", "3")
	resetNetMaxEntriesForTest()
	defer resetNetMaxEntriesForTest()

	for i := 0; i < 5; i++ {
		params := json.RawMessage(fmt.Sprintf(
			`{"requestId":"r%d","type":"Script","response":{"status":200}}`, i))
		netMonitorTap("target", "", "Network.responseReceived", params, 1)
	}

	netMu.Lock()
	requests, order := len(netRequests), len(netOrder)
	netMu.Unlock()
	if requests > 3 || order > 3 {
		t.Fatalf("requests=%d order=%d, want both <= 3 (capture window)", requests, order)
	}
	if order != 3 {
		t.Fatalf("order=%d, want 3 (response-only entries must be ordered)", order)
	}
}

// The capture loop, tab reassignment and replay all touch netRequest fields
// from different goroutines. Run them together under -race; the wire map must
// be serialised while netMu is held.
func TestNetCaptureConcurrentEventsAndReplay(t *testing.T) {
	defer resetNetCapture()()
	defer snapshotTabState()()

	tabMu.Lock()
	activeTabID = unknownTab
	tabMu.Unlock()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				params := json.RawMessage(fmt.Sprintf(
					`{"requestId":"%d-%d","type":"Script","request":{"url":"https://h%d.example/x%d.js","method":"GET"}}`,
					g, i, g, i))
				netMonitorTap("target", "", "Network.requestWillBeSent", params, 1+i%2)
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 30; i++ {
			reassignUnknownTab(1)
			replayNetTo(func(map[string]any) {})
		}
	}()
	wg.Wait()

	// 4 goroutines × 30 unique request ids, plus the response-only entry.
	params := json.RawMessage(`{"requestId":"orphan","type":"Image","response":{"status":200}}`)
	netMonitorTap("target", "", "Network.responseReceived", params, 1)
	if got := len(snapshotNetworkLog()); got != 121 {
		t.Fatalf("snapshot = %d entries, want 121", got)
	}
}
