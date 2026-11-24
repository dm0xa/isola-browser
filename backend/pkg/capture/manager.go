package capture

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	pollInterval   = 10 * time.Second
	pollTimeout    = 20 * time.Second
	gcInterval     = time.Hour
	manifestSchema = 1
)

// recording is one live session's in-memory ingestion state. meta.json on disk
// is the durable record; this struct is what the poller appends to.
type recording struct {
	sessionID string
	targetURL string
	addr      string
	dir       string
	key       string
	keySHA256 string
	startedAt time.Time

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	mu         sync.Mutex
	seen       map[string]bool   // streamer item id -> ingested (captured or not)
	paths      map[string]string // files/<rel> -> sha256 stored there
	files      map[string]string // sha256 -> first archive-relative rel holding it
	items      []Item
	counts     map[string]int64
	bytes      int64
	deduped    int64
	captured   int
	truncated  bool
	dirty      bool
	stopped    bool
	complete   bool
	endedAt    time.Time
	stopReason string
}

// Manager owns the archive directory and every live puller.
type Manager struct {
	cfg       Config
	log       zerolog.Logger
	pollEvery time.Duration

	mu       sync.Mutex
	sessions map[string]*recording
	starting map[string]bool
	closed   bool

	closedCh  chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewManager builds the manager. With capture disabled it touches no files and
// starts no goroutines, so a disabled deployment pays nothing.
func NewManager(cfg Config, log zerolog.Logger) *Manager {
	cfg = normalizeConfig(cfg)
	m := &Manager{
		cfg:       cfg,
		log:       log,
		pollEvery: pollInterval,
		sessions:  make(map[string]*recording),
		starting:  make(map[string]bool),
		closedCh:  make(chan struct{}),
	}
	if !cfg.Enabled {
		return m
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		m.log.Error().Err(err).Str("dir", cfg.Dir).Msg("capture: create archive dir")
		return m
	}
	// Archives from the previous CAS layout become readable before GC can
	// evict them.
	m.Migrate()
	m.gcNow()
	m.wg.Add(1)
	go m.gcLoop()
	return m
}

// Enabled reports whether the operator turned resource capture on.
func (m *Manager) Enabled() bool { return m != nil && m.cfg.Enabled }

// Start begins a recording for one session and launches the puller.
func (m *Manager) Start(ctx context.Context, sessionID, targetURL, streamerAddr string) (StartResult, error) {
	if !m.Enabled() {
		return StartResult{}, ErrDisabled
	}
	if !validArchiveID(sessionID) {
		return StartResult{}, ErrInvalidArchiveID
	}
	streamerAddr = strings.TrimSpace(streamerAddr)
	if streamerAddr == "" {
		return StartResult{}, errors.New("capture: empty streamer address")
	}

	// Idempotent retry first: a second start on a live recording must hand the
	// same archive back. Checking the archive directory before this branch made
	// every retry fail (the first start creates it), so a client retry saw 502.
	m.mu.Lock()
	if rec, ok := m.sessions[sessionID]; ok {
		res := StartResult{ArchiveID: sessionID, ArchiveKey: rec.key, StartedAt: rec.startedAt}
		m.mu.Unlock()
		return res, nil
	}
	m.mu.Unlock()

	dir := archiveDir(m.cfg.Dir, sessionID)
	if _, err := os.Stat(dir); err == nil {
		// An archive for this id already exists on disk but this process has no
		// recorder for it (a restart happened mid-session). Refuse instead of
		// overwriting evidence the operator may still need.
		return StartResult{}, ErrArchiveExists
	} else if !errors.Is(err, fs.ErrNotExist) {
		return StartResult{}, err
	}

	m.mu.Lock()
	if rec, ok := m.sessions[sessionID]; ok {
		// A concurrent start won the race while we stat'ed the directory.
		res := StartResult{ArchiveID: sessionID, ArchiveKey: rec.key, StartedAt: rec.startedAt}
		m.mu.Unlock()
		return res, nil
	}
	if m.starting[sessionID] {
		m.mu.Unlock()
		return StartResult{}, ErrAlreadyRecording
	}
	m.starting[sessionID] = true
	m.mu.Unlock()

	finish := func() {
		m.mu.Lock()
		delete(m.starting, sessionID)
		m.mu.Unlock()
	}

	client := newStreamerClient(streamerAddr)
	startResp, err := client.start(ctx, sessionID, m.cfg.MaxFileBytes, m.cfg.MaxRecordBytes)
	if err != nil {
		finish()
		return StartResult{}, err
	}
	startedAt := parseMetaTime(startResp.StartedAt)
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}

	key, err := randomHex(32)
	if err != nil {
		finish()
		abortStreamer(client)
		return StartResult{}, err
	}

	if err := os.MkdirAll(tmpDir(dir), 0o700); err != nil {
		finish()
		abortStreamer(client)
		return StartResult{}, err
	}

	rctx, cancel := context.WithCancel(context.Background())
	rec := &recording{
		sessionID: sessionID,
		targetURL: targetURL,
		addr:      streamerAddr,
		dir:       dir,
		key:       key,
		keySHA256: keyHash(key),
		startedAt: startedAt,
		ctx:       rctx,
		cancel:    cancel,
		done:      make(chan struct{}),
		seen:      make(map[string]bool),
		paths:     make(map[string]string),
		files:     make(map[string]string),
		items:     make([]Item, 0),
		counts:    make(map[string]int64),
	}
	rec.mu.Lock()
	err = m.writeMetaLocked(rec)
	rec.mu.Unlock()
	if err != nil {
		cancel()
		finish()
		abortStreamer(client)
		return StartResult{}, err
	}
	if err := writeIndex(dir, rec.items); err != nil {
		m.log.Debug().Err(err).Str("session_id", sessionID).Msg("capture: write index")
	}

	m.mu.Lock()
	m.sessions[sessionID] = rec
	m.mu.Unlock()
	finish()

	go m.runPoller(rec)

	return StartResult{ArchiveID: sessionID, ArchiveKey: key, StartedAt: startedAt}, nil
}

