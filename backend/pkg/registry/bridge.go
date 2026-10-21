// Package registry provides builder functions that compose plugin registry
// entries into plain function types for the api.HandlerExt extension points.
// These are the ONLY bridge between module-aware code and general handler code.
//
// Imported exclusively by cmd/*/main.go — general packages never import registry.
package registry

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/api"
	"github.com/dm0xa/isola-browser/backend/pkg/plugin"
)

// BuildSessionValidator returns a validator that runs all registered session guards
// and URL policies.
func BuildSessionValidator() func(ctx context.Context, r *http.Request, userID uuid.UUID, fingerprint, targetURL string) error {
	return func(ctx context.Context, r *http.Request, userID uuid.UUID, fingerprint, targetURL string) error {
		for _, guard := range SessionGuards() {
			if err := guard.BeforeCreate(ctx, r, userID, fingerprint); err != nil {
				return err
			}
		}
		// Policies receive the caller's identity (uuid.Nil for anonymous) so
		// per-user/group rules can resolve. Host/port are unknown pre-launch.
		sessCtx := &plugin.SessionContext{UserID: userID.String()}
		for _, policy := range URLPolicies() {
			allowed, reason := policy.AllowURL(ctx, sessCtx, targetURL)
			if !allowed {
				return fmt.Errorf("%s", reason)
			}
		}
		return nil
	}
}

// BuildOnSessionReady returns a callback that fires all session hooks (OnCreate)
// and audit loggers with a session.create event.
func BuildOnSessionReady() func(ctx context.Context, ev api.SessionEvent) {
	return func(ctx context.Context, ev api.SessionEvent) {
		sc := &plugin.SessionContext{
			SessionID:   ev.SessionID,
			UserID:      ev.UserID,
			ContainerID: ev.ContainerID,
			TargetURL:   ev.TargetURL,
			BrowserType: ev.BrowserType,
			VncPassword: ev.VncPassword,
			Host:        ev.Host,
			Port:        ev.Port,
			CdpPort:     ev.CdpPort,
		}
		for _, hook := range SessionHooks() {
			if err := hook.OnCreate(ctx, sc); err != nil {
				log.Warn().Err(err).Str("hook", hook.Info().Name).Msg("session hook OnCreate failed")
			}
		}
		for _, audit := range AuditLoggers() {
			audit.Log(ctx, plugin.AuditEvent{
				Type:      "session.create",
				SessionID: ev.SessionID,
				UserID:    ev.UserID,
				Timestamp: time.Now(),
				Data: map[string]interface{}{
					"target_url":   ev.TargetURL,
					"browser_type": ev.BrowserType,
				},
			})
		}
	}
}

// BuildOnSessionDeleted returns a callback that fires all session hooks (OnTerminate)
// and audit loggers with a session.terminate event.
func BuildOnSessionDeleted() func(ctx context.Context, ev api.SessionEvent) {
	return func(ctx context.Context, ev api.SessionEvent) {
		sc := &plugin.SessionContext{
			SessionID:   ev.SessionID,
			UserID:      ev.UserID,
			ContainerID: ev.ContainerID,
			TargetURL:   ev.TargetURL,
			BrowserType: ev.BrowserType,
		}
		for _, hook := range SessionHooks() {
			if err := hook.OnTerminate(ctx, sc); err != nil {
				log.Warn().Err(err).Str("hook", hook.Info().Name).Msg("session hook OnTerminate failed")
			}
		}
		for _, audit := range AuditLoggers() {
			audit.Log(ctx, plugin.AuditEvent{
				Type:      "session.terminate",
				SessionID: ev.SessionID,
				UserID:    ev.UserID,
				Timestamp: time.Now(),
			})
		}
	}
}

// BuildPluginConfig returns a plugin config provider from registered frontend plugins.
func BuildPluginConfig() api.PluginConfigFunc {
	return func() []map[string]interface{} {
		plugins := FrontendPlugins()
		if len(plugins) == 0 {
			return nil
		}
		out := make([]map[string]interface{}, len(plugins))
		for i, p := range plugins {
			out[i] = map[string]interface{}{
				"name":    p.Info().Name,
				"version": p.Info().Version,
				"config":  p.FrontendConfig(),
			}
		}
		return out
	}
}
