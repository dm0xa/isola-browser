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
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
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

	// Readable tree: one copy of the bytes under two names (hardlinked).
	dir := archiveDir(m.cfg.Dir, "session-1")
	first := filepath.Join(dir, "files", "a.example", "app.js")
	info, err := os.Stat(first)
	if err != nil {
		t.Fatalf("readable file: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("file mode = %v, want 0644", info.Mode().Perm())
	}
	second := filepath.Join(dir, "files", "a.example", "app__v_2.js")
	secondInfo, err := os.Stat(second)
	if err != nil {
		t.Fatalf("deduped file: %v", err)
	}
	if !os.SameFile(info, secondInfo) {
		t.Fatal("dedupe did not hardlink the second name")
	}
	if fileExists(blobPath(dir, sha)) {
		t.Fatal("CAS blob path still written")
	}
	if meta.Items[0].File != "files/a.example/app.js" || meta.Items[1].File != "files/a.example/app__v_2.js" {
		t.Fatalf("item files = %q, %q", meta.Items[0].File, meta.Items[1].File)
	}
	if meta.Items[0].Filename != "app.js" || meta.Items[1].Filename != "app__v_2.js" {
		t.Fatalf("item filenames = %q, %q", meta.Items[0].Filename, meta.Items[1].Filename)
	}
	waitFor(t, "index.txt batch", func() bool {
		raw, readErr := os.ReadFile(filepath.Join(dir, indexName))
		if readErr != nil {
			return false
		}
		return strings.Contains(string(raw), "https://a.example/app.js\tfiles/a.example/app.js\tcaptured\ttext/javascript")
	})
	metaInfo, err := os.Stat(metaPath(dir))
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

func TestZipContainsReadableTree(t *testing.T) {
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
	if _, ok := got[indexName]; !ok {
		t.Fatalf("zip has no %s", indexName)
	}
	filePath := "files/a.example/a.txt"
	if string(got[filePath]) != string(data) {
		t.Fatalf("zip file %q = %q entries=%v", filePath, got[filePath], len(got))
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

	// The readable-file lookup must never leave files/.
	for _, rel := range []string{
		"../meta.json",
		"files/../meta.json",
		"files/a/../../etc/passwd",
		"/files/a/x",
		"files/a\\x",
		"files//x",
		"files/./x",
	} {
		if _, _, _, err := m.File("ok", rel); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("File(%q) err = %v, want ErrInvalidPath", rel, err)
		}
	}
	if _, _, _, err := m.File("../escape", "files/a/x"); !errors.Is(err, ErrInvalidArchiveID) {
		t.Fatalf("File traversal id err = %v", err)
	}
}

func TestFileAcceptsRelAndFullPath(t *testing.T) {
	m := newTestManager(t, Config{Enabled: true})
	dir := archiveDir(m.cfg.Dir, "shape")
	rel := "files/a.example/x.js"
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha := shaOf([]byte("body"))
	meta := &metaFile{
		SchemaVersion: 1,
		ArchiveID:     "shape",
		SessionID:     "shape",
		Items: []Item{{
			URL: "https://a.example/x.js", SHA256: sha, File: rel,
			State: "captured", Mime: "text/javascript",
		}},
	}
	if err := writeMeta(dir, meta); err != nil {
		t.Fatal(err)
	}

	for _, wantPath := range []string{"a.example/x.js", rel} {
		p, mime, name, err := m.File("shape", wantPath)
		if err != nil {
			t.Fatalf("File(%q): %v", wantPath, err)
		}
		if p != target || mime != "text/javascript" || name != "x.js" {
			t.Fatalf("File(%q) = %q, %q, %q", wantPath, p, mime, name)
		}
	}
}

func TestDeriveRel(t *testing.T) {
	cases := []struct {
		name      string
		url, mime string
		want      string
	}{
		{"document", "https://Example.COM/", "text/html", "files/example.com/index.html"},
		{"script", "https://a.example/app.js", "text/javascript", "files/a.example/app.js"},
		{"query", "https://a.example/app.js?v=2", "text/javascript", "files/a.example/app__v_2.js"},
		{"default port", "https://a.example:443/x.css", "text/css", "files/a.example/x.css"},
		{"custom port", "http://a.example:8080/x", "text/plain", "files/a.example__8080/x.txt"},
		{"no host", "file:///tmp/notes", "text/plain", "files/local/tmp/notes.txt"},
		{"nested", "https://a.example/blog/posts/one", "text/html", "files/a.example/blog/posts/one.html"},
		{"trailing slash", "https://a.example/blog/", "text/html", "files/a.example/blog/index.html"},
		{"mime ext", "https://a.example/style", "text/css", "files/a.example/style.css"},
		{"no mime ext", "https://a.example/data", "application/octet-stream", "files/a.example/data"},
		{"sanitize", "https://a.example/a b/c%3Fd", "application/octet-stream", "files/a.example/a_b/c_d"},
	}
	for _, tc := range cases {
		got, err := deriveRel(tc.url, "", tc.mime)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}

	for _, bad := range []string{
		"https://a.example/%2e%2e/secret",
		"https://a.example/a/../../b",
		"https://a.example/a%2F..%2Fb",
		"https://a.example/a\\b",
	} {
		if _, err := deriveRel(bad, "", "text/plain"); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("deriveRel(%q) err = %v, want ErrInvalidPath", bad, err)
		}
	}

	// Deeper than 8 segments: first 7 plus the last.
	deep, err := deriveRel("https://a.example/1/2/3/4/5/6/7/8/9.js", "", "text/javascript")
	if err != nil {
		t.Fatal(err)
	}
	if deep != "files/a.example/1/2/3/4/5/6/7/9.js" {
		t.Fatalf("deep path = %q", deep)
	}
}

