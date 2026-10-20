package main

import (
	"encoding/json"
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
		if got := netMaxEntries(); got != tc.want {
			t.Errorf("NET_LOG_MAX_ENTRIES=%q: netMaxEntries() = %d, want %d", tc.env, got, tc.want)
		}
	}
}

// TestRememberOrderEvictsAtConfiguredCap: the capture window is a rolling one —
// the oldest key leaves both the replay order and the request map.
func TestRememberOrderEvictsAtConfiguredCap(t *testing.T) {
	t.Setenv("NET_LOG_MAX_ENTRIES", "2")

	netMu.Lock()
	prevRequests, prevOrder := netRequests, netOrder
	netRequests = map[string]*netRequest{}
	netOrder = nil
	for _, key := range []string{"a", "b", "c"} {
		rememberOrder(key)
	}
	gotOrder := append([]string(nil), netOrder...)
	netMu.Unlock()

	defer func() {
		netMu.Lock()
		netRequests, netOrder = prevRequests, prevOrder
		netMu.Unlock()
	}()

	if len(gotOrder) != 2 || gotOrder[0] != "b" || gotOrder[1] != "c" {
		t.Fatalf("netOrder = %v, want [b c]", gotOrder)
	}
}

func TestNetMonitorStoresProtocolAndConnectionID(t *testing.T) {
	netMu.Lock()
	netRequests = map[string]*netRequest{}
	netOrder = []string{}
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
