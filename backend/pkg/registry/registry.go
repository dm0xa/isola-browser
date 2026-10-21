// Package registry provides a global plugin registry.
// Plugins self-register via init() and the main entry point reads from here
// to wire routes, session hooks, and frontend config.
package registry

import (
	"sync"

	"github.com/dm0xa/isola-browser/backend/pkg/plugin"
	"github.com/rs/zerolog/log"
)

var (
	mu sync.RWMutex

	authProviders   []plugin.AuthProvider
	sessionHooks    []plugin.SessionHook
	wsPlugins       []plugin.WebSocketPlugin
	dlpProviders    []plugin.DLPProvider
	urlPolicies     []plugin.URLPolicy
	auditLoggers    []plugin.AuditLogger
	sessionGuards   []plugin.SessionGuard
	frontendPlugins []plugin.FrontendPlugin
)

// RegisterAuth registers an AuthProvider plugin.
func RegisterAuth(p plugin.AuthProvider) {
	mu.Lock()
	defer mu.Unlock()
	authProviders = append(authProviders, p)
	log.Info().Str("name", p.Info().Name).Msg("plugin registered: auth")
}

// RegisterSessionHook registers a session lifecycle hook.
func RegisterSessionHook(h plugin.SessionHook) {
	mu.Lock()
	defer mu.Unlock()
	sessionHooks = append(sessionHooks, h)
	log.Info().Str("name", h.Info().Name).Msg("plugin registered: session hook")
}

// RegisterWebSocketPlugin registers a WebSocket sub-path handler.
func RegisterWebSocketPlugin(p plugin.WebSocketPlugin) {
	mu.Lock()
	defer mu.Unlock()
	wsPlugins = append(wsPlugins, p)
	log.Info().Str("name", p.Info().Name).Str("subpath", p.SubPath()).Msg("plugin registered: websocket")
}

// RegisterDLP registers a DLP provider.
func RegisterDLP(p plugin.DLPProvider) {
	mu.Lock()
	defer mu.Unlock()
	dlpProviders = append(dlpProviders, p)
	log.Info().Str("name", p.Info().Name).Msg("plugin registered: dlp")
}

// RegisterURLPolicy registers a URL policy provider.
func RegisterURLPolicy(p plugin.URLPolicy) {
	mu.Lock()
	defer mu.Unlock()
	urlPolicies = append(urlPolicies, p)
	log.Info().Str("name", p.Info().Name).Msg("plugin registered: url policy")
}

// RegisterAuditLogger registers an audit logger.
func RegisterAuditLogger(l plugin.AuditLogger) {
	mu.Lock()
	defer mu.Unlock()
	auditLoggers = append(auditLoggers, l)
	log.Info().Str("name", l.Info().Name).Msg("plugin registered: audit")
}

// RegisterSessionGuard registers a session guard for pre-creation checks (rate limiting, abuse prevention).
func RegisterSessionGuard(g plugin.SessionGuard) {
	mu.Lock()
	defer mu.Unlock()
	sessionGuards = append(sessionGuards, g)
	log.Info().Str("name", g.Info().Name).Msg("plugin registered: session guard")
}

// RegisterFrontendPlugin registers a frontend UI plugin.
func RegisterFrontendPlugin(p plugin.FrontendPlugin) {
	mu.Lock()
	defer mu.Unlock()
	frontendPlugins = append(frontendPlugins, p)
	log.Info().Str("name", p.Info().Name).Msg("plugin registered: frontend")
}

// AuthProviders returns all registered auth providers.
func AuthProviders() []plugin.AuthProvider {
	mu.RLock()
	defer mu.RUnlock()
	return append([]plugin.AuthProvider{}, authProviders...)
}

// UnregisterAuth removes a previously registered provider (identity match).
// Used by tests and dynamic module unload; a no-op if absent.
func UnregisterAuth(p plugin.AuthProvider) {
	mu.Lock()
	defer mu.Unlock()
	for i, candidate := range authProviders {
		if candidate == p {
			authProviders = append(authProviders[:i], authProviders[i+1:]...)
			return
		}
	}
}

// SessionHooks returns all registered session hooks.
func SessionHooks() []plugin.SessionHook {
	mu.RLock()
	defer mu.RUnlock()
	return append([]plugin.SessionHook{}, sessionHooks...)
}

// WebSocketPlugins returns all registered WebSocket plugins.
func WebSocketPlugins() []plugin.WebSocketPlugin {
	mu.RLock()
	defer mu.RUnlock()
	return append([]plugin.WebSocketPlugin{}, wsPlugins...)
}

// DLPProviders returns all registered DLP providers.
func DLPProviders() []plugin.DLPProvider {
	mu.RLock()
	defer mu.RUnlock()
	return append([]plugin.DLPProvider{}, dlpProviders...)
}

// URLPolicies returns all registered URL policy providers.
func URLPolicies() []plugin.URLPolicy {
	mu.RLock()
	defer mu.RUnlock()
	return append([]plugin.URLPolicy{}, urlPolicies...)
}

// AuditLoggers returns all registered audit loggers.
func AuditLoggers() []plugin.AuditLogger {
	mu.RLock()
	defer mu.RUnlock()
	return append([]plugin.AuditLogger{}, auditLoggers...)
}

// SessionGuards returns all registered session guards.
func SessionGuards() []plugin.SessionGuard {
	mu.RLock()
	defer mu.RUnlock()
	return append([]plugin.SessionGuard{}, sessionGuards...)
}

// FrontendPlugins returns all registered frontend plugins.
func FrontendPlugins() []plugin.FrontendPlugin {
	mu.RLock()
	defer mu.RUnlock()
	return append([]plugin.FrontendPlugin{}, frontendPlugins...)
}
