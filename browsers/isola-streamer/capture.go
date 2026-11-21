package main

// Resource capture: while a record is active, every decoded response body the
// network monitor already reads back for hashing is written to disk, deduped by
// sha256, next to one metadata row per request. Paths and lifetimes are owned by
// the backend (it pulls the record and archives it); this side only has to be
// crash-safe, quota-bounded and thread-safe.
//
// Layout of one record directory (<ISOLA_CAPTURE_DIR>/<session_id>):
//
//	items.jsonl                            one item per line, fsynced on every append
//	meta.json                              atomic rewrite on start/stop/truncation
//	files/<host>/<segments>/<name>.<ext>   decoded response body, 0644
//	tmp-<random>                           blob being written, renamed into place
//
// The files/ tree is readable on purpose: a human must be able to find a body
// by its URL rather than by its SHA-256. One body is stored once per sha256
// under its first URL; later rows pointing at the same bytes reuse that path
// (File repeats) and the backend builds the host-side hardlinks. Every path
// atom is sanitized and the assembled rel is re-validated before it touches
// the filesystem, so no URL can escape files/.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// captureDirDefault is where records are written unless ISOLA_CAPTURE_DIR
	// overrides it. Never /tmp: that tmpfs already holds HOME and the Chromium
	// profile, and filling it freezes the session.
	captureDirDefault = "/home/isolauser/capture"

	captureItemStateCaptured     = "captured"
	captureItemStateTooLarge     = "too_large"
	captureItemStateMissingBody  = "missing_body"
	captureItemStateExcludedType = "excluded_type"
	captureItemStateFailed       = "failed"

	captureItemIDPrefix = "r"

	// captureMaxStartBodyBytes bounds the start JSON (three numbers and a session id).
	captureMaxStartBodyBytes = 64 << 10
)

// captureSessionIDRe: archive ids are user-supplied filenames, so this is the
// whole traversal defence.
var captureSessionIDRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// captureSHA256Re is strict: no uppercase, no prefix, no length tolerance.
var captureSHA256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)

// captureCounts mirrors the manifest's per-state tally.
type captureCounts struct {
	Requests     int `json:"requests"`
	Captured     int `json:"captured"`
	HashOnly     int `json:"hash_only"`
	TooLarge     int `json:"too_large"`
	ExcludedType int `json:"excluded_type"`
	MissingBody  int `json:"missing_body"`
	Failed       int `json:"failed"`
}

type captureSizes struct {
	RecordBytes int64 `json:"record_bytes"`
}

type captureLimits struct {
	MaxFileBytes   int64 `json:"max_file_bytes"`
	MaxRecordBytes int64 `json:"max_record_bytes"`
}

// captureMeta is the meta.json document (and the "meta" member of the manifest).
type captureMeta struct {
	SchemaVersion int           `json:"schema_version"`
	RecordID      string        `json:"record_id"`
	SessionID     string        `json:"session_id"`
	StartedAt     string        `json:"started_at"`
	EndedAt       *string       `json:"ended_at"`
	Recording     bool          `json:"recording"`
	StopReason    string        `json:"stop_reason"`
	Counts        captureCounts `json:"counts"`
	Sizes         captureSizes  `json:"sizes"`
	Truncated     bool          `json:"truncated"`
	Limits        captureLimits `json:"limits"`
}

// captureItem is one request's manifest row. sha256/filename/file are empty
// for every state except captured; the backend recomputes the readable path
// from the URL rather than trusting it.
type captureItem struct {
	ID                string `json:"id"`
	URL               string `json:"url"`
	FinalURL          string `json:"final_url"`
	Method            string `json:"method"`
	Status            int    `json:"status"`
	Mime              string `json:"mime"`
	Type              string `json:"type"`
	SizeWire          int64  `json:"size_wire"`
	SizeDecoded       int64  `json:"size_decoded"`
	SHA256            string `json:"sha256,omitempty"`
	SHA1              string `json:"sha1,omitempty"`
	Filename          string `json:"filename,omitempty"`
	File              string `json:"file,omitempty"`
	Tab               int    `json:"tab"`
	OriginHost        string `json:"origin_host"`
	TSRequested       int64  `json:"ts_requested"`
	TSFinished        int64  `json:"ts_finished"`
	TSSaved           int64  `json:"ts_saved"`
	FromDiskCache     bool   `json:"from_disk_cache"`
	FromServiceWorker bool   `json:"from_service_worker"`
	State             string `json:"state"`
}

