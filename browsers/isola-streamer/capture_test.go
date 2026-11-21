package main

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withCaptureStore swaps the process-wide record for one test.
func withCaptureStore(t *testing.T, s *captureStore) {
	t.Helper()
	prev := captureRec
	captureRec = s
	t.Cleanup(func() { captureRec = prev })
}

func newTestStore(t *testing.T) *captureStore {
	t.Helper()
	return newCaptureStore(t.TempDir())
}

func mustStart(t *testing.T, s *captureStore, sid string, maxFile, maxRecord int64) {
	t.Helper()
	if _, err := s.Start(sid, maxFile, maxRecord); err != nil {
		t.Fatalf("start: %v", err)
	}
}

func manifestOf(t *testing.T, s *captureStore) captureManifest {
	t.Helper()
	m, err := s.Manifest()
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	return m
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func itemURL(url string) captureItem { return captureItem{URL: url, FinalURL: url, Type: "Script"} }

func TestCaptureStartStopIdempotent(t *testing.T) {
	s := newTestStore(t)

	first, err := s.Start("sess-1", 1<<20, 4<<20)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	again, err := s.Start("sess-1", 1<<20, 4<<20)
	if err != nil {
		t.Fatalf("same-session start must be idempotent: %v", err)
	}
	if !again.Equal(first) {
		t.Fatalf("idempotent start moved started_at: %v vs %v", again, first)
	}
	if _, err := s.Start("sess-2", 1<<20, 4<<20); err == nil {
		t.Fatal("a second session must be rejected while recording")
	}

	s.SaveBody(itemURL("https://a.example/app.js"), []byte("hello"))
	if got := s.Status().Items; got != 1 {
		t.Fatalf("items = %d after one save, want 1", got)
	}

	res, err := s.Stop()
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if res.Items != 1 || res.Bytes != 5 || !res.Complete || res.EndedAt == "" {
		t.Fatalf("stop result = %+v", res)
	}
	res2, err := s.Stop()
	if err != nil {
		t.Fatalf("stop must be idempotent: %v", err)
	}
	if res2.Items != res.Items || res2.Bytes != res.Bytes {
		t.Fatalf("idempotent stop changed the tally: %+v vs %+v", res2, res)
	}
	if s.Status().Recording {
		t.Fatal("status still reports recording after stop")
	}
}

func TestCaptureDedupeSameHashTwoURLs(t *testing.T) {
	s := newTestStore(t)
	mustStart(t, s, "sess-1", 1<<20, 4<<20)

	body := []byte("console.log(1)\n")
	s.SaveBody(itemURL("https://a.example/app.js"), body)
	s.SaveBody(itemURL("https://b.example/app.js"), body)

	m := manifestOf(t, s)
	if len(m.Items) != 2 {
		t.Fatalf("items = %d, want two rows for two requests", len(m.Items))
	}
	digest := sha256Hex(body)
	for _, it := range m.Items {
		if it.SHA256 != digest {
			t.Fatalf("item sha256 = %q, want %q", it.SHA256, digest)
		}
		if it.State != captureItemStateCaptured {
			t.Fatalf("item state = %q", it.State)
		}
		if _, err := s.BlobPath(it.SHA256); err != nil {
			t.Fatalf("blob for %s: %v", it.URL, err)
		}
	}

	s.mu.Lock()
	blobs, dedup, bytes := len(s.blobs), s.dedup, s.bytes
	s.mu.Unlock()
	if blobs != 1 {
		t.Fatalf("blobs on disk = %d, want 1", blobs)
	}
	if dedup != 1 {
		t.Fatalf("deduped = %d, want 1", dedup)
	}
	if bytes != int64(len(body)) {
		t.Fatalf("bytes = %d, want %d", bytes, len(body))
	}
}

func TestCaptureSameURLNewHash(t *testing.T) {
	s := newTestStore(t)
	mustStart(t, s, "sess-1", 1<<20, 4<<20)

	const url = "https://a.example/app.js"
	v1, v2 := []byte("v1"), []byte("v2")
	s.SaveBody(itemURL(url), v1)
	s.SaveBody(itemURL(url), v2)

	m := manifestOf(t, s)
	if len(m.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(m.Items))
	}
	if m.Items[0].SHA256 == m.Items[1].SHA256 {
		t.Fatal("a new body for the same URL must be a new item with a new hash")
	}
	if m.Items[0].Filename == m.Items[1].Filename {
		t.Fatal("filenames must differ when the hash differs")
	}
	for _, it := range m.Items {
		p, err := s.BlobPath(it.SHA256)
		if err != nil {
			t.Fatalf("blob %s: %v", it.SHA256, err)
		}
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if sha256Hex(got) != it.SHA256 {
			t.Fatalf("blob content does not match its hash")
		}
	}
}

