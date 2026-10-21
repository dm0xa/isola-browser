package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
)

// Snapshot persistence for single-user deployments: the backend's session
// store and routing cache are in-memory, so every backend restart (redeploy,
// host reboot) wiped all sessions and every open viewer was stuck in a
// reconnect loop against a 401. The sandbox containers themselves keep
// running — only the routing state was lost — so persisting that state to
// disk and restoring it at boot makes viewers reconnect seamlessly.
//
// The snapshot contains control tokens; the file is written 0600 into the
// backend's data volume.

type persistedCacheEntry struct {
	Value  string    `json:"v"`
	Expire time.Time `json:"e,omitempty"` // zero = no expiry
}

type snapshotFile struct {
	Sessions []models.Session               `json:"sessions"`
	Cache    map[string]persistedCacheEntry `json:"cache"`
	SavedAt  time.Time                      `json:"saved_at"`
}

var snapshotMu sync.Mutex // serializes concurrent snapshot writes

// WriteSnapshot atomically persists MemStore sessions and MemCache entries.
func WriteSnapshot(path string, ms *MemStore, c *MemCache) error {
	snapshotMu.Lock()
	defer snapshotMu.Unlock()

	snap := snapshotFile{
		Sessions: ms.SnapshotSessions(),
		Cache:    c.SnapshotEntries(),
		SavedAt:  time.Now(),
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadSnapshot restores sessions (running/provisioning, unexpired) and
// cache entries from a snapshot file. A missing file is not an error.
func LoadSnapshot(path string, ms *MemStore, c *MemCache) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap snapshotFile
	if err := json.Unmarshal(b, &snap); err != nil {
		return err
	}
	now := time.Now()
	restored := 0
	for _, ps := range snap.Sessions {
		if ps.Status != models.SessionStatusRunning && ps.Status != models.SessionStatusProvisioning {
			continue
		}
		if ps.ExpiresAt != nil && now.After(*ps.ExpiresAt) {
			continue
		}
		ms.RestoreSession(&ps)
		restored++
	}
	cacheRestored := 0
	for k, e := range snap.Cache {
		if !e.Expire.IsZero() && now.After(e.Expire) {
			continue
		}
		c.RestoreEntry(k, e.Value, e.Expire)
		cacheRestored++
	}
	log.Info().Int("sessions", restored).Int("cache_keys", cacheRestored).
		Time("snapshot_at", snap.SavedAt).Msg("restored session state from snapshot")
	return nil
}

// StartSnapshotSaver periodically writes the snapshot until ctx is done;
// a final snapshot is written on shutdown.
func StartSnapshotSaver(ctx context.Context, path string, ms *MemStore, c *MemCache, interval time.Duration) {
	// 0700: the directory holds sessions.json, which contains control tokens
	// (VULN-26).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Error().Err(err).Str("dir", filepath.Dir(path)).Msg("snapshot: data dir unavailable, persistence disabled")
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := WriteSnapshot(path, ms, c); err != nil {
				log.Error().Err(err).Msg("snapshot: final write failed")
			} else {
				log.Info().Str("path", path).Msg("snapshot: final write on shutdown")
			}
			return
		case <-t.C:
			if err := WriteSnapshot(path, ms, c); err != nil {
				log.Error().Err(err).Msg("snapshot: write failed")
			}
		}
	}
}
