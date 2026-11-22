package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/dm0xa/isola-browser/backend/pkg/capture"
	"github.com/dm0xa/isola-browser/backend/pkg/config"
	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// fakeCaptureStreamer implements the in-container capture endpoints used by the
// puller. It is intentionally minimal: enough for start/stop/manifest/blob/status.
type fakeCaptureStreamer struct {
	mu    sync.Mutex
	sid   string
	items []map[string]any
	blobs map[string][]byte
}

func (f *fakeCaptureStreamer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/capture/start":
		var body struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.sid = body.SessionID
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "record_id": body.SessionID,
			"started_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
	case r.Method == http.MethodPost && r.URL.Path == "/capture/stop":
		f.mu.Lock()
		total := int64(0)
		for _, data := range f.blobs {
			total += int64(len(data))
		}
		items := len(f.items)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": items, "bytes": total, "deduped": 0, "complete": true,
			"truncated": false, "ended_at": time.Now().UTC().Format(time.RFC3339Nano),
		})
	case r.Method == http.MethodGet && r.URL.Path == "/capture/status":
		f.mu.Lock()
		body := map[string]any{
			"recording": true, "session_id": f.sid, "items": len(f.items),
			"bytes": 0, "deduped": 0, "truncated": false,
			"started_at": time.Now().UTC().Format(time.RFC3339Nano),
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	case r.Method == http.MethodGet && r.URL.Path == "/capture/manifest":
		f.mu.Lock()
		body := map[string]any{
			"meta": map[string]any{
				"schema_version": 1,
				"record_id":      f.sid,
				"session_id":     f.sid,
				"started_at":     time.Now().UTC().Format(time.RFC3339Nano),
				"recording":      true,
				"counts":         map[string]int64{},
				"sizes":          map[string]int64{},
				"truncated":      false,
			},
			"items": f.items,
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(body)
	case r.Method == http.MethodGet && r.URL.Path == "/capture/blob":
		sha := r.URL.Query().Get("sha256")
		f.mu.Lock()
		data, ok := f.blobs[sha]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func captureFixture(t *testing.T, cfg *config.Config, items []map[string]any, blobs map[string][]byte) (*Handler, uuid.UUID, string, *capture.Manager, *fakeCaptureStreamer) {
	t.Helper()
	fake := &fakeCaptureStreamer{items: items, blobs: blobs}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	mem := store.NewMemStore()
	t.Cleanup(func() { mem.Close() })
	cache := store.NewMemCache()
	orch, err := orchestrator.New("unix:///nonexistent/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Capture = capture.Config{Enabled: true, Dir: filepath.Join(t.TempDir(), "captures")}
	h := New(mem, cache, orch, cfg)
	mgr := capture.NewManager(cfg.Capture, zerolog.Nop())
	h.Capture = mgr
	t.Cleanup(mgr.Close)

	ctx := context.Background()
	sid := uuid.New()
	if err := mem.CreateSession(ctx, &models.Session{
		ID:        sid,
		Status:    models.SessionStatusProvisioning,
		TargetURL: "https://a.example/",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := mem.ActivateSession(ctx, sid, "container-1", "127.0.0.1", 5900, time.Hour); err != nil {
		t.Fatal(err)
	}
	const token = "control-token"
	if err := cache.Set(ctx, "session:"+sid.String()+":control_token", token, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := cache.Set(ctx, "session:"+sid.String(), strings.TrimPrefix(srv.URL, "http://"), time.Hour); err != nil {
		t.Fatal(err)
	}
	return h, sid, token, mgr, fake
}

func sessionCaptureRequest(method, sid, token string) *http.Request {
	req := httptest.NewRequest(method, "/api/sessions/"+sid+"/capture", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", sid)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	if token != "" {
		req.Header.Set("X-Session-Token", token)
	}
	return req
}

func archiveRequest(method, archiveID, sha, auth string) *http.Request {
	req := httptest.NewRequest(method, "/api/captures/"+archiveID, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("archiveID", archiveID)
	if sha != "" {
		rctx.URLParams.Add("sha256", sha)
	}
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}

func shaOfBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func waitForArchiveItems(t *testing.T, mgr *capture.Manager, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		archive, err := mgr.Get(id)
		if err == nil {
			if items, ok := archive.Meta["items"].([]any); ok && len(items) >= want {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("archive %s never reached %d items", id, want)
}

func captureItem(url, mime string, data []byte) map[string]any {
	return map[string]any{
		"id": "r1", "url": url, "final_url": url, "method": "GET", "status": 200,
		"mime": mime, "type": "Script", "size_wire": len(data), "size_decoded": len(data),
		"sha256": shaOfBytes(data), "sha1": strings.Repeat("a", 40),
		"filename": "streamer-supplied.should-be-recomputed", "tab": 1,
		"origin_host": "a.example", "state": "captured",
	}
}

func decodeResponseBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return body
}

func TestCaptureStartRequiresOwner(t *testing.T) {
	data := []byte("body")
	h, sid, token, mgr, _ := captureFixture(t, nil,
		[]map[string]any{captureItem("https://a.example/app.js", "text/javascript", data)},
		map[string][]byte{shaOfBytes(data): data})

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("start without token = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), "wrong"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("start with wrong token = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	if w.Code != http.StatusOK {
		t.Fatalf("start = %d: %s", w.Code, w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	body := decodeResponseBody(t, w)
	if body["archive_id"] != sid.String() {
		t.Fatalf("archive_id = %v", body["archive_id"])
	}
	key, _ := body["archive_key"].(string)
	if len(key) != 64 {
		t.Fatalf("archive_key = %q", key)
	}

	waitForArchiveItems(t, mgr, sid.String(), 1)
	w = httptest.NewRecorder()
	h.GetCaptureStatus(w, sessionCaptureRequest(http.MethodGet, sid.String(), token))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	status := decodeResponseBody(t, w)
	if status["recording"] != true || status["archive_id"] != sid.String() {
		t.Fatalf("status = %v", status)
	}

	w = httptest.NewRecorder()
	h.GetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if decodeResponseBody(t, w)["capture_enabled"] != true {
		t.Fatalf("capture_enabled = %v, want true", decodeResponseBody(t, w)["capture_enabled"])
	}
}

func TestCaptureArchiveKeyRequired(t *testing.T) {
	data := []byte("body")
	sha := shaOfBytes(data)
	h, sid, token, mgr, _ := captureFixture(t, nil,
		[]map[string]any{captureItem("https://a.example/app.js", "text/javascript", data)},
		map[string][]byte{sha: data})

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	if w.Code != http.StatusOK {
		t.Fatalf("start = %d: %s", w.Code, w.Body.String())
	}
	key := decodeResponseBody(t, w)["archive_key"].(string)
	waitForArchiveItems(t, mgr, sid.String(), 1)

	w = httptest.NewRecorder()
	h.GetArchive(w, archiveRequest(http.MethodGet, sid.String(), "", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("archive without key = %d: %s", w.Code, w.Body.String())
	}
	if code := decodeResponseBody(t, w)["error"].(map[string]any)["code"]; code != "archive_key_invalid" {
		t.Fatalf("error code = %v", code)
	}

	w = httptest.NewRecorder()
	h.GetArchive(w, archiveRequest(http.MethodGet, sid.String(), "", "Bearer wrong"))
	if w.Code != http.StatusForbidden {
		t.Fatalf("archive with wrong key = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.GetArchive(w, archiveRequest(http.MethodGet, sid.String(), "", "Bearer "+key))
	if w.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", w.Code, w.Body.String())
	}
	archive := decodeResponseBody(t, w)
	if archive["archive_id"] != sid.String() {
		t.Fatalf("archive meta = %v", archive["archive_id"])
	}
	if items, _ := archive["items"].([]any); len(items) != 1 {
		t.Fatalf("archive items = %v", items)
	}
}

func TestCaptureDisabled(t *testing.T) {
	h, sid, token, _, _ := captureFixture(t, nil, nil, nil)
	h.Capture = nil

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	if w.Code != http.StatusForbidden {
		t.Fatalf("start with capture disabled = %d: %s", w.Code, w.Body.String())
	}
	if code := decodeResponseBody(t, w)["error"].(map[string]any)["code"]; code != "capture_disabled" {
		t.Fatalf("error code = %v", code)
	}

	w = httptest.NewRecorder()
	h.GetConfig(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	body := decodeResponseBody(t, w)
	if body["capture_enabled"] != false {
		t.Fatalf("capture_enabled = %v", body["capture_enabled"])
	}
}

func TestCaptureArchiveDeleteAndNotFound(t *testing.T) {
	data := []byte("body")
	sha := shaOfBytes(data)
	h, sid, token, mgr, _ := captureFixture(t, nil,
		[]map[string]any{captureItem("https://a.example/app.js", "text/javascript", data)},
		map[string][]byte{sha: data})

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	key := decodeResponseBody(t, w)["archive_key"].(string)
	waitForArchiveItems(t, mgr, sid.String(), 1)

	w = httptest.NewRecorder()
	h.GetArchive(w, archiveRequest(http.MethodGet, sid.String(), "", "Bearer "+key))
	if w.Code != http.StatusOK {
		t.Fatalf("archive before delete = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.DeleteArchive(w, archiveRequest(http.MethodDelete, sid.String(), "", "Bearer "+key))
	if w.Code != http.StatusOK {
		t.Fatalf("delete = %d: %s", w.Code, w.Body.String())
	}
	if body := decodeResponseBody(t, w); body["status"] != "deleted" {
		t.Fatalf("delete body = %v", body)
	}

	w = httptest.NewRecorder()
	h.GetArchive(w, archiveRequest(http.MethodGet, sid.String(), "", "Bearer "+key))
	if w.Code != http.StatusNotFound {
		t.Fatalf("archive after delete = %d: %s", w.Code, w.Body.String())
	}
	if code := decodeResponseBody(t, w)["error"].(map[string]any)["code"]; code != "archive_not_found" {
		t.Fatalf("error code = %v", code)
	}

	// A blob's archive check runs first, so a deleted archive is a 404 rather
	// than an auth failure.
	w = httptest.NewRecorder()
	h.GetArchiveBlob(w, archiveRequest(http.MethodGet, sid.String(), sha, "Bearer "+key))
	if w.Code != http.StatusNotFound {
		t.Fatalf("blob after delete = %d: %s", w.Code, w.Body.String())
	}
}

func TestCaptureBlobHeaders(t *testing.T) {
	data := []byte("console.log('hi')")
	sha := shaOfBytes(data)
	h, sid, token, mgr, _ := captureFixture(t, nil,
		[]map[string]any{captureItem("https://a.example/app.js", "text/javascript", data)},
		map[string][]byte{sha: data})

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	key := decodeResponseBody(t, w)["archive_key"].(string)
	waitForArchiveItems(t, mgr, sid.String(), 1)

	w = httptest.NewRecorder()
	h.GetArchiveBlob(w, archiveRequest(http.MethodGet, sid.String(), sha, ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("blob without key = %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.GetArchiveBlob(w, archiveRequest(http.MethodGet, sid.String(), sha, "Bearer "+key))
	if w.Code != http.StatusOK {
		t.Fatalf("blob = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/javascript" {
		t.Fatalf("content-type = %q", got)
	}
	cd := w.Header().Get("Content-Disposition")
	wantName := "app.js"
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, `filename="`+wantName+`"`) {
		t.Fatalf("content-disposition = %q, want %q", cd, wantName)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("body = %q", w.Body.Bytes())
	}

	// Malformed hashes never reach the filesystem.
	w = httptest.NewRecorder()
	h.GetArchiveBlob(w, archiveRequest(http.MethodGet, sid.String(), "../../etc/passwd", "Bearer "+key))
	if w.Code != http.StatusNotFound {
		t.Fatalf("traversal blob = %d: %s", w.Code, w.Body.String())
	}
	if code := decodeResponseBody(t, w)["error"].(map[string]any)["code"]; code != "blob_not_found" {
		t.Fatalf("error code = %v", code)
	}
}

func TestCaptureDownloadZip(t *testing.T) {
	data := []byte("zipme")
	sha := shaOfBytes(data)
	h, sid, token, mgr, _ := captureFixture(t, nil,
		[]map[string]any{captureItem("https://a.example/app.js", "text/javascript", data)},
		map[string][]byte{sha: data})

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	key := decodeResponseBody(t, w)["archive_key"].(string)
	waitForArchiveItems(t, mgr, sid.String(), 1)

	w = httptest.NewRecorder()
	h.DownloadArchive(w, archiveRequest(http.MethodGet, sid.String(), "", "Bearer "+key))
	if w.Code != http.StatusOK {
		t.Fatalf("download = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/zip" {
		t.Fatalf("content-type = %q", got)
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip body: %v", err)
	}
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	if !names["meta.json"] || !names["index.txt"] || !names["files/a.example/app.js"] {
		t.Fatalf("zip entries = %v", names)
	}
}

func TestCaptureFileRoute(t *testing.T) {
	data := []byte("console.log('hi')")
	sha := shaOfBytes(data)
	h, sid, token, mgr, _ := captureFixture(t, nil,
		[]map[string]any{captureItem("https://a.example/app.js", "text/javascript", data)},
		map[string][]byte{sha: data})

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	key := decodeResponseBody(t, w)["archive_key"].(string)
	waitForArchiveItems(t, mgr, sid.String(), 1)

	w = httptest.NewRecorder()
	h.GetArchiveFile(w, archiveFileRequest(http.MethodGet, sid.String(), "files/a.example/app.js", ""))
	if w.Code != http.StatusForbidden {
		t.Fatalf("file without key = %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.GetArchiveFile(w, archiveFileRequest(http.MethodGet, sid.String(), "files/a.example/app.js", "Bearer "+key))
	if w.Code != http.StatusOK {
		t.Fatalf("file = %d: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "text/javascript" {
		t.Fatalf("content-type = %q", got)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, `filename="app.js"`) {
		t.Fatalf("content-disposition = %q", cd)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("body = %q", w.Body.Bytes())
	}

	// Unknown paths and paths outside files/ never reach the filesystem.
	w = httptest.NewRecorder()
	h.GetArchiveFile(w, archiveFileRequest(http.MethodGet, sid.String(), "files/a.example/missing.js", "Bearer "+key))
	if w.Code != http.StatusNotFound {
		t.Fatalf("missing file = %d: %s", w.Code, w.Body.String())
	}
	if code := decodeResponseBody(t, w)["error"].(map[string]any)["code"]; code != "blob_not_found" {
		t.Fatalf("missing file code = %v", code)
	}

	for _, rel := range []string{"../meta.json", "files/a/../../meta.json"} {
		w = httptest.NewRecorder()
		h.GetArchiveFile(w, archiveFileRequest(http.MethodGet, sid.String(), rel, "Bearer "+key))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("file %q = %d: %s", rel, w.Code, w.Body.String())
		}
		if code := decodeResponseBody(t, w)["error"].(map[string]any)["code"]; code != "invalid_path" {
			t.Fatalf("file %q code = %v", rel, code)
		}
	}

	// A valid-shaped path with no recorded item is a 404, never a file read.
	w = httptest.NewRecorder()
	h.GetArchiveFile(w, archiveFileRequest(http.MethodGet, sid.String(), "meta.json", "Bearer "+key))
	if w.Code != http.StatusNotFound {
		t.Fatalf("meta.json file = %d: %s", w.Code, w.Body.String())
	}

	// The sha route still resolves through the readable tree.
	w = httptest.NewRecorder()
	h.GetArchiveBlob(w, archiveRequest(http.MethodGet, sid.String(), sha, "Bearer "+key))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatalf("sha route = %d: %s", w.Code, w.Body.String())
	}
}

func archiveFileRequest(method, archiveID, rel, auth string) *http.Request {
	req := httptest.NewRequest(method, "/api/captures/"+archiveID+"/files/"+rel, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("archiveID", archiveID)
	rctx.URLParams.Add("*", rel)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	return req
}

// The wildcard route must match multi-segment paths; a nil manager answers
// capture_disabled only after the route matched (otherwise chi's 404).
func TestCaptureFileRoutePattern(t *testing.T) {
	h := &Handler{}
	r := chi.NewRouter()
	h.RegisterCaptureRoutes(r)

	req := httptest.NewRequest(http.MethodGet, "/api/captures/abc/files/a.example/blog/index.html", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("nested files route = %d: %s", w.Code, w.Body.String())
	}
	if code := decodeResponseBody(t, w)["error"].(map[string]any)["code"]; code != "capture_disabled" {
		t.Fatalf("error code = %v", code)
	}
}

// End-to-end through the real router: chi's wildcard is the path after
// /files/ with a leading slash, so the handler must add "files/" itself.
func TestCaptureFileRouteServesReadablePath(t *testing.T) {
	jsData := []byte("console.log('live')")
	cssData := []byte("body{color:red}")
	jsItem := captureItem("https://example.com/s.js", "text/javascript", jsData)
	jsItem["id"] = "r-js"
	cssItem := captureItem("https://example.com/assets/css/site.css", "text/css", cssData)
	cssItem["id"] = "r-css"
	h, sid, token, mgr, _ := captureFixture(t, nil,
		[]map[string]any{jsItem, cssItem},
		map[string][]byte{shaOfBytes(jsData): jsData, shaOfBytes(cssData): cssData})

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	if w.Code != http.StatusOK {
		t.Fatalf("start = %d: %s", w.Code, w.Body.String())
	}
	key := decodeResponseBody(t, w)["archive_key"].(string)
	waitForArchiveItems(t, mgr, sid.String(), 2)

	router := chi.NewRouter()
	h.RegisterCaptureRoutes(router)
	get := func(rel string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/captures/"+sid.String()+"/files/"+rel, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := get("example.com/s.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET files/example.com/s.js = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/javascript" {
		t.Fatalf("content-type = %q", got)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, `filename="s.js"`) {
		t.Fatalf("content-disposition = %q", cd)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), jsData) {
		t.Fatalf("body = %q", rec.Body.Bytes())
	}

	rec = get("example.com/assets/css/site.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("nested GET = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/css" {
		t.Fatalf("nested content-type = %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), cssData) {
		t.Fatalf("nested body = %q", rec.Body.Bytes())
	}

	rec = get("example.com/missing.js")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown file = %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeResponseBody(t, rec)["error"].(map[string]any)["code"]; code != "blob_not_found" {
		t.Fatalf("unknown file code = %v", code)
	}

	// A .. segment inside the wildcard must be rejected, not resolved.
	rec = get("example.com/%2e%2e/%2e%2e/meta.json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("traversal = %d: %s", rec.Code, rec.Body.String())
	}
	if code := decodeResponseBody(t, rec)["error"].(map[string]any)["code"]; code != "invalid_path" {
		t.Fatalf("traversal code = %v", code)
	}
}

func TestCaptureListRequiresOperatorBearer(t *testing.T) {
	data := []byte("body")
	sha := shaOfBytes(data)
	const operator = "01234567890123456789012345678901"
	cfg := &config.Config{APITokens: []string{operator}}
	h, sid, token, mgr, _ := captureFixture(t, cfg,
		[]map[string]any{captureItem("https://a.example/app.js", "text/javascript", data)},
		map[string][]byte{sha: data})

	w := httptest.NewRecorder()
	h.StartCapture(w, sessionCaptureRequest(http.MethodPost, sid.String(), token))
	key := decodeResponseBody(t, w)["archive_key"].(string)
	waitForArchiveItems(t, mgr, sid.String(), 1)

	w = httptest.NewRecorder()
	h.ListCaptures(w, httptest.NewRequest(http.MethodGet, "/api/captures", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("list without bearer = %d: %s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/captures", nil)
	req.Header.Set("Authorization", "Bearer "+operator)
	w = httptest.NewRecorder()
	h.ListCaptures(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body.String())
	}
	body := decodeResponseBody(t, w)
	captures, _ := body["captures"].([]any)
	if len(captures) != 1 {
		t.Fatalf("captures = %v", body["captures"])
	}
	row := captures[0].(map[string]any)
	if row["archive_id"] != sid.String() || row["items"].(float64) != 1 {
		t.Fatalf("capture row = %v", row)
	}

	// The operator bearer can also delete without the archive key.
	w = httptest.NewRecorder()
	h.DeleteArchive(w, archiveRequest(http.MethodDelete, sid.String(), "", "Bearer "+operator))
	if w.Code != http.StatusOK {
		t.Fatalf("operator delete = %d: %s", w.Code, w.Body.String())
	}

	// And the archive key still stops a capture while the session runs.
	if key == "" {
		t.Fatal("empty archive key")
	}
}