func TestVersionedNameOnChangedContent(t *testing.T) {
	first := []byte("one")
	second := []byte("two")
	sha1, sha2 := shaOf(first), shaOf(second)
	items := []streamerItem{
		{ID: "r1", URL: "https://a.example/load.php", Mime: "text/html", SizeDecoded: int64(len(first)), SHA256: sha1, State: "captured"},
		{ID: "r2", URL: "https://a.example/load.php", Mime: "text/html", SizeDecoded: int64(len(second)), SHA256: sha2, State: "captured"},
	}
	fake := newFakeStreamer(items, map[string][]byte{sha1: first, sha2: second})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	m := newTestManager(t, Config{Enabled: true})
	if _, err := m.Start(context.Background(), "session-ver", "https://a.example/", strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "two items", func() bool {
		meta, err := readMeta(archiveDir(m.cfg.Dir, "session-ver"))
		return err == nil && len(meta.Items) == 2
	})

	meta := readArchive(t, m, "session-ver")
	wantVersioned := "files/a.example/load~" + sha2[:12] + ".php"
	if meta.Items[0].File != "files/a.example/load.php" {
		t.Fatalf("first file = %q", meta.Items[0].File)
	}
	if meta.Items[1].File != wantVersioned {
		t.Fatalf("versioned file = %q want %q", meta.Items[1].File, wantVersioned)
	}
	dir := archiveDir(m.cfg.Dir, "session-ver")
	if !fileExists(filepath.Join(dir, filepath.FromSlash(wantVersioned))) {
		t.Fatal("versioned file missing on disk")
	}
	if meta.Items[1].SHA256 != sha2 {
		t.Fatalf("versioned item sha = %q", meta.Items[1].SHA256)
	}
	if meta.Items[0].Filename != "load.php" || meta.Items[1].Filename != path.Base(wantVersioned) {
		t.Fatalf("filenames = %q, %q", meta.Items[0].Filename, meta.Items[1].Filename)
	}
}