// abortStreamer stops a capture that failed to materialize on the host.
func abortStreamer(client *streamerClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = client.stop(ctx)
}

// Stop ends the recording, performs the final pull and completes the archive.
func (m *Manager) Stop(ctx context.Context, sessionID string) (StopResult, error) {
	if !m.Enabled() {
		return StopResult{}, ErrDisabled
	}
	if !validArchiveID(sessionID) {
		return StopResult{}, ErrInvalidArchiveID
	}

	m.mu.Lock()
	rec := m.sessions[sessionID]
	m.mu.Unlock()

	if rec == nil {
		// Idempotent: an already-finalized archive answers with its stored
		// totals instead of pretending nothing was recorded.
		meta, err := readMeta(archiveDir(m.cfg.Dir, sessionID))
		if err != nil {
			return StopResult{}, ErrNotRecording
		}
		return StopResult{
			Items:    len(meta.Items),
			Bytes:    meta.Sizes["record_bytes"],
			Deduped:  meta.Counts["deduped"],
			Complete: meta.Complete,
		}, nil
	}

	m.stopPoller(rec)

	var stopErr error
	var stopResp streamerStop
	client := newStreamerClient(rec.addr)
	if resp, err := client.stop(ctx); err != nil {
		stopErr = err
	} else {
		stopResp = resp
	}

	ingestErr := m.ingest(ctx, rec)
	complete := stopErr == nil && ingestErr == nil

	endedAt := time.Now().UTC()
	if t := parseMetaTime(stopResp.EndedAt); !t.IsZero() {
		endedAt = t
	}

	rec.mu.Lock()
	rec.stopped = true
	rec.complete = complete
	rec.endedAt = endedAt
	rec.stopReason = "viewer_stop"
	if !complete {
		writeTombstone(rec.dir, "stop_incomplete")
	}
	if err := m.writeMetaLocked(rec); err != nil {
		m.log.Error().Err(err).Str("session_id", sessionID).Msg("capture: write meta on stop")
	}
	result := StopResult{
		Items:    len(rec.items),
		Bytes:    rec.bytes,
		Deduped:  rec.deduped,
		Complete: complete,
	}
	rec.mu.Unlock()

	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if stopErr != nil {
		return result, fmt.Errorf("capture: stop streamer: %w", stopErr)
	}
	if ingestErr != nil {
		return result, fmt.Errorf("capture: final pull: %w", ingestErr)
	}
	return result, nil
}