func TestCaptureTooLargeAndExcludedRows(t *testing.T) {
	s := newTestStore(t)
	mustStart(t, s, "sess-1", 4, 1<<20)

	// Decoded body over max_file_bytes: a row, no blob.
	it := itemURL("https://a.example/big.bin")
	it.SHA1 = "aa"
	s.SaveBody(it, []byte("way too long"))

	// Denied by type/size before any body read (no CDP call).
	netMu.Lock()
	excluded, ok := captureRowFor(&netRequest{typ: "Ping", url: "https://a.example/beacon"}, false)
	tooLarge, ok2 := captureRowFor(&netRequest{typ: "Script", url: "https://a.example/huge.js", bytes: netBodyHashMaxBytes + 1}, false)
	_, ok3 := captureRowFor(&netRequest{typ: "Media", url: "https://a.example/v.mp4"}, false)
	netMu.Unlock()
	if !ok || !ok2 {
		t.Fatalf("expected rows for excluded and oversized requests (ok=%v ok2=%v)", ok, ok2)
	}
	if ok3 {
		t.Fatal("media must stay excluded from the manifest")
	}
	s.AppendRow(excluded)
	s.AppendRow(tooLarge)

	m := manifestOf(t, s)
	if len(m.Items) != 3 {
		t.Fatalf("items = %d, want 3", len(m.Items))
	}
	states := map[string]int{}
	for _, row := range m.Items {
		states[row.State]++
		if row.State != captureItemStateCaptured && row.SHA256 != "" {
			t.Fatalf("%s row carries sha256 %q; the backend would try to pull a blob", row.State, row.SHA256)
		}
	}
	if states[captureItemStateTooLarge] != 2 || states[captureItemStateExcludedType] != 1 {
		t.Fatalf("states = %v", states)
	}
	if m.Meta.Counts.TooLarge != 2 || m.Meta.Counts.ExcludedType != 1 || m.Meta.Counts.Requests != 3 {
		t.Fatalf("counts = %+v", m.Meta.Counts)
	}
	if m.Meta.Truncated {
		t.Fatal("a per-file overage must not truncate the record")
	}
}