func TestQueryTokenDropsVolatileParams(t *testing.T) {
	// With csrf_token dropped but platform kept, the token is 59 characters and
	// fits under the 64-character cap.
	const ya = "https://ya.ru/portal/feed/web/actions?action_type=batch&blocks=rythm_feed_action&csrf_token=dc986f320d58fc0e8f2ac0c6659d56c296ef1a34%3A1795299522&platform=desktop"
	got, err := deriveRel(ya, "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	const words = "action_type_batch_blocks_rythm_feed_action"
	want := "files/ya.ru/portal/feed/web/actions__" + words + "_platform_desktop.json"
	if got != want {
		t.Fatalf("deriveRel = %q want %q", got, want)
	}

	// Without the non-volatile platform pair the same endpoint keeps its full
	// words inside the cap.
	core := "https://ya.ru/portal/feed/web/actions?action_type=batch&blocks=rythm_feed_action&csrf_token=abc123"
	coreRel, err := deriveRel(core, "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if want := "files/ya.ru/portal/feed/web/actions__" + words + ".json"; coreRel != want {
		t.Fatalf("deriveRel core = %q want %q", coreRel, want)
	}

	a, err := deriveRel("https://ya.ru/x?q=1&csrf_token=aaa", "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	b, err := deriveRel("https://ya.ru/x?q=1&csrf_token=bbb", "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a != "files/ya.ru/x__q_1.json" {
		t.Fatalf("csrf variants = %q, %q", a, b)
	}

	c, err := deriveRel("https://ya.ru/x?_=1795299522&cb=99", "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if c != "files/ya.ru/x.json" {
		t.Fatalf("cache-buster rel = %q", c)
	}

	// A query made only of volatile keys names no file variant at all.
	if d, err := deriveRel("https://ya.ru/x?ts=1&nonce=2", "", "application/json"); err != nil || d != "files/ya.ru/x.json" {
		t.Fatalf("volatile-only query rel = %q, %v", d, err)
	}

	// A token longer than 64 characters clamps deterministically.
	long := "https://ya.ru/x?" + strings.Repeat("a", 30) + "=1&" + strings.Repeat("b", 40) + "=2"
	l1, err := deriveRel(long, "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	l2, err := deriveRel(long, "", "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if l1 != l2 {
		t.Fatalf("clamp not deterministic: %q vs %q", l1, l2)
	}
	base := path.Base(l1)
	token := strings.TrimSuffix(strings.TrimPrefix(base, "x__"), ".json")
	if len(token) != maxQueryLen {
		t.Fatalf("clamped token len = %d, want %d (%q)", len(token), maxQueryLen, token)
	}
	if strings.HasSuffix(token, "_") {
		t.Fatalf("clamped token keeps a trailing separator: %q", token)
	}
}

func TestIndexCollapsesRepeatedRequests(t *testing.T) {
	m := newTestManager(t, Config{Enabled: true})
	dir := archiveDir(m.cfg.Dir, "index-repeat")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("d", 64)
	row := func(ts int64) Item {
		return Item{URL: "https://ya.ru/api?q=1", File: "files/ya.ru/api__q_1.json", State: "captured", Mime: "application/json", SizeDecoded: 42, SHA256: sha, TSRequested: ts}
	}
	items := []Item{row(1), row(2), row(3), row(4)}
	if err := writeIndex(dir, items); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, indexName))
	if err != nil {
		t.Fatal(err)
	}
	want := "https://ya.ru/api?q=1\tfiles/ya.ru/api__q_1.json\tcaptured\tapplication/json\t42\tdddddddddddd\tx4\n"
	if string(raw) != want {
		t.Fatalf("index = %q want %q", raw, want)
	}

	// meta.json keeps every request.
	if len(items) != 4 {
		t.Fatalf("meta rows = %d", len(items))
	}

	// Non-repeated rows stay single.
	items = append(items, Item{URL: "https://ya.ru/api?q=1", File: "files/ya.ru/api__q_1.json", State: "captured", Mime: "application/json", SizeDecoded: 42, SHA256: strings.Repeat("e", 64), TSRequested: 5})
	if err := writeIndex(dir, items); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, indexName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "\tx4") || strings.HasSuffix(lines[1], "\tx") {
		t.Fatalf("index lines = %q", lines)
	}
}