// Finalize runs before the session container is stopped: one last pull, then
// complete the archive or leave a tombstone. Idempotent and best-effort.
func (m *Manager) Finalize(ctx context.Context, sessionID string) {
	if !m.Enabled() || !validArchiveID(sessionID) {
		return
	}
	dir := archiveDir(m.cfg.Dir, sessionID)

	m.mu.Lock()
	rec := m.sessions[sessionID]
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return
	}

	if rec == nil {
		// Archive exists but this process never recorded it (restart, or an
		// earlier run left it incomplete). Mark it incomplete once.
		meta, err := readMeta(dir)
		if err != nil || meta.Complete || hasTombstone(dir) {
			return
		}
		meta.Complete = false
		meta.StopReason = "session_end"
		meta.EndedAt = time.Now().UTC().Format(time.RFC3339Nano)
		writeTombstone(dir, "finalize_unreachable")
		if err := writeMeta(dir, meta); err != nil {
			m.log.Warn().Err(err).Str("session_id", sessionID).Msg("capture: finalize meta")
		}
		return
	}

	m.stopPoller(rec)
	ingestErr := m.ingest(ctx, rec)
	complete := ingestErr == nil

	endedAt := time.Now().UTC()
	if complete {
		// Graceful stop is best-effort: the container is about to die, and the
		// final manifest pull is already what makes the archive complete.
		client := newStreamerClient(rec.addr)
		stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if resp, err := client.stop(stopCtx); err == nil {
			if t := parseMetaTime(resp.EndedAt); !t.IsZero() {
				endedAt = t
			}
		}
		cancel()
	}

	rec.mu.Lock()
	rec.stopped = true
	rec.complete = complete
	rec.endedAt = endedAt
	rec.stopReason = "session_end"
	if !complete {
		writeTombstone(rec.dir, "finalize_unreachable")
		m.log.Warn().Err(ingestErr).Str("session_id", sessionID).Msg("capture: finalize incomplete")
	}
	if err := m.writeMetaLocked(rec); err != nil {
		m.log.Error().Err(err).Str("session_id", sessionID).Msg("capture: finalize meta")
	}
	rec.mu.Unlock()

	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()
}

// Status returns the live recording state, falling back to the archive on disk.
func (m *Manager) Status(ctx context.Context, sessionID string) (Status, error) {
	if !m.Enabled() {
		return Status{}, ErrDisabled
	}
	if !validArchiveID(sessionID) {
		return Status{}, ErrInvalidArchiveID
	}

	m.mu.Lock()
	rec := m.sessions[sessionID]
	m.mu.Unlock()

	if rec == nil {
		meta, err := readMeta(archiveDir(m.cfg.Dir, sessionID))
		if err != nil {
			if errors.Is(err, ErrArchiveNotFound) {
				// Never recorded: a status document, not an error. A viewer can
				// poll before pressing Record and must see recording:false rather
				// than a 404 it cannot tell apart from a missing archive.
				return Status{SessionID: sessionID, Recording: false}, nil
			}
			return Status{}, err
		}
		startedAt := parseMetaTime(meta.CreatedAt)
		switch meta.StopReason {
		case "quota":
			return Status{
				SessionID: sessionID,
				Items:     len(meta.Items),
				Bytes:     meta.Sizes["record_bytes"],
				Deduped:   meta.Counts["deduped"],
				Truncated: true,
				StartedAt: startedAt,
			}, nil
		}
		return Status{
			SessionID: sessionID,
			Items:     len(meta.Items),
			Bytes:     meta.Sizes["record_bytes"],
			Deduped:   meta.Counts["deduped"],
			StartedAt: startedAt,
		}, nil
	}

	rec.mu.Lock()
	st := Status{
		Recording:  true,
		SessionID:  sessionID,
		Items:      len(rec.items),
		Bytes:      rec.bytes,
		Deduped:    rec.deduped,
		Truncated:  rec.truncated,
		StartedAt:  rec.startedAt,
		ArchiveKey: rec.key,
	}
	addr := rec.addr
	rec.mu.Unlock()

	if sr, err := newStreamerClient(addr).status(ctx); err == nil {
		st.Items = sr.Items
		st.Bytes = sr.Bytes
		st.Deduped = sr.Deduped
		st.Truncated = st.Truncated || sr.Truncated
		if t := parseMetaTime(sr.StartedAt); !t.IsZero() {
			st.StartedAt = t
		}
	}
	return st, nil
}