type captureManifest struct {
	Meta  captureMeta   `json:"meta"`
	Items []captureItem `json:"items"`
}

type captureStatus struct {
	Recording bool          `json:"recording"`
	SessionID string        `json:"session_id"`
	StartedAt string        `json:"started_at"`
	Items     int           `json:"items"`
	Bytes     int64         `json:"bytes"`
	Deduped   int           `json:"deduped"`
	Truncated bool          `json:"truncated"`
	Counts    captureCounts `json:"counts"`
}

type captureStopResult struct {
	Items     int    `json:"items"`
	Bytes     int64  `json:"bytes"`
	Deduped   int    `json:"deduped"`
	Complete  bool   `json:"complete"`
	Truncated bool   `json:"truncated"`
	EndedAt   string `json:"ended_at"`
}

var (
	errCaptureBusy        = errors.New("another session is recording")
	errCaptureBadSession  = errors.New("invalid session id")
	errCaptureNotActive   = errors.New("no recording active")
	errCaptureBadSHA256   = errors.New("invalid sha256")
	errCaptureBlobMissing = errors.New("blob not found")
)

// captureStore owns one record at a time. Every mutating method is a no-op
// once the record is truncated or stopped, so a late hash worker cannot
// resurrect a finished archive.
type captureStore struct {
	baseDir string

	mu         sync.Mutex
	sessionID  string
	dir        string
	recording  bool
	startedAt  time.Time
	endedAt    time.Time
	stopReason string
	truncated  bool
	maxFile    int64
	maxRecord  int64

	itemsF *os.File
	seq    int
	items  int
	bytes  int64
	dedup  int
	counts captureCounts

	// blobs maps sha256 → its canonical record-relative path ("files/…");
	// rels is the reverse index used to keep two different bodies off the
	// same readable name. Both are rebuilt from scratch on Start.
	blobs map[string]string
	rels  map[string]string

	// warnWrite logs the first blob write failure only; a full disk would
	// otherwise emit one line per resource.
	warnWrite bool
}

func newCaptureStore(baseDir string) *captureStore {
	return &captureStore{baseDir: baseDir, blobs: map[string]string{}, rels: map[string]string{}}
}

// captureDirFromEnv resolves the configured capture directory.
func captureDirFromEnv() string {
	if v := os.Getenv("ISOLA_CAPTURE_DIR"); v != "" {
		return v
	}
	return captureDirDefault
}

// captureRec is the process-wide record, nil when capture is unavailable.
var captureRec *captureStore

// initCapture prepares the capture directory and publishes the store. Any
// failure disables capture without touching the rest of the streamer.
func initCapture() {
	dir := captureDirFromEnv()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("capture: disabled dir=%s err=%v", dir, err)
		return
	}
	captureRec = newCaptureStore(dir)
	log.Printf("capture: ready dir=%s", dir)
}

// captureActive reports whether a record is currently accepting items. Hot
// path (called under netMu), so it only touches the store mutex briefly.
func captureActive() bool {
	s := captureRec
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recording && !s.truncated
}

// captureFetchLimit is the largest body worth reading back: the plain hash cap
// unless a record is active, in which case its per-file limit wins when bigger.
// Agrees with shouldHashBody by construction — both call it.
func captureFetchLimit() int64 {
	s := captureRec
	if s == nil {
		return netBodyHashMaxBytes
	}
	return s.fileLimit()
}

