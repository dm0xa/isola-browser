package registry

import (
	"context"
	"sync"

	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/plugin"
)

// zerologAuditLogger writes audit events as structured zerolog entries. It
// gives deployments an audit trail (session create/terminate) without an extra
// plugin, which previously left the build with no audit logging at all
// (VULN-27).
type zerologAuditLogger struct{}

func (zerologAuditLogger) Info() plugin.Info {
	return plugin.Info{Name: "zerolog-audit", Version: "1.0.0"}
}

func (zerologAuditLogger) Log(_ context.Context, ev plugin.AuditEvent) {
	entry := log.Info().
		Str("event_type", ev.Type).
		Time("event_time", ev.Timestamp)
	if ev.SessionID != "" {
		entry = entry.Str("session_id", ev.SessionID)
	}
	if ev.UserID != "" {
		entry = entry.Str("user_id", ev.UserID)
	}
	if len(ev.Data) > 0 {
		entry = entry.Interface("data", ev.Data)
	}
	entry.Msg("audit")
}

var defaultAuditOnce sync.Once

// RegisterDefaultAuditLogger installs the built-in zerolog audit logger once.
// cmd/*/main.go calls it so every binary emits an audit trail.
func RegisterDefaultAuditLogger() {
	defaultAuditOnce.Do(func() {
		RegisterAuditLogger(zerologAuditLogger{})
	})
}