// List returns every archive on disk, newest first.
// Quota returns the host-wide archive budget and retention window so an API
// caller can show usage without reading the config or the logs. A zero quota
// means unlimited; zero retention days means archives are kept indefinitely.
func (m *Manager) Quota() (totalBytes int64, retentionDays int) {
	if m == nil {
		return 0, 0
	}
	if m.cfg.TotalQuotaBytes > 0 {
		totalBytes = m.cfg.TotalQuotaBytes
	}
	if m.cfg.Retention > 0 {
		retentionDays = int(m.cfg.Retention.Hours() / 24)
	}
	return totalBytes, retentionDays
}

func (m *Manager) List() ([]Summary, error) {
	if !m.Enabled() {
		return nil, ErrDisabled
	}
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validArchiveID(entry.Name()) {
			continue
		}
		meta, err := readMeta(archiveDir(m.cfg.Dir, entry.Name()))
		if err != nil {
			continue
		}
		out = append(out, Summary{
			ArchiveID: meta.ArchiveID,
			SessionID: meta.SessionID,
			TargetURL: meta.TargetURL,
			CreatedAt: parseMetaTime(meta.CreatedAt),
			EndedAt:   parseMetaTime(meta.EndedAt),
			Complete:  meta.Complete,
			Items:     len(meta.Items),
			Bytes:     meta.Sizes["record_bytes"],
		})
	}
	sortSummaries(out)
	return out, nil
}

// Get returns the parsed meta.json for one archive.
func (m *Manager) Get(archiveID string) (Archive, error) {
	if !m.Enabled() {
		return Archive{}, ErrDisabled
	}
	if !validArchiveID(archiveID) {
		return Archive{}, ErrInvalidArchiveID
	}
	raw, err := os.ReadFile(metaPath(archiveDir(m.cfg.Dir, archiveID)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Archive{}, ErrArchiveNotFound
		}
		return Archive{}, err
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return Archive{}, fmt.Errorf("capture: parse archive %s: %w", archiveID, err)
	}
	return Archive{Meta: meta}, nil
}

// Blob locates one archived body and returns its path plus metadata for the
// response headers.
func (m *Manager) Blob(archiveID, sha string) (path, mime, filename string, err error) {
	if !m.Enabled() {
		return "", "", "", ErrDisabled
	}
	if !validArchiveID(archiveID) {
		return "", "", "", ErrInvalidArchiveID
	}
	if !validSHA256(sha) {
		return "", "", "", ErrInvalidHash
	}
	dir := archiveDir(m.cfg.Dir, archiveID)
	meta, err := readMeta(dir)
	if err != nil {
		return "", "", "", err
	}
	var item *Item
	for i := range meta.Items {
		if meta.Items[i].SHA256 == sha && meta.Items[i].State == "captured" {
			item = &meta.Items[i]
			break
		}
	}
	if item == nil {
		return "", "", "", ErrBlobNotFound
	}
	p := ""
	if item.File != "" {
		if clean, relErr := validFileRel(item.File); relErr == nil {
			p = filepath.Join(dir, filepath.FromSlash(clean))
		}
	}
	if p == "" || !fileExists(p) {
		// Archives written by the previous CAS layout keep their bytes here.
		p = blobPath(dir, sha)
	}
	if _, statErr := os.Stat(p); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return "", "", "", ErrBlobNotFound
		}
		return "", "", "", statErr
	}
	mime = item.Mime
	if !safeContentType(mime) {
		mime = "application/octet-stream"
	}
	filename = item.Filename
	if filename == "" {
		filename = sha
	}
	return p, mime, filename, nil
}