// fileLimit reports the effective per-file cap. Lock order is netMu -> store
// mutex (callers of shouldHashBody hold netMu).
func (s *captureStore) fileLimit() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.recording || s.truncated || s.maxFile <= netBodyHashMaxBytes {
		return netBodyHashMaxBytes
	}
	return s.maxFile
}

func (s *captureStore) Active() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recording && !s.truncated
}

// ---------- lifecycle ----------

func (s *captureStore) Start(sessionID string, maxFileBytes, maxRecordBytes int64) (time.Time, error) {
	if !captureSessionIDRe.MatchString(sessionID) {
		return time.Time{}, errCaptureBadSession
	}
	if maxFileBytes <= 0 {
		maxFileBytes = netBodyHashMaxBytes
	}
	if maxRecordBytes <= 0 {
		maxRecordBytes = 256 << 20
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recording {
		if s.sessionID == sessionID {
			return s.startedAt, nil // idempotent for the same session
		}
		return time.Time{}, errCaptureBusy
	}

	dir := filepath.Join(s.baseDir, sessionID)
	// A fresh start owns the directory: a leftover record from an earlier run
	// of the same session would otherwise be merged into the new manifest.
	if err := os.RemoveAll(dir); err != nil {
		return time.Time{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return time.Time{}, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "items.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return time.Time{}, err
	}

	s.sessionID = sessionID
	s.dir = dir
	s.recording = true
	s.startedAt = time.Now().UTC()
	s.endedAt = time.Time{}
	s.stopReason = ""
	s.truncated = false
	s.maxFile = maxFileBytes
	s.maxRecord = maxRecordBytes
	s.itemsF = f
	s.seq = 0
	s.items = 0
	s.bytes = 0
	s.dedup = 0
	s.counts = captureCounts{}
	s.blobs = map[string]string{}
	s.rels = map[string]string{}
	s.warnWrite = false

	if err := s.writeMetaLocked(); err != nil {
		s.closeItemsLocked()
		s.recording = false
		return time.Time{}, err
	}
	log.Printf("capture: start dir=%s session=%s", dir, sessionID)
	return s.startedAt, nil
}

func (s *captureStore) Stop() (captureStopResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recording {
		s.recording = false
		s.endedAt = time.Now().UTC()
		if s.stopReason == "" {
			s.stopReason = "viewer_stop"
		}
		s.closeItemsLocked()
		if err := s.writeMetaLocked(); err != nil {
			return captureStopResult{}, err
		}
		log.Printf("capture: stop session=%s items=%d bytes=%d truncated=%v", s.sessionID, s.items, s.bytes, s.truncated)
	}
	ended := s.endedAt
	if ended.IsZero() {
		ended = time.Now().UTC()
	}
	return captureStopResult{
		Items:     s.items,
		Bytes:     s.bytes,
		Deduped:   s.dedup,
		Complete:  !s.truncated,
		Truncated: s.truncated,
		EndedAt:   ended.Format(time.RFC3339Nano),
	}, nil
}

func (s *captureStore) Status() captureStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := captureStatus{
		Recording: s.recording,
		SessionID: s.sessionID,
		Items:     s.items,
		Bytes:     s.bytes,
		Deduped:   s.dedup,
		Truncated: s.truncated,
		Counts:    s.counts,
	}
	if !s.startedAt.IsZero() {
		out.StartedAt = s.startedAt.Format(time.RFC3339Nano)
	}
	return out
}

// Clear removes the record directory and resets all state.
func (s *captureStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeItemsLocked()
	dir := s.dir
	s.sessionID = ""
	s.dir = ""
	s.recording = false
	s.startedAt = time.Time{}
	s.endedAt = time.Time{}
	s.stopReason = ""
	s.truncated = false
	s.items = 0
	s.bytes = 0
	s.dedup = 0
	s.counts = captureCounts{}
	s.blobs = map[string]string{}
	s.rels = map[string]string{}
	if dir == "" {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *captureStore) closeItemsLocked() {
	if s.itemsF != nil {
		_ = s.itemsF.Close()
		s.itemsF = nil
	}
}

// writeMetaLocked writes meta.json through a same-directory temp file so a
// reader never sees a half-written document.
func (s *captureStore) writeMetaLocked() error {
	if s.dir == "" {
		return nil
	}
	meta := s.metaLocked()
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, "meta.json.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "meta.json"))
}

func (s *captureStore) metaLocked() captureMeta {
	m := captureMeta{
		SchemaVersion: 1,
		RecordID:      s.sessionID,
		SessionID:     s.sessionID,
		Recording:     s.recording,
		StopReason:    s.stopReason,
		Counts:        s.counts,
		Sizes:         captureSizes{RecordBytes: s.bytes},
		Truncated:     s.truncated,
		Limits:        captureLimits{MaxFileBytes: s.maxFile, MaxRecordBytes: s.maxRecord},
	}
	if !s.startedAt.IsZero() {
		m.StartedAt = s.startedAt.Format(time.RFC3339Nano)
	}
	if !s.endedAt.IsZero() {
		v := s.endedAt.Format(time.RFC3339Nano)
		m.EndedAt = &v
	}
	return m
}

// ---------- items ----------

// AppendRow adds a metadata-only item (excluded_type, too_large, failed,
// missing_body) — no body was read, so nothing is persisted.
func (s *captureStore) AppendRow(it captureItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.recording || s.truncated {
		return
	}
	if it.State == "" {
		it.State = captureItemStateFailed
	}
	s.appendLocked(it)
}

// SaveBody persists one decoded body and its manifest row. The body is
// written to a temp file and renamed into place only after the hash is known
// to match; a body over max_file_bytes becomes a too_large row and no blob.
func (s *captureStore) SaveBody(it captureItem, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.recording || s.truncated {
		return
	}

	it.SizeDecoded = int64(len(body))
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	if s.maxFile > 0 && it.SizeDecoded > s.maxFile {
		// Keep the sha1 for display but never the sha256: the backend pulls
		// blobs by sha256, and there is no blob for this row.
		it.State = captureItemStateTooLarge
		it.SHA256 = ""
		it.Filename = ""
		s.appendLocked(it)
		return
	}

	it.SHA256 = digest
	it.State = captureItemStateCaptured

	rel, created := s.relForLocked(it.URL, it.Mime, digest)
	it.File = rel
	it.Filename = path.Base(rel)
	if !created {
		// The bytes are already on disk under this sha256: one canonical file,
		// one row per request, and the backend links the extra readable names.
		s.dedup++
		it.TSSaved = time.Now().UnixMilli()
		s.appendLocked(it)
		return
	}

	if s.maxRecord > 0 && s.bytes+it.SizeDecoded > s.maxRecord {
		s.truncateLocked("quota (record budget)")
		return
	}

	dest := filepath.Join(s.dir, filepath.FromSlash(rel))
	if !captureRelValid(rel) || !captureInsideDir(filepath.Join(s.dir, "files"), dest) {
		s.failItemLocked(it, errors.New("unsafe capture path: "+rel))
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		s.failItemLocked(it, err)
		return
	}
	tmp := filepath.Join(s.dir, "tmp-"+captureRandSuffix())
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		_ = os.Remove(tmp)
		s.handleWriteErrLocked(it, err)
		return
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		s.handleWriteErrLocked(it, err)
		return
	}
	s.blobs[digest] = rel
	s.rels[rel] = digest
	s.bytes += it.SizeDecoded
	it.TSSaved = time.Now().UnixMilli()
	s.appendLocked(it)
}

// relForLocked resolves the readable archive path for one body. A sha256 the
// record already stores keeps its first path (created=false) — the streamer
// deliberately writes exactly one copy per sha256 and lets several manifest
// rows name the same file; the backend turns those extra rows into host-side
// hardlinks so Finder still shows the readable names.
//
// Collisions on the readable name are resolved as the layout contract says:
// a different body behind the same path gets "~<sha256[:12]>", and if that is
// taken too, the full sha256. The result is deterministic per URL+mime+digest
// sequence.
func (s *captureStore) relForLocked(rawURL, mimeType, digest string) (string, bool) {
	if prev, ok := s.blobs[digest]; ok {
		return prev, false
	}
	spec := capturePathSpecFor(rawURL, mimeType)
	rel := spec.rel("")
	if _, taken := s.rels[rel]; !taken {
		return rel, true
	}
	short := digest
	if len(short) > 12 {
		short = short[:12]
	}
	rel = spec.rel("~" + short)
	if _, taken := s.rels[rel]; !taken {
		return rel, true
	}
	return spec.rel("~" + digest), true
}

// handleWriteErrLocked treats a full disk as the quota stop; anything else is
// a per-item failure so a single bad path cannot end the recording.
func (s *captureStore) handleWriteErrLocked(it captureItem, err error) {
	if errors.Is(err, syscall.ENOSPC) {
		s.truncateLocked("quota (no space)")
		return
	}
	s.failItemLocked(it, err)
}

func (s *captureStore) failItemLocked(it captureItem, err error) {
	if !s.warnWrite {
		s.warnWrite = true
		log.Printf("capture: write failed: %v", err)
	}
	it.State = captureItemStateFailed
	it.SHA256 = ""
	it.Filename = ""
	it.File = ""
	s.appendLocked(it)
}

func (s *captureStore) appendLocked(it captureItem) {
	if !s.recording || s.truncated || s.itemsF == nil {
		return
	}
	s.seq++
	if it.ID == "" {
		it.ID = captureItemIDPrefix + strconv.Itoa(s.seq)
	}
	if it.TSFinished == 0 {
		it.TSFinished = time.Now().UnixMilli()
	}
	line, err := json.Marshal(it)
	if err != nil {
		return
	}
	line = append(line, '\n')
	if _, err := s.itemsF.Write(line); err != nil {
		s.truncateLocked("quota (manifest write: " + err.Error() + ")")
		return
	}
	if err := s.itemsF.Sync(); err != nil {
		s.truncateLocked("quota (manifest sync: " + err.Error() + ")")
		return
	}
	s.items++
	s.counts.Requests = s.items
	switch it.State {
	case captureItemStateCaptured:
		s.counts.Captured++
	case captureItemStateTooLarge:
		s.counts.TooLarge++
	case captureItemStateExcludedType:
		s.counts.ExcludedType++
	case captureItemStateMissingBody:
		s.counts.MissingBody++
	default:
		s.counts.Failed++
	}
}

// truncateLocked stops the record from accepting more data while leaving it
// readable — the archive is incomplete, not gone.
func (s *captureStore) truncateLocked(reason string) {
	if s.truncated {
		return
	}
	s.truncated = true
	s.stopReason = "quota"
	log.Printf("capture: truncated session=%s reason=%s items=%d bytes=%d", s.sessionID, reason, s.items, s.bytes)
	if err := s.writeMetaLocked(); err != nil {
		log.Printf("capture: meta write after truncation: %v", err)
	}
}

// ---------- read side ----------

// Manifest is the document the backend polls: live counters (they move with
// every item) plus the rows as they were fsynced, so a crash never hides an
// item the backend may already have pulled.
func (s *captureStore) Manifest() (captureManifest, error) {
	s.mu.Lock()
	dir := s.dir
	meta := s.metaLocked()
	s.mu.Unlock()
	if dir == "" {
		return captureManifest{}, errCaptureNotActive
	}
	items, err := readCaptureItems(dir)
	if err != nil {
		return captureManifest{}, err
	}
	return captureManifest{Meta: meta, Items: items}, nil
}

// readCaptureManifest rebuilds the whole manifest from disk, as a restart
// would: the on-disk meta.json snapshot plus every fsynced item line.
func readCaptureManifest(dir string) (captureManifest, error) {
	var out captureManifest
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out.Meta); err != nil {
		return out, err
	}
	out.Items, err = readCaptureItems(dir)
	return out, err
}

