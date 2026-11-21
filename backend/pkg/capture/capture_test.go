package capture

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeStreamer is the in-container capture API with enough behavior to exercise
// the puller: start/stop/status/manifest/blob.
type fakeStreamer struct {
	mu       sync.Mutex
	items    []streamerItem
	blobs    map[string][]byte
	meta     streamerMeta
	stopResp streamerStop
	blobHits int
}

func newFakeStreamer(items []streamerItem, blobs map[string][]byte) *fakeStreamer {
	return &fakeStreamer{
		items: items,
		blobs: blobs,
		meta: streamerMeta{
			SchemaVersion: 1,
			Recording:     true,
		},
		stopResp: streamerStop{Complete: true, EndedAt: time.Now().UTC().Format(time.RFC3339Nano)},
	}
}

func (f *fakeStreamer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/capture/start":
		var body struct {
			SessionID string `json:"session_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.meta.SessionID = body.SessionID
		f.meta.RecordID = body.SessionID
		f.meta.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
		startedAt := f.meta.StartedAt
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "record_id": body.SessionID, "started_at": startedAt})
	case r.Method == http.MethodPost && r.URL.Path == "/capture/stop":
		f.mu.Lock()
		f.meta.Recording = false
		resp := f.stopResp
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodGet && r.URL.Path == "/capture/status":
		f.mu.Lock()
		resp := streamerStatus{
			Recording: f.meta.Recording,
			SessionID: f.meta.SessionID,
			StartedAt: f.meta.StartedAt,
			Items:     len(f.items),
			Truncated: f.meta.Truncated,
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodGet && r.URL.Path == "/capture/manifest":
		f.mu.Lock()
		resp := streamerManifest{Meta: f.meta, Items: append([]streamerItem(nil), f.items...)}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(resp)
	case r.Method == http.MethodGet && r.URL.Path == "/capture/blob":
		sha := r.URL.Query().Get("sha256")
		f.mu.Lock()
		data, ok := f.blobs[sha]
		f.blobHits++
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

func shaOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newTestManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	if cfg.Dir == "" {
		cfg.Dir = filepath.Join(t.TempDir(), "captures")
	}
	m := NewManager(cfg, zerolog.Nop())
	m.pollEvery = 20 * time.Millisecond
	t.Cleanup(m.Close)
	return m
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readArchive(t *testing.T, m *Manager, id string) *metaFile {
	t.Helper()
	meta, err := readMeta(archiveDir(m.cfg.Dir, id))
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	return meta
}

func TestPollerIngestsDedupesAndStops(t *testing.T) {
	data := []byte("console.log('hi')")
	sha := shaOf(data)
	items := []streamerItem{
		{ID: "r1", URL: "https://a.example/app.js", FinalURL: "https://a.example/app.js", Method: "GET", Status: 200, Mime: "text/javascript", Type: "Script", SizeDecoded: int64(len(data)), SHA256: sha, State: "captured"},
		// Same bytes served from a second URL: one blob, two items.
		{ID: "r2", URL: "https://a.example/app.js?v=2", Mime: "text/javascript", SizeDecoded: int64(len(data)), SHA256: sha, State: "captured"},
		{ID: "r3", URL: "https://a.example/huge.bin", State: "too_large"},
	}
	fake := newFakeStreamer(items, map[string][]byte{sha: data})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	m := newTestManager(t, Config{Enabled: true})
	addr := strings.TrimPrefix(srv.URL, "http://")
	res, err := m.Start(context.Background(), "session-1", "https://a.example/", addr)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if res.ArchiveID != "session-1" || len(res.ArchiveKey) != 64 {
		t.Fatalf("start result = %+v", res)
	}

	waitFor(t, "three items", func() bool {
		meta, err := readMeta(archiveDir(m.cfg.Dir, "session-1"))
		return err == nil && len(meta.Items) == 3
	})

	meta := readArchive(t, m, "session-1")
	if meta.Counts["captured"] != 2 || meta.Counts["too_large"] != 1 {
		t.Fatalf("counts = %v", meta.Counts)
	}
	if meta.Counts["deduped"] != 1 {
		t.Fatalf("deduped = %d, want 1", meta.Counts["deduped"])
	}

	// One blob on disk, named by its verified sha256.
	path := blobPath(archiveDir(m.cfg.Dir, "session-1"), sha)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("blob file: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("blob mode = %v, want 0644", info.Mode().Perm())
	}
	metaInfo, err := os.Stat(metaPath(archiveDir(m.cfg.Dir, "session-1")))
	if err != nil {
		t.Fatalf("meta file: %v", err)
	}
	if metaInfo.Mode().Perm() != 0o600 {
		t.Fatalf("meta mode = %v, want 0600", metaInfo.Mode().Perm())
	}

	// A second poll must not duplicate items or refetch the blob.
	time.Sleep(60 * time.Millisecond)
	meta = readArchive(t, m, "session-1")
	if len(meta.Items) != 3 {
		t.Fatalf("items after second poll = %d, want 3", len(meta.Items))
	}
	fake.mu.Lock()
	hits := fake.blobHits
	fake.mu.Unlock()
	if hits != 1 {
		t.Fatalf("blob requests = %d, want 1", hits)
	}

	stop, err := m.Stop(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !stop.Complete || stop.Items != 3 || stop.Bytes != int64(len(data)) {
		t.Fatalf("stop = %+v", stop)
	}
	meta = readArchive(t, m, "session-1")
	if !meta.Complete || meta.EndedAt == "" {
		t.Fatalf("archive not finalized: complete=%v ended_at=%q", meta.Complete, meta.EndedAt)
	}
	if !m.CheckKey("session-1", res.ArchiveKey) {
		t.Fatal("CheckKey rejected the issued key")
	}
	if m.CheckKey("session-1", "wrong") {
		t.Fatal("CheckKey accepted a wrong key")
	}
}

func TestInvalidHashIsRejected(t *testing.T) {
	good := []byte("good")
	bad := []byte("tampered")
	sha := shaOf(good)
	items := []streamerItem{
		{ID: "r1", URL: "https://a.example/x.js", Mime: "text/javascript", SizeDecoded: int64(len(good)), SHA256: sha, State: "captured"},
	}
	fake := newFakeStreamer(items, map[string][]byte{sha: bad})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	m := newTestManager(t, Config{Enabled: true})
	if _, err := m.Start(context.Background(), "session-hash", "https://a.example/", strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("start: %v", err)
	}

	waitFor(t, "failed item", func() bool {
		meta, err := readMeta(archiveDir(m.cfg.Dir, "session-hash"))
		return err == nil && len(meta.Items) == 1 && meta.Items[0].State == "failed"
	})
	if _, err := os.Stat(blobPath(archiveDir(m.cfg.Dir, "session-hash"), sha)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched blob was stored: %v", err)
	}
	meta := readArchive(t, m, "session-hash")
	if meta.Counts["failed"] != 1 {
		t.Fatalf("failed count = %d", meta.Counts["failed"])
	}
}

func TestMetaWriteIsAtomic(t *testing.T) {
	m := newTestManager(t, Config{Enabled: true})
	dir := archiveDir(m.cfg.Dir, "atomic")
	if err := os.MkdirAll(tmpDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	old := &metaFile{SchemaVersion: 1, ArchiveID: "atomic", SessionID: "atomic", KeySHA256: "deadbeef"}
	if err := writeMeta(dir, old); err != nil {
		t.Fatal(err)
	}

	// Fail the next write by removing write permission on the directory: no
	// half-written meta.json may be left behind and the old file must survive.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	newMeta := &metaFile{SchemaVersion: 1, ArchiveID: "atomic", SessionID: "atomic", Items: []Item{{ID: "x"}}}
	if err := writeMeta(dir, newMeta); err == nil {
		os.Chmod(dir, 0o700)
		t.Fatal("writeMeta succeeded on a read-only dir")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := readMeta(dir)
	if err != nil {
		t.Fatalf("old meta did not survive: %v", err)
	}
	if len(got.Items) != 0 || got.KeySHA256 != "deadbeef" {
		t.Fatalf("meta was partially overwritten: %+v", got)
	}

	// A stale tmp file from a crashed writer must not shadow meta.json.
	if err := os.WriteFile(filepath.Join(dir, "meta.json.tmp-stale"), []byte(`{"trunc`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMeta(dir); err != nil {
		t.Fatalf("stale tmp file broke the meta read: %v", err)
	}
}

func TestGCRetentionAndQuota(t *testing.T) {
	root := filepath.Join(t.TempDir(), "captures")
	now := time.Now().UTC()
	write := func(id string, ended time.Time, bytes int64) {
		dir := archiveDir(root, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		meta := &metaFile{
			SchemaVersion: 1,
			ArchiveID:     id,
			SessionID:     id,
			CreatedAt:     ended.Add(-time.Hour).Format(time.RFC3339Nano),
			EndedAt:       ended.Format(time.RFC3339Nano),
			Complete:      true,
			Sizes:         map[string]int64{"record_bytes": bytes},
		}
		if err := writeMeta(dir, meta); err != nil {
			t.Fatal(err)
		}
	}
	write("old-retention", now.Add(-40*24*time.Hour), 10)
	write("oldest-quota", now.Add(-2*time.Hour), 60)
	write("newest-quota", now.Add(-time.Hour), 60)

	m := newTestManager(t, Config{
		Enabled:         true,
		Dir:             root,
		TotalQuotaBytes: 100,
		Retention:       30 * 24 * time.Hour,
	})
	m.gcNow()

	if fileExists(archiveDir(root, "old-retention")) {
		t.Fatal("archive past retention survived")
	}
	if fileExists(archiveDir(root, "oldest-quota")) {
		t.Fatal("oldest archive over quota survived")
	}
	if !fileExists(archiveDir(root, "newest-quota")) {
		t.Fatal("newest archive was evicted")
	}
}

func TestFinalizeWritesTombstoneWhenStreamerGone(t *testing.T) {
	data := []byte("body")
	sha := shaOf(data)
	items := []streamerItem{
		{ID: "r1", URL: "https://a.example/a.js", Mime: "text/javascript", SizeDecoded: int64(len(data)), SHA256: sha, State: "captured"},
	}
	fake := newFakeStreamer(items, map[string][]byte{sha: data})
	srv := httptest.NewServer(fake)

	m := newTestManager(t, Config{Enabled: true})
	if _, err := m.Start(context.Background(), "session-final", "https://a.example/", strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "item ingested", func() bool {
		meta, err := readMeta(archiveDir(m.cfg.Dir, "session-final"))
		return err == nil && len(meta.Items) == 1
	})

	// The container is gone: the final pull cannot reach the streamer.
	srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.Finalize(ctx, "session-final")
	m.Finalize(ctx, "session-final") // idempotent

	dir := archiveDir(m.cfg.Dir, "session-final")
	if !hasTombstone(dir) {
		t.Fatal("tombstone missing after an unreachable finalize")
	}
	meta := readArchive(t, m, "session-final")
	if meta.Complete {
		t.Fatal("archive claims complete after a failed finalize")
	}
	if meta.EndedAt == "" {
		t.Fatal("ended_at not recorded on failed finalize")
	}
}

func TestZipContainsMetaAndBlobs(t *testing.T) {
	data := []byte("body-bytes")
	sha := shaOf(data)
	items := []streamerItem{
		{ID: "r1", URL: "https://a.example/a.txt", Mime: "text/plain", SizeDecoded: int64(len(data)), SHA256: sha, State: "captured"},
	}
	fake := newFakeStreamer(items, map[string][]byte{sha: data})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	m := newTestManager(t, Config{Enabled: true})
	if _, err := m.Start(context.Background(), "session-zip", "https://a.example/", strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "item ingested", func() bool {
		meta, err := readMeta(archiveDir(m.cfg.Dir, "session-zip"))
		return err == nil && len(meta.Items) == 1
	})
	if _, err := m.Stop(context.Background(), "session-zip"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	var buf bytes.Buffer
	if err := m.Zip(&buf, "session-zip"); err != nil {
		t.Fatalf("zip: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	got := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(rc)
		rc.Close()
		got[f.Name] = body
	}
	if _, ok := got["meta.json"]; !ok {
		t.Fatalf("zip has no meta.json: %v", len(got))
	}
	blobName := "blobs/" + sha[:2] + "/" + sha
	if string(got[blobName]) != string(data) {
		t.Fatalf("zip blob %q = %q", blobName, got[blobName])
	}
}

func TestPathTraversalRejected(t *testing.T) {
	m := newTestManager(t, Config{Enabled: true})
	sha := strings.Repeat("a", 64)

	if _, _, _, err := m.Blob("../escape", sha); !errors.Is(err, ErrInvalidArchiveID) {
		t.Fatalf("Blob traversal id err = %v", err)
	}
	if _, _, _, err := m.Blob("ok", "../../etc/passwd"); !errors.Is(err, ErrInvalidHash) {
		t.Fatalf("Blob traversal sha err = %v", err)
	}
	if _, err := m.Get("../../etc"); !errors.Is(err, ErrInvalidArchiveID) {
		t.Fatalf("Get traversal err = %v", err)
	}
	if err := m.Delete("a/b"); !errors.Is(err, ErrInvalidArchiveID) {
		t.Fatalf("Delete traversal err = %v", err)
	}
	if err := m.Zip(io.Discard, "a/b"); !errors.Is(err, ErrInvalidArchiveID) {
		t.Fatalf("Zip traversal err = %v", err)
	}
	if m.CheckKey("../escape", "x") {
		t.Fatal("CheckKey accepted a traversal id")
	}
}

func TestStatusAndList(t *testing.T) {
	data := []byte("x")
	sha := shaOf(data)
	items := []streamerItem{
		{ID: "r1", URL: "https://a.example/x", Mime: "text/plain", SizeDecoded: int64(len(data)), SHA256: sha, State: "captured"},
	}
	fake := newFakeStreamer(items, map[string][]byte{sha: data})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	m := newTestManager(t, Config{Enabled: true})
	if _, err := m.Start(context.Background(), "session-status", "https://a.example/", strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "item ingested", func() bool {
		meta, err := readMeta(archiveDir(m.cfg.Dir, "session-status"))
		return err == nil && len(meta.Items) == 1
	})

	st, err := m.Status(context.Background(), "session-status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Recording || st.Items != 1 || st.SessionID != "session-status" {
		t.Fatalf("status = %+v", st)
	}

	list, err := m.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ArchiveID != "session-status" || list[0].TargetURL != "https://a.example/" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Items != 1 || list[0].Bytes != int64(len(data)) {
		t.Fatalf("summary = %+v", list[0])
	}

	if _, err := m.Status(context.Background(), "never-recorded"); !errors.Is(err, ErrArchiveNotFound) {
		t.Fatalf("unrecorded status err = %v", err)
	}
}