// File locates one readable archive file and returns its path plus metadata for
// the response headers. Only paths under files/ that the archive itself
// recorded are served.
func (m *Manager) File(archiveID, rel string) (filePath, mime, filename string, err error) {
	if !m.Enabled() {
		return "", "", "", ErrDisabled
	}
	if !validArchiveID(archiveID) {
		return "", "", "", ErrInvalidArchiveID
	}
	// Accept both "example.com/s.js" and the full "files/example.com/s.js".
	rel = strings.TrimPrefix(rel, "files/")
	rel = "files/" + rel
	clean, err := validFileRel(rel)
	if err != nil {
		return "", "", "", err
	}
	dir := archiveDir(m.cfg.Dir, archiveID)
	meta, err := readMeta(dir)
	if err != nil {
		return "", "", "", err
	}
	var item *Item
	for i := range meta.Items {
		if meta.Items[i].File == clean && meta.Items[i].State == "captured" {
			item = &meta.Items[i]
			break
		}
	}
	if item == nil {
		return "", "", "", ErrBlobNotFound
	}
	p := filepath.Join(dir, filepath.FromSlash(clean))
	if _, statErr := os.Stat(p); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return "", "", "", ErrBlobNotFound
		}
		return "", "", "", statErr
	}
	mime = item.Mime
	if !safeContentType(mime) {
		mime = "application/octet-stream"
	}
	return p, mime, path.Base(clean), nil
}

// CheckKey constant-time compares sha256(key) against the stored digest.
func (m *Manager) CheckKey(archiveID, key string) bool {
	if !m.Enabled() || !validArchiveID(archiveID) || key == "" {
		return false
	}
	meta, err := readMeta(archiveDir(m.cfg.Dir, archiveID))
	if err != nil || meta.KeySHA256 == "" {
		return false
	}
	sum := sha256.Sum256([]byte(key))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(meta.KeySHA256)) == 1
}

// Zip streams meta.json, index.txt and every readable file. Nothing is buffered.
func (m *Manager) Zip(w io.Writer, archiveID string) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	if !validArchiveID(archiveID) {
		return ErrInvalidArchiveID
	}
	dir := archiveDir(m.cfg.Dir, archiveID)
	meta, err := readMeta(dir)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(metaPath(dir))
	if err != nil {
		return err
	}
	indexRaw, err := os.ReadFile(filepath.Join(dir, indexName))
	if err != nil {
		indexRaw = renderIndex(meta.Items)
	}

	zw := zip.NewWriter(w)
	if err := zipBytes(zw, metaName, 0o600, raw); err != nil {
		return err
	}
	if err := zipBytes(zw, indexName, 0o644, indexRaw); err != nil {
		return err
	}

	seen := make(map[string]bool, len(meta.Items))
	for _, it := range meta.Items {
		name := ""
		if clean, relErr := validFileRel(it.File); relErr == nil {
			name = clean
		} else if it.SHA256 != "" {
			// Archives written by the previous CAS layout.
			name = "blobs/" + it.SHA256[:2] + "/" + it.SHA256
		} else {
			continue
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fh.SetMode(0o644)
		if info, statErr := f.Stat(); statErr == nil {
			fh.SetModTime(info.ModTime())
		}
		ww, err := zw.CreateHeader(fh)
		if err != nil {
			f.Close()
			return err
		}
		if _, err := io.Copy(ww, f); err != nil {
			f.Close()
			return err
		}
		f.Close()
	}
	return zw.Close()
}

func zipBytes(zw *zip.Writer, name string, mode os.FileMode, data []byte) error {
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(mode)
	hdr.SetModTime(time.Now())
	ww, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	_, err = ww.Write(data)
	return err
}

// Delete removes one archive directory.
func (m *Manager) Delete(archiveID string) error {
	if !m.Enabled() {
		return ErrDisabled
	}
	if !validArchiveID(archiveID) {
		return ErrInvalidArchiveID
	}
	dir := archiveDir(m.cfg.Dir, archiveID)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrArchiveNotFound
		}
		return err
	}
	return os.RemoveAll(dir)
}