func TestIndexSortedByURLThenTime(t *testing.T) {
	m := newTestManager(t, Config{Enabled: true})
	dir := archiveDir(m.cfg.Dir, "index-order")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	items := []Item{
		{URL: "https://b.example/", File: "files/b.example/index.html", State: "captured", Mime: "text/html", SizeDecoded: 11, SHA256: strings.Repeat("b", 64), TSRequested: 100},
		{URL: "https://a.example/", File: "files/a.example/index.html", State: "captured", Mime: "text/html", SizeDecoded: 3, SHA256: strings.Repeat("a", 64), TSRequested: 200},
		{URL: "https://a.example/", File: "files/a.example/index~2.html", State: "captured", Mime: "text/html", SizeDecoded: 4, SHA256: strings.Repeat("c", 64), TSRequested: 100},
		{URL: "https://c.example/x", State: "too_large", Mime: "image/png", SizeDecoded: 9000},
	}
	if err := writeIndex(dir, items); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, indexName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	want := []string{
		"https://a.example/\tfiles/a.example/index~2.html\tcaptured\ttext/html\t4\tcccccccccccc",
		"https://a.example/\tfiles/a.example/index.html\tcaptured\ttext/html\t3\taaaaaaaaaaaa",
		"https://b.example/\tfiles/b.example/index.html\tcaptured\ttext/html\t11\tbbbbbbbbbbbb",
		"https://c.example/x\t-\ttoo_large\timage/png\t9000\t-",
	}
	if len(lines) != len(want) {
		t.Fatalf("index lines = %d: %q", len(lines), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %q want %q", i, lines[i], want[i])
		}
	}
}

func TestIngestRejectsTraversalURL(t *testing.T) {
	data := []byte("evil")
	sha := shaOf(data)
	items := []streamerItem{
		{ID: "r1", URL: "https://a.example/..%2f..%2fescape.txt", Mime: "text/plain", SizeDecoded: int64(len(data)), SHA256: sha, State: "captured", File: "files/../../escape.txt"},
	}
	fake := newFakeStreamer(items, map[string][]byte{sha: data})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	m := newTestManager(t, Config{Enabled: true})
	if _, err := m.Start(context.Background(), "session-evil", "https://a.example/", strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, "rejected item", func() bool {
		meta, err := readMeta(archiveDir(m.cfg.Dir, "session-evil"))
		return err == nil && len(meta.Items) == 1 && meta.Items[0].State == "failed"
	})
	meta := readArchive(t, m, "session-evil")
	if meta.Items[0].File != "" {
		t.Fatalf("traversal item kept a file path: %q", meta.Items[0].File)
	}
	if meta.Counts["failed"] != 1 {
		t.Fatalf("failed count = %d", meta.Counts["failed"])
	}
	dir := archiveDir(m.cfg.Dir, "session-evil")
	if fileExists(filepath.Join(dir, "escape.txt")) || fileExists(filepath.Join(dir, "..", "escape.txt")) {
		t.Fatal("traversal item wrote outside files/")
	}
}

