package main

// Session recording: tee's the live fMP4 output to disk alongside the
// WebSocket fan-out. The recorder is a pure consumer of the encoder's
// fragment stream — it never alters pacing or fan-out — and is controlled
// over the streamer's local HTTP surface (POST /record/start|stop) by the
// backend, which owns policy and persistence.
//
// Crash safety: ffmpeg emits fragmented MP4 (ftyp+moov init, then moof
// fragments), so every file written here is playable while being written
// and after an abrupt teardown. A new encoder run (ABR rung change, resize,
// keyframe restart) starts a fresh file on its init segment, because each
// run produces a new init segment; the backend stitches the file list into
// the recording row.

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// recordDir is the directory recordings are written to. Empty (default)
// disables the feature entirely: the record endpoints return 503.
var recordDir = os.Getenv("ISOLA_RECORD_DIR")

// recordingActive keeps the capture producer running even when no viewer is
// attached — a recording of an unwatched session must still capture frames.
var recordingActive atomic.Bool

type sessionRecorder struct {
	mu      sync.Mutex
	active  bool
	f       *os.File
	fName   string
	size    int64
	files   []string
	started time.Time
	warnRaw bool // logged once per raw-h264 run
}

var rec sessionRecorder

// recordSegment tees one raw fMP4 segment (init or fragment) to disk.
// Called from the run's output pump; never blocks the fan-out beyond the
// file write itself. Rotation: an init segment always starts a new file.
func recordSegment(seg []byte, isInit bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.active {
		return
	}
	if isInit || rec.f == nil {
		if rec.f != nil {
			rec.f.Close()
			rec.f = nil
		}
		name := fmt.Sprintf("rec-%d.mp4", time.Now().UnixNano())
		f, err := os.Create(filepath.Join(recordDir, name))
		if err != nil {
			if !rec.warnRaw {
				log.Printf("record: create %s: %v", name, err)
			}
			return
		}
		rec.f = f
		rec.fName = name
		// rec.size accumulates across rotations — the stop manifest reports
		// the total recording size, not the last file's.
		rec.files = append(rec.files, name)
	}
	if rec.f == nil {
		return
	}
	n, err := rec.f.Write(seg)
	rec.size += int64(n)
	if err != nil {
		log.Printf("record: write %s: %v", rec.fName, err)
		rec.f.Close()
		rec.f = nil
	}
}

// recordRawRun notes that the active run produces raw Annex-B (webcodecs
// viewer mode) — nothing recordable without a muxer; logged once.
func recordRawRun() {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.active && !rec.warnRaw {
		rec.warnRaw = true
		log.Printf("record: skipped (raw h264)")
	}
}

// recordStart activates recording. Fails when already active.
func recordStart() error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.active {
		return fmt.Errorf("recording already active")
	}
	rec.active = true
	rec.files = nil
	rec.f = nil
	rec.size = 0
	rec.started = time.Now()
	rec.warnRaw = false
	recordingActive.Store(true)
	rec.mu.Unlock()
	// Gate push outside the lock: it writes to the ctrl socket (bounded by a
	// write deadline) and must never stall the output pump waiting on rec.mu.
	defer rec.mu.Lock()
	syncProducerCapture(false)
	log.Printf("record: start dir=%s", recordDir)
	return nil
}

// recordStop closes the current file and returns the recording manifest.
func recordStop() (files []string, sizeBytes int64, duration time.Duration, err error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if !rec.active {
		return nil, 0, 0, fmt.Errorf("no recording active")
	}
	if rec.f != nil {
		rec.f.Close()
		rec.f = nil
	}
	rec.active = false
	recordingActive.Store(false)
	rec.mu.Unlock()
	defer rec.mu.Lock()
	syncProducerCapture(false)
	out := append([]string{}, rec.files...)
	log.Printf("record: stopped (%d files, %d bytes)", len(out), rec.size)
	return out, rec.size, time.Since(rec.started), nil
}

// recordCleanup deletes the named files (after the backend has pulled them).
func recordCleanup(names []string) {
	for _, name := range names {
		if !safeRecordName(name) {
			continue
		}
		if err := os.Remove(filepath.Join(recordDir, name)); err != nil && !os.IsNotExist(err) {
			log.Printf("record: cleanup %s: %v", name, err)
		}
	}
}

// safeRecordName restricts file access to names the recorder itself emits.
func safeRecordName(name string) bool {
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return false
	}
	return strings.HasPrefix(name, "rec-") && strings.HasSuffix(name, ".mp4")
}

// recordFile opens a recorded file for streaming to the backend.
func recordFile(name string) (*os.File, int64, error) {
	if !safeRecordName(name) {
		return nil, 0, fmt.Errorf("invalid file name")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	f, err := os.Open(filepath.Join(recordDir, name))
	if err != nil {
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// registerRecordRoutes mounts the recording control surface. Disabled
// (503) unless ISOLA_RECORD_DIR is set.
func registerRecordRoutes(mux *http.ServeMux) {
	if recordDir == "" {
		return
	}
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		log.Printf("record: %s: %v", recordDir, err)
		return
	}

	mux.HandleFunc("/record/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		if err := recordStart(); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})

	mux.HandleFunc("/record/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		files, size, dur, err := recordStop()
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		if files == nil {
			files = []string{}
		}
		writeJSON(w, map[string]any{
			"files": files, "size_bytes": size, "duration_sec": int(dur.Seconds()),
		})
	})

	mux.HandleFunc("/record/file", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("name")
		f, size, err := recordFile(name)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		http.ServeContent(w, r, name, time.Now(), f)
	})

	mux.HandleFunc("/record/cleanup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Names []string `json:"names"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		recordCleanup(req.Names)
		writeJSON(w, map[string]any{"ok": true})
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
