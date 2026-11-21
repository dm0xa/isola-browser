package capture

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// metaFile is the host-side meta.json. Items are stored typed (not as opaque
// maps) so ingestion can look up a sha256 without re-decoding arbitrary JSON.
type metaFile struct {
	SchemaVersion int              `json:"schema_version"`
	ArchiveID     string           `json:"archive_id"`
	SessionID     string           `json:"session_id"`
	TargetURL     string           `json:"target_url"`
	CreatedAt     string           `json:"created_at"`
	EndedAt       string           `json:"ended_at"`
	Complete      bool             `json:"complete"`
	StopReason    string           `json:"stop_reason"`
	KeySHA256     string           `json:"key_sha256"`
	Counts        map[string]int64 `json:"counts"`
	Sizes         map[string]int64 `json:"sizes"`
	Items         []Item           `json:"items"`
}

// Item mirrors one streamer manifest item. filename and file are always
// recomputed on the host; the streamer-supplied values are never trusted.
type Item struct {
	ID                string `json:"id"`
	URL               string `json:"url"`
	FinalURL          string `json:"final_url"`
	Method            string `json:"method"`
	Status            int    `json:"status"`
	Mime              string `json:"mime"`
	Type              string `json:"type"`
	SizeWire          int64  `json:"size_wire"`
	SizeDecoded       int64  `json:"size_decoded"`
	SHA256            string `json:"sha256"`
	SHA1              string `json:"sha1"`
	Filename          string `json:"filename"`
	File              string `json:"file"`
	Tab               int    `json:"tab"`
	OriginHost        string `json:"origin_host"`
	TSRequested       int64  `json:"ts_requested"`
	TSFinished        int64  `json:"ts_finished"`
	TSSaved           int64  `json:"ts_saved"`
	FromDiskCache     bool   `json:"from_disk_cache"`
	FromServiceWorker bool   `json:"from_service_worker"`
	State             string `json:"state"`
}

const metaName = "meta.json"

func archiveDir(root, archiveID string) string {
	return filepath.Join(root, archiveID)
}

func metaPath(dir string) string { return filepath.Join(dir, metaName) }

func blobDir(dir, sha string) string {
	return filepath.Join(dir, "blobs", sha[:2])
}

func blobPath(dir, sha string) string {
	return filepath.Join(blobDir(dir, sha), sha)
}

func tmpDir(dir string) string { return filepath.Join(dir, "blobs", "tmp") }

// writeMeta atomically rewrites meta.json: a tmp file in the same directory,
// fsync, then rename. A crash leaves either the old file or the new one.
func writeMeta(dir string, meta *metaFile) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, metaName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, metaPath(dir))
}

func readMeta(dir string) (*metaFile, error) {
	data, err := os.ReadFile(metaPath(dir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrArchiveNotFound
		}
		return nil, err
	}
	var meta metaFile
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("capture: parse %s: %w", metaPath(dir), err)
	}
	return &meta, nil
}

// writeTombstone marks an archive whose finalize could not complete.
func writeTombstone(dir, reason string) {
	payload, _ := json.Marshal(map[string]string{
		"reason": reason,
		"at":     time.Now().UTC().Format(time.RFC3339),
	})
	path := filepath.Join(dir, "tombstone")
	if err := os.WriteFile(path, payload, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Best effort: the meta's complete=false is the authoritative signal.
		_ = err
	}
}

func hasTombstone(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "tombstone"))
	return err == nil
}

// randomHex returns n random bytes as hex, used for archive keys and tmp names.
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// keyHash is the stored form of an archive key: sha256 hex. The plaintext key
// lives only in process memory and in the caller's localStorage.
func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func parseMetaTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// normalizeMime strips parameters and lowercases a mime type.
func normalizeMime(mime string) string {
	if idx := strings.IndexByte(mime, ';'); idx >= 0 {
		mime = mime[:idx]
	}
	return strings.ToLower(strings.TrimSpace(mime))
}

// sortSummaries keeps the listing stable: newest archive first.
func sortSummaries(list []Summary) {
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
}