// Close stops every poller and the GC loop. Idempotent.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		recs := make([]*recording, 0, len(m.sessions))
		for _, rec := range m.sessions {
			recs = append(recs, rec)
		}
		m.mu.Unlock()
		close(m.closedCh)
		for _, rec := range recs {
			rec.cancel()
		}
		for _, rec := range recs {
			<-rec.done
		}
		m.wg.Wait()
	})
}

// ---- poller ----------------------------------------------------------------

func (m *Manager) runPoller(rec *recording) {
	defer close(rec.done)
	m.pollOnce(rec)
	ticker := time.NewTicker(m.pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.closedCh:
			return
		case <-rec.ctx.Done():
			return
		case <-ticker.C:
			m.pollOnce(rec)
		}
	}
}

func (m *Manager) pollOnce(rec *recording) {
	ctx, cancel := context.WithTimeout(rec.ctx, pollTimeout)
	defer cancel()
	if err := m.ingest(ctx, rec); err != nil && !errors.Is(err, context.Canceled) {
		m.log.Debug().Err(err).Str("session_id", rec.sessionID).Msg("capture: poll")
	}
}

// stopPoller cancels the poller and waits for it to leave the ingest path.
func (m *Manager) stopPoller(rec *recording) {
	rec.cancel()
	<-rec.done
}

// ingesting marks a blob the streamer reported as captured but that we cannot
// store: too large for the configured per-file limit.
var errBlobTooLarge = errors.New("blob exceeds max file size")

