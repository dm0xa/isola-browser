package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// tap is a small helper: it feeds one raw CDP event into the network tap.
func tap(t *testing.T, method, params string) {
	t.Helper()
	netMonitorTap("target-aaaa", "sess-1", method, json.RawMessage(params), 1)
}

// A redirect reuses the CDP requestId. The chain must survive the URL
// overwrite, otherwise every hop but the last is lost.
func TestRedirectChainIsKept(t *testing.T) {
	defer resetNetCapture()()

	tap(t, "Network.requestWillBeSent",
		`{"requestId":"1","type":"Document","request":{"url":"http://example.com/","method":"GET"}}`)
	tap(t, "Network.requestWillBeSent",
		`{"requestId":"1","type":"Document","request":{"url":"http://example.com/login","method":"GET"},"redirectResponse":{"status":302,"url":"http://example.com/"}}`)
	tap(t, "Network.requestWillBeSent",
		`{"requestId":"1","type":"Document","request":{"url":"https://example.com/login","method":"GET"},"redirectResponse":{"status":301,"url":"http://example.com/login"}}`)

	entries := snapshotNetworkLog()
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1 (one requestId)", len(entries))
	}
	e := entries[0]
	if e.URL != "https://example.com/login" {
		t.Fatalf("final URL = %q", e.URL)
	}
	if len(e.Redirects) != 2 {
		t.Fatalf("redirects = %d, want 2", len(e.Redirects))
	}
	if e.Redirects[0].Status != 302 || e.Redirects[1].Status != 301 {
		t.Fatalf("statuses = %d,%d", e.Redirects[0].Status, e.Redirects[1].Status)
	}
	if e.Redirects[0].From != "http://example.com/" || e.Redirects[0].To != "http://example.com/login" {
		t.Fatalf("first hop = %+v", e.Redirects[0])
	}
}

func TestDocumentHeadersAndPostureAreCaptured(t *testing.T) {
	defer resetNetCapture()()

	tap(t, "Network.requestWillBeSent",
		`{"requestId":"r1","type":"Document","request":{"url":"https://example.com/","method":"GET"}}`)
	tap(t, "Network.responseReceived",
		`{"requestId":"r1","type":"Document","response":{"status":200,"mimeType":"text/html","protocol":"h2","remoteIPAddress":"93.184.216.34","timing":{"receiveHeadersEnd":123.4},"headers":{"Server":"nginx","Strict-Transport-Security":"max-age=31536000","Content-Security-Policy":"default-src 'self'","X-Content-Type-Options":"nosniff"}}}`)

	entries := snapshotNetworkLog()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	e := entries[0]
	if e.Headers["server"] != "nginx" {
		t.Fatalf("headers = %v", e.Headers)
	}
	if e.Posture == nil || e.Posture.Score != 3 {
		t.Fatalf("posture = %+v, want score 3", e.Posture)
	}
	if e.TTFBMs != 123 {
		t.Fatalf("ttfb = %d, want 123", e.TTFBMs)
	}
}

// ExtraInfo carries the raw wire header block, including headers Chromium
// filters out of response.headers.
func TestExtraInfoMergesRawHeaderText(t *testing.T) {
	defer resetNetCapture()()

	tap(t, "Network.requestWillBeSent",
		`{"requestId":"r2","type":"Document","request":{"url":"https://example.com/","method":"GET"}}`)
	tap(t, "Network.responseReceivedExtraInfo",
		`{"requestId":"r2","statusCode":200,"headersText":"HTTP/2 200\r\nserver: envoy\r\nx-debug: raw\r\n"}`)

	entries := snapshotNetworkLog()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	e := entries[0]
	if e.Headers["server"] != "envoy" || e.Headers["x-debug"] != "raw" {
		t.Fatalf("headers = %v", e.Headers)
	}
	if e.Status != 200 {
		t.Fatalf("status = %d, want 200 from ExtraInfo", e.Status)
	}
}

func TestNonDocumentHeadersAreDropped(t *testing.T) {
	defer resetNetCapture()()

	tap(t, "Network.requestWillBeSent",
		`{"requestId":"img","type":"Image","request":{"url":"https://example.com/a.png","method":"GET"}}`)
	tap(t, "Network.responseReceived",
		`{"requestId":"img","type":"Image","response":{"status":200,"mimeType":"image/png","headers":{"server":"nginx"}}}`)

	e := snapshotNetworkLog()[0]
	if e.Headers != nil {
		t.Fatalf("image headers must not be stored, got %v", e.Headers)
	}
}