func TestCaptureManifestSurvivesCrash(t *testing.T) {
	dir := t.TempDir()
	s := newCaptureStore(dir)
	mustStart(t, s, "sess-1", 1<<20, 4<<20)
	s.SaveBody(itemURL("https://a.example/a.js"), []byte("a"))
	s.SaveBody(itemURL("https://a.example/b.js"), []byte("bb"))
	live := manifestOf(t, s)
	if live.Meta.Sizes.RecordBytes != 3 || live.Meta.Counts.Captured != 2 {
		t.Fatalf("live meta = %+v", live.Meta)
	}
	// Simulate a crash: the process dies without Stop(). meta.json and every
	// item line on disk must still parse.
	m, err := readCaptureManifest(filepath.Join(dir, "sess-1"))
	if err != nil {
		t.Fatalf("read after crash: %v", err)
	}
	if len(m.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(m.Items))
	}
	if !m.Meta.Recording || m.Meta.RecordID != "sess-1" || m.Meta.SchemaVersion != 1 {
		t.Fatalf("meta = %+v", m.Meta)
	}
	// A torn trailing line is ignored, not fatal.
	f, err := os.OpenFile(filepath.Join(dir, "sess-1", "items.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"r9","url":"https://a.exa`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	m, err = readCaptureManifest(filepath.Join(dir, "sess-1"))
	if err != nil {
		t.Fatalf("torn append must not fail the manifest: %v", err)
	}
	if len(m.Items) != 2 {
		t.Fatalf("torn line changed the item count: %d", len(m.Items))
	}
}

func TestCaptureQuotaTruncation(t *testing.T) {
	s := newTestStore(t)
	mustStart(t, s, "sess-1", 1<<20, 6)

	s.SaveBody(itemURL("https://a.example/a.js"), []byte("aaaa"))   // 4 bytes
	s.SaveBody(itemURL("https://a.example/b.js"), []byte("bbbbbb")) // would exceed 6
	s.SaveBody(itemURL("https://a.example/c.js"), []byte("cc"))     // dropped

	st := s.Status()
	if !st.Truncated {
		t.Fatal("budget overrun must set truncated")
	}
	if st.Items != 1 || st.Bytes != 4 {
		t.Fatalf("status after truncation = %+v", st)
	}
	m := manifestOf(t, s)
	if !m.Meta.Truncated || m.Meta.StopReason != "quota" {
		t.Fatalf("meta truncation = truncated:%v reason:%q", m.Meta.Truncated, m.Meta.StopReason)
	}
	if len(m.Items) != 1 {
		t.Fatalf("items = %d, want only the bytes that fit", len(m.Items))
	}

	res, err := s.Stop()
	if err != nil {
		t.Fatalf("stop after truncation: %v", err)
	}
	if res.Complete || !res.Truncated {
		t.Fatalf("stop result after truncation = %+v", res)
	}
}

func TestCaptureRejectsTraversal(t *testing.T) {
	s := newTestStore(t)
	for _, sid := range []string{"../etc", "a/b", "", "sess 1", strings.Repeat("a", 65), "sess.1"} {
		if _, err := s.Start(sid, 1<<20, 1<<20); err == nil {
			t.Fatalf("session id %q must be rejected", sid)
		}
	}
	mustStart(t, s, "sess-1", 1<<20, 1<<20)
	s.SaveBody(itemURL("https://a.example/x.js"), []byte("x"))

	for _, sha := range []string{"", "../x", strings.Repeat("A", 64), strings.Repeat("a", 63), strings.Repeat("a", 65), "zz" + strings.Repeat("a", 62)} {
		if _, err := s.BlobPath(sha); err == nil {
			t.Fatalf("sha256 %q must be rejected", sha)
		}
	}
	if p, err := s.BlobPath(sha256Hex([]byte("x"))); err != nil {
		t.Fatalf("valid sha: %v", err)
	} else if !strings.HasPrefix(p, s.dir) {
		t.Fatalf("blob path escaped the record dir: %s", p)
	}
	if _, err := s.BlobPath(sha256Hex([]byte("nope"))); err == nil {
		t.Fatal("an unknown hash must not resolve")
	}

	// The derived filename is a pair of safe tokens, never a path.
	if name := captureFilename("https://a.example.app/../../etc/passwd", "text/plain", strings.Repeat("a", 64)); strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		t.Fatalf("filename is not path-safe: %q", name)
	}
}