// readCaptureItems parses items.jsonl. A torn trailing append is ignored
// rather than fatal.
func readCaptureItems(dir string) ([]captureItem, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "items.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return []captureItem{}, nil
		}
		return nil, err
	}
	items := []captureItem{}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var it captureItem
		if json.Unmarshal(line, &it) != nil {
			continue // torn trailing append
		}
		items = append(items, it)
	}
	return items, nil
}

// BlobPath resolves a validated sha256 to the readable file the record stores
// it under. Every row for that sha names the same path; the backend pulls by
// sha256 and writes its own path from the URL, so this is the only place the
// streamer has to map hash → file.
func (s *captureStore) BlobPath(sha string) (string, error) {
	if !captureSHA256Re.MatchString(sha) {
		return "", errCaptureBadSHA256
	}
	s.mu.Lock()
	dir, rel := s.dir, s.blobs[sha]
	s.mu.Unlock()
	if dir == "" {
		return "", errCaptureNotActive
	}
	if rel == "" {
		return "", errCaptureBlobMissing
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if !captureRelValid(rel) || !captureInsideDir(filepath.Join(dir, "files"), p) {
		return "", errCaptureBlobMissing
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return "", errCaptureBlobMissing
	}
	return p, nil
}

// ---------- helpers ----------

func captureRandSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// ---------- readable path derivation ----------

const (
	capturePathMaxHost     = 80
	capturePathMaxSegment  = 80
	capturePathMaxSegments = 8
	capturePathMaxQuery    = 40
	// capturePathMaxName bounds the composed file name (query + version
	// suffix + extension included) so a hostile URL cannot produce a name the
	// filesystem rejects; the stem is truncated to fit.
	capturePathMaxName = 160
)

// captureItemExtRe: an extension is kept only when it looks like one.
var captureItemExtRe = regexp.MustCompile(`^\.[A-Za-z0-9]{1,5}$`)

// capturePathSpec is the readable location of one body, before versioning:
// files/<host>/<dirs…>/<stem><query><version><ext>.
type capturePathSpec struct {
	Host  string
	Dirs  []string
	Stem  string
	Ext   string
	Query string
}

// rel renders the record-relative path (forward slashes, always "files/…").
func (p capturePathSpec) rel(version string) string {
	var b strings.Builder
	b.WriteString("files/")
	b.WriteString(p.Host)
	for _, d := range p.Dirs {
		b.WriteString("/")
		b.WriteString(d)
	}
	b.WriteString("/")
	b.WriteString(p.name(version))
	return b.String()
}

// name builds the final segment: <stem>__<query><version><ext>. The version
// suffix ("" or "~<hex>") sits immediately before the extension, as the layout
// contract specifies.
func (p capturePathSpec) name(version string) string {
	tail := ""
	if p.Query != "" {
		tail += "__" + p.Query
	}
	tail += version + p.Ext
	stem := p.Stem
	if max := capturePathMaxName - len(tail); len(stem) > max {
		stem = stem[:max]
	}
	if stem == "" {
		stem = "index"
	}
	return stem + tail
}

// capturePathSpecFor derives the spec from the URL and mime type. Pure and
// deterministic: identical input always yields the identical spec, which is
// what makes the on-disk name stable across runs.
func capturePathSpecFor(rawURL, mimeType string) capturePathSpec {
	spec := capturePathSpec{Host: capturePathHost(rawURL)}
	var segs []string
	if u, err := url.Parse(rawURL); err == nil {
		if u.RawQuery != "" {
			spec.Query = captureQueryToken(u.RawQuery)
		}
		if p := strings.TrimPrefix(u.Path, "/"); p != "" {
			segs = strings.Split(p, "/")
		}
	}
	if len(segs) == 0 {
		segs = []string{""} // no path, or a path ending in "/"
	}
	if len(segs) > capturePathMaxSegments {
		// Deeper paths keep the first 7 components and the last one; the last
		// becomes the file name, so at most 7 directories survive.
		trimmed := make([]string, 0, capturePathMaxSegments)
		trimmed = append(trimmed, segs[:capturePathMaxSegments-1]...)
		trimmed = append(trimmed, segs[len(segs)-1])
		segs = trimmed
	}
	last := segs[len(segs)-1]
	for _, s := range segs[:len(segs)-1] {
		if atom := captureAtom(s, capturePathMaxSegment); atom != "" {
			spec.Dirs = append(spec.Dirs, atom)
		} else {
			spec.Dirs = append(spec.Dirs, "_")
		}
	}

	stem, ext := captureSplitExt(captureAtom(last, capturePathMaxSegment))
	if ext == "" {
		ext = captureExtFromMime(mimeType)
	}
	spec.Stem = stem
	spec.Ext = ext
	return spec
}

// capturePathHost: lowercased hostname, non-default port appended as "__<port>",
// sanitized to [a-z0-9._-] and capped; no host (or an unusable one) → "local".
// The port suffix is reserved before the clamp, so a very long hostname loses
// its tail rather than the port that distinguishes the origin.
func capturePathHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "local"
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "local"
	}
	suffix := ""
	if p := u.Port(); p != "" && p != captureDefaultPort(u.Scheme) {
		suffix = "__" + p
	}
	host = captureAtom(host, capturePathMaxHost-len(suffix))
	if host == "" {
		return "local"
	}
	return host + suffix
}

func captureDefaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// captureAtom sanitizes one path component: only [A-Za-z0-9._-] survive,
// everything else becomes "_", runs of dots collapse to one dot (so ".." can
// never be assembled), the result is capped at maxLen bytes, and an atom that
// is empty or a lone dot is reported as "" for the caller to replace.
func captureAtom(s string, maxLen int) string {
	var b strings.Builder
	prevDot := false
	for _, r := range s {
		if r == '.' {
			if prevDot {
				continue
			}
			prevDot = true
			b.WriteRune(r)
		} else {
			prevDot = false
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		if b.Len() >= maxLen {
			break
		}
	}
	out := b.String()
	if out == "." {
		return ""
	}
	return out
}

// captureSplitExt splits a file name into stem and a kept extension; an
// extension outside [A-Za-z0-9]{1,5} is left on the stem so the mime-derived
// one can be appended instead.
func captureSplitExt(name string) (string, string) {
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		if cand := name[i:]; captureItemExtRe.MatchString(cand) {
			return name[:i], strings.ToLower(cand)
		}
	}
	return name, ""
}

// captureExtFromMime is the layout contract's table, verbatim: anything not
// listed gets no extension at all.
var captureMimeExt = map[string]string{
	"text/html":                ".html",
	"application/xhtml+xml":    ".html",
	"text/css":                 ".css",
	"application/json":         ".json",
	"text/json":                ".json",
	"text/xml":                 ".xml",
	"application/xml":          ".xml",
	"image/svg+xml":            ".svg",
	"image/png":                ".png",
	"image/jpeg":               ".jpg",
	"image/gif":                ".gif",
	"image/webp":               ".webp",
	"image/avif":               ".avif",
	"image/x-icon":             ".ico",
	"image/vnd.microsoft.icon": ".ico",
	"font/woff":                ".woff",
	"font/woff2":               ".woff2",
	"font/ttf":                 ".ttf",
	"text/plain":               ".txt",
	"application/wasm":         ".wasm",
	"application/pdf":          ".pdf",
	"application/octet-stream": ".bin",
	"application/zip":          ".zip",
}