func TestIframeDocumentDoesNotChangeTabOrigin(t *testing.T) {
	defer resetNetCapture()()

	// Main frame first; an iframe document (a consent wall) must not become the
	// page's origin, or every first-party request is mislabelled third-party.
	tap(t, "Network.requestWillBeSent",
		`{"requestId":"m","type":"Document","frameId":"MAIN","request":{"url":"https://www.bbc.co.uk/news","method":"GET"}}`)
	tap(t, "Network.requestWillBeSent",
		`{"requestId":"i","type":"Document","frameId":"IFRAME","request":{"url":"https://cdn.privacy-mgmt.com/index.html","method":"GET"}}`)
	report := buildTrackerReport(10)
	if report.Origin != "www.bbc.co.uk" {
		t.Fatalf("origin = %q, want the main frame host", report.Origin)
	}
	for _, e := range snapshotNetworkLog() {
		if strings.Contains(e.URL, "privacy-mgmt") && !e.ThirdParty {
			t.Fatalf("iframe document not classified third-party: %+v", e)
		}
	}
}

func TestWireFlagsThirdPartyTrackerAndBeacon(t *testing.T) {
	e := netEntryWire(&netRequest{
		id:         "t:1",
		url:        "https://www.google-analytics.com/collect",
		typ:        "Ping",
		originHost: "example.com",
		ttfbMs:     42,
		cached:     true,
	})
	if e["tp"] != true {
		t.Fatalf("third_party flag missing: %v", e)
	}
	if e["tr"] != "Google Analytics" {
		t.Fatalf("tracker = %v", e["tr"])
	}
	if e["bc"] != true {
		t.Fatalf("beacon flag missing: %v", e)
	}
	if e["ttfb"] != int64(42) || e["dc"] != true {
		t.Fatalf("timing/cache flags missing: %v", e)
	}
}

func TestWireFlagsFirstPartyWithoutTracker(t *testing.T) {
	e := netEntryWire(&netRequest{
		id:         "t:1",
		url:        "https://www.example.com/app.js",
		typ:        "Script",
		originHost: "example.com",
	})
	if _, ok := e["tp"]; ok {
		t.Fatal("same-site request flagged third party")
	}
	if _, ok := e["tr"]; ok {
		t.Fatal("unknown host labelled a tracker")
	}
}

func TestHeadersMessageCarriesPostureAndTLS(t *testing.T) {
	h := map[string]string{"strict-transport-security": "max-age=31536000"}
	r := &netRequest{
		id:      "t:1",
		url:     "https://example.com/",
		status:  200,
		tab:     2,
		typ:     "Document",
		headers: h,
		posture: assessPosture(h, "secure"),
		tls:     &netTLS{Protocol: "TLS 1.3", Issuer: "Test CA"},
	}
	msg := netHeadersMessage(r)
	if msg["type"] != "headers" {
		t.Fatalf("type = %v", msg["type"])
	}
	e := msg["e"].(map[string]any)
	if e["pst"] == nil || e["tls"] == nil || e["hd"] == nil {
		t.Fatalf("headers message incomplete: %v", e)
	}
}

func TestRedirectsAppearOnTheWire(t *testing.T) {
	e := netEntryWire(&netRequest{
		id:  "t:1",
		url: "https://example.com/",
		redirects: []netRedirect{
			{Status: 301, From: "http://example.com/", To: "https://example.com/"},
		},
	})
	rd, ok := e["rd"].([]netRedirect)
	if !ok || len(rd) != 1 || rd[0].Status != 301 {
		t.Fatalf("rd = %v", e["rd"])
	}
}