func TestMigrateLegacyCASArchive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "captures")
	id := "31e43100-f1dd-460b-9f73-74b693a80c67"
	dir := archiveDir(root, id)

	jsData := []byte("console.log('legacy')")
	jsSHA := shaOf(jsData)
	cssData := []byte("body{color:red}")
	cssSHA := shaOf(cssData)
	goneSHA := shaOf([]byte("gone"))

	for sha, data := range map[string][]byte{jsSHA: jsData, cssSHA: cssData} {
		if err := os.MkdirAll(blobDir(dir, sha), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(blobPath(dir, sha), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	meta := &metaFile{
		SchemaVersion: 1,
		ArchiveID:     id,
		SessionID:     id,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Complete:      true,
		Items: []Item{
			// Flat filename from the old layout: migration must rewrite it.
			{ID: "r1", URL: "https://example.com/s.js", Mime: "text/javascript", SizeDecoded: int64(len(jsData)), SHA256: jsSHA, Filename: "example.com_" + jsSHA[:12] + ".js", State: "captured"},
			// Legacy streamer-supplied path that does not exist on the host.
			{ID: "r2", URL: "https://example.com/nested/app.css", Mime: "text/css", SizeDecoded: int64(len(cssData)), SHA256: cssSHA, File: "files/aa/" + cssSHA, State: "captured"},
			// No legacy bytes at all: skipped, never fatal.
			{ID: "r3", URL: "https://example.com/gone.png", Mime: "image/png", SHA256: goneSHA, State: "captured"},
		},
	}
	if err := writeMeta(dir, meta); err != nil {
		t.Fatal(err)
	}

	// NewManager migrates before its GC pass.
	m := newTestManager(t, Config{Enabled: true, Dir: root})

	got := readArchive(t, m, id)
	if got.Items[0].File != "files/example.com/s.js" {
		t.Fatalf("item 0 file = %q", got.Items[0].File)
	}
	if got.Items[0].Filename != "s.js" {
		t.Fatalf("item 0 filename = %q, want s.js", got.Items[0].Filename)
	}
	if got.Items[1].File != "files/example.com/nested/app.css" {
		t.Fatalf("item 1 file = %q", got.Items[1].File)
	}
	if got.Items[1].Filename != "app.css" {
		t.Fatalf("item 1 filename = %q, want app.css", got.Items[1].Filename)
	}
	if got.Items[2].File != "" {
		t.Fatalf("item without legacy bytes got a file: %q", got.Items[2].File)
	}

	jsPath := filepath.Join(dir, "files", "example.com", "s.js")
	info, err := os.Stat(jsPath)
	if err != nil {
		t.Fatalf("migrated file: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("migrated mode = %v, want 0644", info.Mode().Perm())
	}
	blobInfo, err := os.Stat(blobPath(dir, jsSHA))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, blobInfo) {
		t.Fatal("migration copied instead of hardlinking")
	}
	if !fileExists(filepath.Join(dir, "files", "example.com", "nested", "app.css")) {
		t.Fatal("nested legacy file not migrated")
	}

	indexRaw, err := os.ReadFile(filepath.Join(dir, indexName))
	if err != nil {
		t.Fatalf("index.txt after migration: %v", err)
	}
	if !strings.Contains(string(indexRaw), "https://example.com/s.js\tfiles/example.com/s.js\tcaptured\ttext/javascript") {
		t.Fatalf("index.txt = %q", indexRaw)
	}

	// The sha route still resolves the old bytes.
	p, mime, _, err := m.Blob(id, jsSHA)
	if err != nil {
		t.Fatalf("Blob after migration: %v", err)
	}
	if p != jsPath || mime != "text/javascript" {
		t.Fatalf("Blob = %q, %q", p, mime)
	}
	if body, err := os.ReadFile(p); err != nil || !bytes.Equal(body, jsData) {
		t.Fatalf("blob bytes = %q, %v", body, err)
	}

	// One captured item has no readable file, so the CAS tree is kept.
	if !fileExists(filepath.Join(dir, "blobs")) {
		t.Fatal("legacy blobs removed although an item had no readable file")
	}

	// Idempotent: a second pass rewrites nothing and adds no versioned files.
	beforeMeta, err := os.ReadFile(metaPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	beforeFiles := archiveFileTree(t, dir)
	m.Migrate()
	afterMeta, err := os.ReadFile(metaPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeMeta, afterMeta) {
		t.Fatal("second migration rewrote meta.json")
	}
	afterFiles := archiveFileTree(t, dir)
	if len(afterFiles) != len(beforeFiles) {
		t.Fatalf("second migration changed the tree: %v -> %v", beforeFiles, afterFiles)
	}
	for i := range afterFiles {
		if afterFiles[i] != beforeFiles[i] {
			t.Fatalf("second migration changed the tree: %v -> %v", beforeFiles, afterFiles)
		}
	}
}

// archiveFileTree lists the readable files under files/ for idempotency checks.
func archiveFileTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(dir, "files"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			rel, relErr := filepath.Rel(dir, p)
			if relErr == nil {
				out = append(out, filepath.ToSlash(rel))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk files tree: %v", err)
	}
	return out
}

func TestMigrateRemovesLegacyBlobsWhenComplete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "captures")
	id := "complete-archive"
	dir := archiveDir(root, id)

	data := []byte("console.log('complete')")
	sha := shaOf(data)
	if err := os.MkdirAll(blobDir(dir, sha), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath(dir, sha), data, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := &metaFile{
		SchemaVersion: 1,
		ArchiveID:     id,
		SessionID:     id,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Complete:      true,
		Items: []Item{
			{ID: "r1", URL: "https://example.com/s.js", Mime: "text/javascript", SizeDecoded: int64(len(data)), SHA256: sha, State: "captured"},
		},
	}
	if err := writeMeta(dir, meta); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, Config{Enabled: true, Dir: root})

	got := readArchive(t, m, id)
	if got.Items[0].File != "files/example.com/s.js" {
		t.Fatalf("file = %q", got.Items[0].File)
	}
	if fileExists(filepath.Join(dir, "blobs")) {
		t.Fatal("legacy blobs survived a complete migration")
	}
	migrated := filepath.Join(dir, "files", "example.com", "s.js")
	if body, err := os.ReadFile(migrated); err != nil || !bytes.Equal(body, data) {
		t.Fatalf("migrated bytes = %q, %v", body, err)
	}

	// The sha lookup still serves the body after the CAS tree is gone.
	p, mime, _, err := m.Blob(id, sha)
	if err != nil {
		t.Fatalf("Blob after blob removal: %v", err)
	}
	if p != migrated || mime != "text/javascript" {
		t.Fatalf("Blob = %q, %q", p, mime)
	}

	// Idempotent: a second pass neither recreates nor needs blobs/.
	m.Migrate()
	if fileExists(filepath.Join(dir, "blobs")) {
		t.Fatal("second migration recreated blobs/")
	}
	if !fileExists(migrated) {
		t.Fatal("second migration lost the readable file")
	}
}

func TestMigrateKeepsLegacyBlobsWhenIncomplete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "captures")
	id := "partial-archive"
	dir := archiveDir(root, id)

	data := []byte("body")
	sha := shaOf(data)
	goneSHA := shaOf([]byte("gone"))
	if err := os.MkdirAll(blobDir(dir, sha), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blobPath(dir, sha), data, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := &metaFile{
		SchemaVersion: 1,
		ArchiveID:     id,
		SessionID:     id,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Items: []Item{
			{ID: "r1", URL: "https://example.com/a.txt", Mime: "text/plain", SizeDecoded: int64(len(data)), SHA256: sha, State: "captured"},
			{ID: "r2", URL: "https://example.com/gone.bin", Mime: "application/octet-stream", SHA256: goneSHA, State: "captured"},
		},
	}
	if err := writeMeta(dir, meta); err != nil {
		t.Fatal(err)
	}

	m := newTestManager(t, Config{Enabled: true, Dir: root})

	got := readArchive(t, m, id)
	if got.Items[0].File != "files/example.com/a.txt" {
		t.Fatalf("migrated file = %q", got.Items[0].File)
	}
	if got.Items[1].File != "" {
		t.Fatalf("unmigratable item got a file: %q", got.Items[1].File)
	}
	if !fileExists(blobPath(dir, sha)) {
		t.Fatal("legacy blob removed although the archive is incomplete")
	}
	if !fileExists(filepath.Join(dir, "files", "example.com", "a.txt")) {
		t.Fatal("complete item not migrated")
	}

	// The CAS fallback still serves archives that keep blobs/.
	if _, _, _, err := m.Blob(id, sha); err != nil {
		t.Fatalf("Blob for migrated sha: %v", err)
	}
	if _, _, _, err := m.Blob(id, goneSHA); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("Blob for missing sha err = %v", err)
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