func captureExtFromMime(mimeType string) string {
	mt := mimeType
	if parsed, _, err := mime.ParseMediaType(mimeType); err == nil {
		mt = parsed
	}
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = mt[:i]
	}
	mt = strings.ToLower(strings.TrimSpace(mt))
	if ext, ok := captureMimeExt[mt]; ok {
		return ext
	}
	switch {
	case strings.Contains(mt, "javascript"), strings.Contains(mt, "ecmascript"):
		return ".js"
	case strings.HasSuffix(mt, "+json"):
		return ".json"
	case strings.HasSuffix(mt, "+xml"):
		return ".xml"
	}
	return ""
}

// captureQueryToken renders a query string as one filename token: runs outside
// [A-Za-z0-9] collapse to "_", the ends are trimmed, and it is capped.
func captureQueryToken(q string) string {
	var b strings.Builder
	dirty := false
	for _, r := range q {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dirty = false
		} else if !dirty {
			b.WriteByte('_')
			dirty = true
		}
		if b.Len() >= capturePathMaxQuery {
			break
		}
	}
	return strings.Trim(b.String(), "_")
}

// captureRelValid is the last gate before bytes hit the filesystem: the rel
// must live under files/, carry forward slashes only, and contain no empty,
// "." or ".." component.
func captureRelValid(rel string) bool {
	if !strings.HasPrefix(rel, "files/") || strings.ContainsAny(rel, `\`) || strings.Contains(rel, "//") {
		return false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// captureInsideDir reports whether target stays inside root after cleaning.
func captureInsideDir(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	return target == root || strings.HasPrefix(target, root+string(os.PathSeparator))
}

// ---------- HTTP surface ----------

// registerCaptureRoutes mounts the capture control surface. Unavailable (404)
// when the store could not be constructed at startup.
func registerCaptureRoutes(mux *http.ServeMux) {
	s := captureRec
	if s == nil {
		return
	}

	mux.HandleFunc("/capture/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SessionID      string `json:"session_id"`
			MaxFileBytes   int64  `json:"max_file_bytes"`
			MaxRecordBytes int64  `json:"max_record_bytes"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, captureMaxStartBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		started, err := s.Start(req.SessionID, req.MaxFileBytes, req.MaxRecordBytes)
		switch {
		case errors.Is(err, errCaptureBadSession):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, errCaptureBusy):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case err != nil:
			http.Error(w, "capture failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{
			"ok": true, "record_id": req.SessionID, "started_at": started.Format(time.RFC3339Nano),
		})
	})

	mux.HandleFunc("/capture/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		res, err := s.Stop()
		if err != nil {
			http.Error(w, "capture failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, res)
	})

	mux.HandleFunc("/capture/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, s.Status())
	})

	mux.HandleFunc("/capture/manifest", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		m, err := s.Manifest()
		if err != nil {
			http.Error(w, "no record", http.StatusNotFound)
			return
		}
		writeJSON(w, m)
	})

	mux.HandleFunc("/capture/blob", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		sha := r.URL.Query().Get("sha256")
		p, err := s.BlobPath(sha)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errCaptureBlobMissing) {
				status = http.StatusNotFound
			}
			http.Error(w, err.Error(), status)
			return
		}
		f, err := os.Open(p)
		if err != nil {
			http.Error(w, "blob not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			http.Error(w, "blob not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", "attachment")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, sha, st.ModTime(), f)
	})

	mux.HandleFunc("/capture/clear", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := s.Clear(); err != nil {
			http.Error(w, "capture failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
}

// captureRowAllowed reports whether a finished request that was denied the
// hash path should still get a metadata row. Streams and media are excluded
// outright — they are neither captured nor reported.
func captureSilentType(typ string) bool {
	switch typ {
	case "Media", "WebSocket", "EventSource":
		return true
	}
	return false
}

// captureRowFor classifies a finished (or failed) request that the hash worker
// did not take. Callers hold netMu.
func captureRowFor(r *netRequest, failed bool) (captureItem, bool) {
	if captureSilentType(r.typ) || !strings.HasPrefix(r.url, "http") {
		return captureItem{}, false
	}
	if failed {
		return captureItemFromNet(r), true
	}
	if !hashableType(r.typ) {
		it := captureItemFromNet(r)
		it.State = captureItemStateExcludedType
		return it, true
	}
	if r.bytes > captureFetchLimit() {
		it := captureItemFromNet(r)
		it.State = captureItemStateTooLarge
		return it, true
	}
	return captureItem{}, false
}