// The headers window must not keep growing: past netHeaderEntries the oldest
// entries lose theirs (the entries themselves stay).
func TestHeaderWindowShedsOldest(t *testing.T) {
	defer resetNetCapture()()
	defer func() { netHeaderEntriesLimit = netHeaderEntriesDefault }()
	netHeaderEntriesLimit = 3
	defer resetNetMaxEntriesForTest()

	for i := 0; i < 6; i++ {
		id := string(rune('a' + i))
		tap(t, "Network.requestWillBeSent",
			`{"requestId":"`+id+`","type":"Document","request":{"url":"https://h`+id+`.example/","method":"GET"}}`)
		tap(t, "Network.responseReceived",
			`{"requestId":"`+id+`","type":"Document","response":{"status":200,"headers":{"server":"`+id+`"}}}`)
	}

	entries := snapshotNetworkLog()
	if len(entries) != 6 {
		t.Fatalf("entries = %d, want 6", len(entries))
	}
	withHeaders := 0
	for _, e := range entries {
		if e.Headers != nil {
			withHeaders++
		}
	}
	if withHeaders > 3 {
		t.Fatalf("entries with headers = %d, want <= 3", withHeaders)
	}
	if strings.TrimSpace(entries[5].Headers["server"]) != "f" {
		t.Fatalf("newest entry lost its headers: %v", entries[5].Headers)
	}
}

// Chromium reports securityState for every response. It is the only way to
// tell a verified chain from one the browser accepted because certificate
// errors were ignored — and the panel must not report the latter as healthy.
func TestUntrustedCertificateStateReachesPosture(t *testing.T) {
	defer resetNetCapture()()

	tap(t, "Network.requestWillBeSent",
		`{"requestId":"tls1","type":"Document","request":{"url":"https://bank.example/","method":"GET"}}`)
	tap(t, "Network.responseReceived",
		`{"requestId":"tls1","type":"Document","response":{`+
			`"status":200,"mimeType":"text/html","protocol":"h2",`+
			`"securityState":"insecure",`+
			`"securityDetails":{"protocol":"TLS 1.3","cipher":"AES_128_GCM",`+
			`"issuer":"Zscaler","subjectName":"bank.example",`+
			`"validFrom":1700000000,"validTo":1800000000,"sanList":["bank.example"]},`+
			`"headers":{`+
			`"Strict-Transport-Security":"max-age=31536000; includeSubDomains",`+
			`"Content-Security-Policy":"default-src 'self'; frame-ancestors 'none'",`+
			`"X-Content-Type-Options":"nosniff",`+
			`"Referrer-Policy":"no-referrer",`+
			`"Permissions-Policy":"geolocation=()",`+
			`"Cross-Origin-Opener-Policy":"same-origin",`+
			`"Cross-Origin-Embedder-Policy":"require-corp"}}}`)

	entries := snapshotNetworkLog()
	if len(entries) != 1 {
		t.Fatalf("entries = %d", len(entries))
	}
	e := entries[0]
	if e.TLS == nil || e.TLS.SecurityState != "insecure" {
		t.Fatalf("tls = %+v, want security_state insecure", e.TLS)
	}
	if e.Posture == nil {
		t.Fatal("posture missing")
	}
	if e.Posture.Score != 7 || e.Posture.Total != 7 {
		t.Fatalf("score = %d/%d, want 7/7", e.Posture.Score, e.Posture.Total)
	}
	if e.Posture.Grade != "C" {
		t.Fatalf("grade = %q, want C (untrusted transport caps an otherwise perfect set)", e.Posture.Grade)
	}
	if e.Posture.TLSState != "insecure" {
		t.Fatalf("posture tls_state = %q, want insecure", e.Posture.TLSState)
	}
}

func TestPlainHTTPResponseIsGradedWithoutTLS(t *testing.T) {
	defer resetNetCapture()()

	tap(t, "Network.requestWillBeSent",
		`{"requestId":"http1","type":"Document","request":{"url":"http://example.com/","method":"GET"}}`)
	tap(t, "Network.responseReceived",
		`{"requestId":"http1","type":"Document","response":{"status":200,"mimeType":"text/html","protocol":"http/1.1","securityState":"neutral","headers":{"Server":"nginx","Content-Security-Policy":"default-src 'self'"}}}`)

	e := snapshotNetworkLog()[0]
	if e.TLS != nil {
		t.Fatalf("plain HTTP must not invent a certificate summary: %+v", e.TLS)
	}
	if e.Posture == nil {
		t.Fatal("posture missing")
	}
	if e.Posture.TLSState != "neutral" {
		t.Fatalf("tls_state = %q, want neutral", e.Posture.TLSState)
	}
	if e.Posture.Grade != "D" {
		// score 1 → D; capping never raises it.
		t.Fatalf("grade = %q, want D", e.Posture.Grade)
	}
	if e.Posture.Note == "" {
		t.Fatal("a plaintext document must say so")
	}
}