// ingest pulls the manifest and stores every newly captured item. Idempotent:
// items already seen are skipped, and blobs are deduped by sha256.
func (m *Manager) ingest(ctx context.Context, rec *recording) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	client := newStreamerClient(rec.addr)
	manifest, err := client.manifest(ctx)
	if err != nil {
		return err
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.stopped {
		return nil
	}

	changed := false
	for key, value := range manifest.Meta.Counts {
		rec.counts[key] = value
	}
	if manifest.Meta.Truncated {
		rec.truncated = true
	}

	for _, raw := range manifest.Items {
		if rec.seen[raw.ID] {
			continue
		}
		item := itemFromStreamer(raw)
		baseRel, relErr := deriveRel(raw.URL, raw.FinalURL, raw.Mime)
		if relErr == nil {
			item.File = baseRel
			item.Filename = path.Base(baseRel)
		}

		if raw.State != "captured" {
			// Metadata-only outcomes still belong in the review table.
			if item.State == "" {
				item.State = "failed"
			}
			rec.items = append(rec.items, item)
			rec.seen[raw.ID] = true
			rec.counts[item.State]++
			changed = true
			continue
		}
		if !validSHA256(raw.SHA256) {
			m.log.Warn().Str("session_id", rec.sessionID).Str("item_id", raw.ID).
				Msg("capture: item has an invalid sha256")
			item.State = "failed"
			rec.items = append(rec.items, item)
			rec.seen[raw.ID] = true
			rec.counts["failed"]++
			changed = true
			continue
		}
		if relErr != nil {
			m.log.Warn().Str("session_id", rec.sessionID).Str("item_id", raw.ID).
				Str("url", raw.URL).Msg("capture: item path rejected")
			item.State = "failed"
			item.File = ""
			rec.items = append(rec.items, item)
			rec.seen[raw.ID] = true
			rec.counts["failed"]++
			changed = true
			continue
		}

		if raw.SizeDecoded > 0 && raw.SizeDecoded > m.cfg.MaxFileBytes {
			rec.counts["too_large"]++
			item.State = "too_large"
			rec.items = append(rec.items, item)
			rec.seen[raw.ID] = true
			changed = true
			continue
		}
		if raw.SizeDecoded > 0 && rec.bytes+raw.SizeDecoded > m.cfg.MaxRecordBytes {
			rec.truncated = true
			break
		}

		first, stored := rec.files[raw.SHA256]
		if !stored {
			// An archive written by the previous CAS layout still has its bytes.
			if legacy := "blobs/" + raw.SHA256[:2] + "/" + raw.SHA256; fileExists(filepath.Join(rec.dir, filepath.FromSlash(legacy))) {
				first, stored = legacy, true
			}
		}
		rel := rec.resolveRel(baseRel, raw.SHA256)
		item.File = rel
		item.Filename = path.Base(rel)
		if stored {
			// Same bytes at another URL: a readable hardlink, one copy on disk.
			if err := linkFile(rec.dir, first, rel); err != nil {
				m.log.Debug().Err(err).Str("session_id", rec.sessionID).
					Str("file", rel).Msg("capture: link duplicate")
				continue
			}
			rec.deduped++
		} else {
			data, err := client.blob(ctx, raw.SHA256, m.cfg.MaxFileBytes)
			if err != nil {
				if errors.Is(err, errBlobTooLarge) {
					rec.counts["too_large"]++
					item.State = "too_large"
					rec.items = append(rec.items, item)
					rec.seen[raw.ID] = true
					changed = true
					continue
				}
				// Transient: leave the item unseen so the next poll retries.
				m.log.Debug().Err(err).Str("session_id", rec.sessionID).
					Str("sha256", raw.SHA256).Msg("capture: fetch blob")
				continue
			}
			if int64(len(data)) > m.cfg.MaxFileBytes {
				rec.counts["too_large"]++
				item.State = "too_large"
				rec.items = append(rec.items, item)
				rec.seen[raw.ID] = true
				changed = true
				continue
			}
			if rec.bytes+int64(len(data)) > m.cfg.MaxRecordBytes {
				rec.truncated = true
				break
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != raw.SHA256 {
				m.log.Warn().Str("session_id", rec.sessionID).Str("sha256", raw.SHA256).
					Msg("capture: blob hash mismatch")
				rec.counts["failed"]++
				item.State = "failed"
				rec.items = append(rec.items, item)
				rec.seen[raw.ID] = true
				changed = true
				continue
			}
			if err := storeBytes(rec.dir, rel, data); err != nil {
				m.log.Debug().Err(err).Str("session_id", rec.sessionID).
					Str("file", rel).Msg("capture: store file")
				continue
			}
			rec.bytes += int64(len(data))
			first = rel
		}

		rec.paths[rel] = raw.SHA256
		rec.files[raw.SHA256] = first
		rec.captured++
		rec.items = append(rec.items, item)
		rec.seen[raw.ID] = true
		rec.counts["captured"] = int64(rec.captured)
		changed = true
	}

	if changed || rec.dirty {
		if err := m.writeMetaLocked(rec); err != nil {
			rec.dirty = true
			return err
		}
		rec.dirty = false
		if err := writeIndex(rec.dir, rec.items); err != nil {
			m.log.Debug().Err(err).Str("session_id", rec.sessionID).Msg("capture: write index")
		}
	}
	return nil
}

func itemFromStreamer(raw streamerItem) Item {
	state := raw.State
	if state == "captured" {
		// Default stays captured; callers downgrade it on rejection.
		state = "captured"
	}
	return Item{
		ID:                raw.ID,
		URL:               raw.URL,
		FinalURL:          raw.FinalURL,
		Method:            raw.Method,
		Status:            raw.Status,
		Mime:              raw.Mime,
		Type:              raw.Type,
		SizeWire:          raw.SizeWire,
		SizeDecoded:       raw.SizeDecoded,
		SHA256:            raw.SHA256,
		SHA1:              raw.SHA1,
		Tab:               raw.Tab,
		OriginHost:        raw.OriginHost,
		TSRequested:       raw.TSRequested,
		TSFinished:        raw.TSFinished,
		TSSaved:           raw.TSSaved,
		FromDiskCache:     raw.FromDiskCache,
		FromServiceWorker: raw.FromServiceWorker,
		State:             state,
	}
}

// storeBytes stages bytes in blobs/tmp and renames them to the archive-relative
// readable path only after the caller verified the hash.
func storeBytes(dir, rel string, data []byte) error {
	tmp := tmpDir(dir)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(tmp, "blob-*")
	if err != nil {
		return err
	}
	tmpName := f.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	target := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// linkFile gives a second readable name to bytes already stored under srcRel.
// A hardlink keeps one copy on disk; filesystems that refuse it get a copy.
func linkFile(dir, srcRel, dstRel string) error {
	if dstRel == "" || srcRel == "" {
		return ErrInvalidPath
	}
	src := filepath.Join(dir, filepath.FromSlash(srcRel))
	dst := filepath.Join(dir, filepath.FromSlash(dstRel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return nil
	}
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return storeBytes(dir, dstRel, data)
}

func (m *Manager) writeMetaLocked(rec *recording) error {
	counts := make(map[string]int64, len(rec.counts)+4)
	for k, v := range rec.counts {
		counts[k] = v
	}
	counts["captured"] = int64(rec.captured)
	counts["deduped"] = rec.deduped
	counts["requests"] = int64(len(rec.items))
	meta := &metaFile{
		SchemaVersion: manifestSchema,
		ArchiveID:     rec.sessionID,
		SessionID:     rec.sessionID,
		TargetURL:     rec.targetURL,
		CreatedAt:     rec.startedAt.UTC().Format(time.RFC3339Nano),
		Complete:      rec.complete,
		StopReason:    rec.stopReason,
		KeySHA256:     rec.keySHA256,
		Counts:        counts,
		Sizes:         map[string]int64{"record_bytes": rec.bytes},
		Items:         rec.items,
	}
	if !rec.endedAt.IsZero() {
		meta.EndedAt = rec.endedAt.UTC().Format(time.RFC3339Nano)
	}
	return writeMeta(rec.dir, meta)
}

// ---- GC --------------------------------------------------------------------

func (m *Manager) gcLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.closedCh:
			return
		case <-ticker.C:
			m.gcNow()
		}
	}
}