func TestCaptureBlobRouteHeaders(t *testing.T) {
	s := newTestStore(t)
	mustStart(t, s, "sess-1", 1<<20, 1<<20)
	body := []byte("<html>hi</html>")
	s.SaveBody(itemURL("https://a.example/index.html"), body)
	digest := sha256Hex(body)

	mux := http.NewServeMux()
	withCaptureStore(t, s)
	registerCaptureRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/capture/blob?sha256="+digest, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Bytes(); string(got) != string(body) {
		t.Fatalf("body = %q, want %q", got, body)
	}
	for h, want := range map[string]string{
		"Content-Type":           "application/octet-stream",
		"Content-Disposition":    "attachment",
		"X-Content-Type-Options": "nosniff",
	} {
		if got := rec.Header().Get(h); got != want {
			t.Fatalf("%s = %q, want %q", h, got, want)
		}
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/capture/blob?sha256="+sha256Hex([]byte("missing")), nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown sha status = %d, want 404", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/capture/blob?sha256=../etc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal sha status = %d, want 400", rec.Code)
	}

	// Manifest + status as the backend polls them.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/capture/manifest", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d", rec.Code)
	}
	var m captureManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("manifest json: %v", err)
	}
	if len(m.Items) != 1 || m.Items[0].SHA256 != digest {
		t.Fatalf("manifest = %+v", m)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/capture/status", nil))
	var st captureStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil || !st.Recording || st.Items != 1 {
		t.Fatalf("status = %s (err %v)", rec.Body.String(), err)
	}

	// Start/stop routes, including the busy conflict.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/capture/start", strings.NewReader(`{"session_id":"other"}`)))
	if rec.Code != http.StatusConflict {
		t.Fatalf("start while recording status = %d, want 409", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/capture/stop", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stop status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/capture/start", strings.NewReader(`{"session_id":"sess-1","max_file_bytes":1024,"max_record_bytes":4096}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("restart status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

// The fetch cap follows the record's per-file limit: a body the archive can
// store must be read back even when it is larger than the hash-only gate.
func TestCaptureFileLimitRaisesFetchCap(t *testing.T) {
	s := newTestStore(t)
	mustStart(t, s, "sess-1", 8<<20, 64<<20)
	withCaptureStore(t, s)

	prev := activeCapture
	activeCapture = &captureClient{}
	defer func() { activeCapture = prev }()

	const threeMiB = int64(3 << 20)
	if got := captureFetchLimit(); got != 8<<20 {
		t.Fatalf("fetch limit = %d with an 8 MiB record, want %d", got, int64(8<<20))
	}

	// 3 MiB script: above the hash-only gate, inside max_file_bytes → fetched
	// and stored.
	three := &netRequest{typ: "Script", url: "https://a.example/big.js", bytes: threeMiB}
	if !shouldHashBody(three, "sess-1") {
		t.Fatal("a 3 MiB script must be fetched while an 8 MiB record is active")
	}
	if _, tooLarge := captureRowFor(three, false); tooLarge {
		t.Fatal("a 3 MiB script inside max_file_bytes must not be a too_large row")
	}
	body := bytes.Repeat([]byte("x"), int(threeMiB))
	s.SaveBody(itemURL("https://a.example/big.js"), body)
	m := manifestOf(t, s)
	if len(m.Items) != 1 || m.Items[0].State != captureItemStateCaptured {
		t.Fatalf("items = %+v", m.Items)
	}
	p, err := s.BlobPath(m.Items[0].SHA256)
	if err != nil {
		t.Fatalf("blob for the 3 MiB body: %v", err)
	}
	if st, err := os.Stat(p); err != nil || st.Size() != threeMiB {
		t.Fatalf("blob size = %v (err %v), want %d", st, err, threeMiB)
	}

	// 9 MiB script: beyond max_file_bytes → nobody fetches it, row only.
	nine := &netRequest{typ: "Script", url: "https://a.example/huge.js", bytes: 9 << 20}
	if shouldHashBody(nine, "sess-1") {
		t.Fatal("a 9 MiB script must not be fetched when max_file_bytes is 8 MiB")
	}
	it, ok := captureRowFor(nine, false)
	if !ok || it.State != captureItemStateTooLarge {
		t.Fatalf("9 MiB row = %+v (ok=%v), want too_large", it, ok)
	}

	// Boundary: captureRowFor must never call something too_large that
	// shouldHashBody is willing to fetch.
	for _, n := range []int64{netBodyHashMaxBytes, netBodyHashMaxBytes + 1, 8 << 20} {
		r := &netRequest{typ: "Script", url: "https://a.example/cap.js", bytes: n}
		wantFetch := shouldHashBody(r, "sess-1")
		if !wantFetch {
			t.Fatalf("%d bytes must be fetchable under an 8 MiB record", n)
		}
		if _, tooLarge := captureRowFor(r, false); tooLarge {
			t.Fatalf("%d bytes is fetchable but classified too_large", n)
		}
	}
}

// Without a record the gate is still the plain 2 MiB hash cap.
func TestCaptureFileLimitWithoutRecordKeepsHashGate(t *testing.T) {
	prevCapture := activeCapture
	activeCapture = &captureClient{}
	defer func() { activeCapture = prevCapture }()

	for _, rec := range []*captureStore{nil, newTestStore(t)} { // nil, and a store that never started
		withCaptureStore(t, rec)
		if got := captureFetchLimit(); got != netBodyHashMaxBytes {
			t.Fatalf("fetch limit = %d without a record, want %d", got, int64(netBodyHashMaxBytes))
		}
		over := &netRequest{typ: "Script", url: "https://a.example/big.js", bytes: 3 << 20}
		if shouldHashBody(over, "sess-1") {
			t.Fatal("3 MiB must stay excluded by the 2 MiB gate without a record")
		}
		it, ok := captureRowFor(over, false)
		if !ok || it.State != captureItemStateTooLarge {
			t.Fatalf("3 MiB row without a record = %+v (ok=%v), want too_large", it, ok)
		}
		atCap := &netRequest{typ: "Script", url: "https://a.example/cap.js", bytes: netBodyHashMaxBytes}
		if !shouldHashBody(atCap, "sess-1") {
			t.Fatal("exactly 2 MiB must still be fetched without a record")
		}
		if _, tooLarge := captureRowFor(atCap, false); tooLarge {
			t.Fatal("exactly 2 MiB must not be too_large without a record")
		}
	}
}

// The capture hook: while recording, the existing hash worker must persist the
// same bytes it hashes and still publish the entry with its SHA-1.
func TestHashNetBodyPersistsCapture(t *testing.T) {
	s := newTestStore(t)
	mustStart(t, s, "sess-1", 1<<20, 1<<20)
	withCaptureStore(t, s)
	defer resetNetCapture()()

	prev := activeCapture
	defer func() { activeCapture = prev }()

	body := "console.log('hi')"
	srv := fakeCDPServer(t, body, false, nil)
	c := newCaptureClient()
	c.baseURL = srv.URL
	go c.Start()
	activeCapture = c
	waitCaptureConnected(t, c)
	defer c.dropConn()

	netMonitorTap("target-aaaa", "sess-1", "Network.requestWillBeSent",
		json.RawMessage(`{"requestId":"1","type":"Script","request":{"url":"https://a.example/app.js","method":"GET"}}`), 1)
	netMonitorTap("target-aaaa", "sess-1", "Network.loadingFinished",
		json.RawMessage(`{"requestId":"1","encodedDataLength":18}`), 1)

	want := sha1Hex([]byte(body))
	deadline := time.After(3 * time.Second)
	for {
		m, err := s.Manifest()
		if err == nil && len(m.Items) == 1 {
			it := m.Items[0]
			if it.State != captureItemStateCaptured {
				t.Fatalf("item state = %q", it.State)
			}
			if it.SHA1 != want {
				t.Fatalf("item sha1 = %q, want %q", it.SHA1, want)
			}
			if it.SHA256 != sha256Hex([]byte(body)) {
				t.Fatalf("item sha256 = %q", it.SHA256)
			}
			p, err := s.BlobPath(it.SHA256)
			if err != nil {
				t.Fatalf("blob: %v", err)
			}
			got, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != body {
				t.Fatalf("blob = %q, want %q", got, body)
			}
			if it.Filename == "" || !strings.HasSuffix(it.Filename, ".js") {
				t.Fatalf("filename = %q, want a .js name", it.Filename)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for the capture row (err=%v)", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// When no record is active the hash path must stay byte-for-byte hash-only.
func TestHashNetBodyWithoutRecordStoresNothing(t *testing.T) {
	withCaptureStore(t, nil)
	defer resetNetCapture()()

	prev := activeCapture
	defer func() { activeCapture = prev }()

	srv := fakeCDPServer(t, "hi", false, nil)
	c := newCaptureClient()
	c.baseURL = srv.URL
	go c.Start()
	activeCapture = c
	waitCaptureConnected(t, c)
	defer c.dropConn()

	netMonitorTap("target-aaaa", "sess-1", "Network.requestWillBeSent",
		json.RawMessage(`{"requestId":"1","type":"Script","request":{"url":"https://a.example/app.js","method":"GET"}}`), 1)
	netMonitorTap("target-aaaa", "sess-1", "Network.loadingFinished",
		json.RawMessage(`{"requestId":"1","encodedDataLength":2}`), 1)

	deadline := time.After(3 * time.Second)
	for {
		netMu.Lock()
		r := netRequests["target-aaaa|1"]
		h := ""
		if r != nil {
			h = r.sha1
		}
		netMu.Unlock()
		if h != "" {
			if h != sha1Hex([]byte("hi")) {
				t.Fatalf("sha1 = %q", h)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatal("hash path did not publish a SHA-1 without a record")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func sha1Hex(b []byte) string {
	sum := sha1.Sum(b)
	return fmt.Sprintf("%x", sum)
}
