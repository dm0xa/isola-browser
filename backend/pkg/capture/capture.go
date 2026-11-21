// Package capture stores resource bodies recorded by a sandbox session.
//
// The streamer inside the container captures decoded response bodies; this
// package pulls them out over HTTP while the session is alive, verifies every
// hash itself, and keeps a per-session archive on the host:
//
//	$ISOLA_DATA_DIR/captures/<session_id>/
//	  meta.json            # archive metadata + items (0600)
//	  blobs/<aa>/<sha256>  # verified decoded bodies (0644)
//	  blobs/tmp/<random>   # same filesystem, renamed into place after verify
//	  tombstone            # present iff finalize could not complete the archive
//
// The sandbox is never mounted into this process: the archive is pulled over the
// loopback streamer port, so container hardening and `--rm` stay untouched.
package capture

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Defaults mirror the shipped config (backend/config.yaml, .env.example). A
// zero-valued Config field falls back to these so tests can enable capture
// without repeating every limit.
const (
	DefaultMaxFileBytes    int64 = 8 << 20   // 8 MiB per resource
	DefaultMaxRecordBytes  int64 = 256 << 20 // 256 MiB per session
	DefaultTotalQuotaBytes int64 = 2 << 30   // 2 GiB on disk across archives
	DefaultRetention             = 30 * 24 * time.Hour
)

// Config is the capture subsystem configuration. It mirrors config.Capture.
type Config struct {
	Enabled         bool
	Dir             string        // $ISOLA_DATA_DIR/captures
	MaxFileBytes    int64         // per-resource decoded size limit
	MaxRecordBytes  int64         // per-session record budget
	TotalQuotaBytes int64         // host-wide archive budget
	Retention       time.Duration // delete archives older than this (0 = never)
}

// StartResult is returned by Manager.Start.
type StartResult struct {
	ArchiveID  string
	ArchiveKey string
	StartedAt  time.Time
}

// StopResult is returned by Manager.Stop.
type StopResult struct {
	Items    int
	Bytes    int64
	Deduped  int64
	Complete bool
}

// Status is the live recording state of one session.
type Status struct {
	Recording bool
	SessionID string
	Items     int
	Bytes     int64
	Deduped   int64
	Truncated bool
	StartedAt time.Time
}

// Summary is one archive row for the operator listing.
type Summary struct {
	ArchiveID string
	SessionID string
	TargetURL string
	CreatedAt time.Time
	EndedAt   time.Time
	Complete  bool
	Items     int
	Bytes     int64
}

// Archive is a parsed meta.json, returned verbatim to the API caller.
type Archive struct {
	Meta map[string]any
}

// Sentinel errors map onto the control-plane error codes.
var (
	ErrDisabled         = errors.New("resource capture is disabled")
	ErrNotRecording     = errors.New("no resource capture is recording for this session")
	ErrArchiveNotFound  = errors.New("resource archive not found")
	ErrBlobNotFound     = errors.New("resource blob not found")
	ErrInvalidArchiveID = errors.New("invalid archive id")
	ErrInvalidHash      = errors.New("invalid sha256")
	ErrAlreadyRecording = errors.New("another resource capture is already recording")
	ErrQuotaExceeded    = errors.New("resource capture quota exceeded")
)

// archiveIDPattern keeps the archive id usable as a single path component.
// Session ids are UUIDs, which match; anything else is rejected before it can
// reach the filesystem.
var archiveIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func validArchiveID(id string) bool {
	return archiveIDPattern.MatchString(id)
}

// validSHA256 accepts exactly 64 lowercase hex characters. The streamer emits
// lowercase; an uppercase or malformed value would otherwise become a filename.
func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// normalizeConfig applies defaults and keeps nonsense negative values from
// turning into "unlimited" by accident.
func normalizeConfig(cfg Config) Config {
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = DefaultMaxFileBytes
	}
	if cfg.MaxRecordBytes <= 0 {
		cfg.MaxRecordBytes = DefaultMaxRecordBytes
	}
	if cfg.TotalQuotaBytes <= 0 {
		cfg.TotalQuotaBytes = DefaultTotalQuotaBytes
	}
	if cfg.Retention < 0 {
		cfg.Retention = 0
	}
	if strings.TrimSpace(cfg.Dir) == "" {
		// Defense in depth: config.Load always resolves this, but a direct
		// caller must never get a relative archive path.
		root := os.Getenv("ISOLA_DATA_DIR")
		if root == "" {
			root = "/var/lib/isola"
		}
		cfg.Dir = filepath.Join(root, "captures")
	}
	return cfg
}