type gcArchive struct {
	id      string
	ended   time.Time
	created time.Time
	bytes   int64
}

// gcNow enforces retention and the total quota (oldest first). Active
// recordings are never evicted; their bytes are excluded from the budget.
func (m *Manager) gcNow() {
	if !m.Enabled() {
		return
	}
	entries, err := os.ReadDir(m.cfg.Dir)
	if err != nil {
		return
	}

	m.mu.Lock()
	active := make(map[string]bool, len(m.sessions))
	for id := range m.sessions {
		active[id] = true
	}
	m.mu.Unlock()

	now := time.Now()
	keep := make([]gcArchive, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !validArchiveID(entry.Name()) {
			continue
		}
		id := entry.Name()
		if active[id] {
			continue
		}
		meta, err := readMeta(archiveDir(m.cfg.Dir, id))
		if err != nil {
			continue
		}
		created := parseMetaTime(meta.CreatedAt)
		ended := parseMetaTime(meta.EndedAt)
		if ended.IsZero() {
			ended = created
		}
		if m.cfg.Retention > 0 && !ended.IsZero() && now.Sub(ended) > m.cfg.Retention {
			m.removeArchive(id, "retention", meta.Sizes["record_bytes"])
			continue
		}
		keep = append(keep, gcArchive{
			id:      id,
			ended:   ended,
			created: created,
			bytes:   meta.Sizes["record_bytes"],
		})
	}

	if m.cfg.TotalQuotaBytes <= 0 {
		return
	}
	total := int64(0)
	for _, a := range keep {
		total += a.bytes
	}
	sort.Slice(keep, func(i, j int) bool {
		if keep[i].ended.Equal(keep[j].ended) {
			return keep[i].created.Before(keep[j].created)
		}
		return keep[i].ended.Before(keep[j].ended)
	})
	for total > m.cfg.TotalQuotaBytes && len(keep) > 0 {
		victim := keep[0]
		keep = keep[1:]
		m.removeArchive(victim.id, "quota", victim.bytes)
		total -= victim.bytes
	}
}

func (m *Manager) removeArchive(id, reason string, bytes int64) {
	if err := os.RemoveAll(archiveDir(m.cfg.Dir, id)); err != nil {
		m.log.Warn().Err(err).Str("archive_id", id).Msg("capture: gc remove")
		return
	}
	m.log.Info().Str("archive_id", id).Str("reason", reason).Int64("bytes", bytes).
		Msg("capture: gc evicted archive")
}

// ---- helpers ---------------------------------------------------------------

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func safeContentType(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return false
	}
	return strings.Contains(value, "/")
}
