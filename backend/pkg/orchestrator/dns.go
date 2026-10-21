package orchestrator

import (
	"context"
	"fmt"
	"net"

	"github.com/rs/zerolog/log"

	"github.com/dm0xa/isola-browser/backend/pkg/dnsproxy"
)

// SetupDNSProxy starts the DNS-over-TLS / DNS-over-HTTPS forwarding server when
// the configured mode asks for it. The server binds this process's browser-net
// address on port 53 and, on success, sessions point their resolver at it.
//
// Plain mode is a no-op. On error the caller should log and continue: sessions
// fall back to plaintext `--dns` resolvers, so a failure here degrades rather
// than breaks container startup.
func (o *Orchestrator) SetupDNSProxy(ctx context.Context, mode string, servers []string) error {
	if mode != "tls" && mode != "https" {
		return nil
	}
	ip := o.proxyIPOnBrowserNet()
	if ip == "" {
		return fmt.Errorf("browser-net address unknown")
	}
	listen := net.JoinHostPort(ip, "53")
	srv, err := dnsproxy.NewServerForMode(mode, servers, []string{listen})
	if err != nil {
		return err
	}
	// Bind synchronously so a port conflict is reported here, before sessions
	// are pointed at an address nothing is listening on.
	if _, err := srv.Listen(); err != nil {
		return err
	}
	o.EnableDNSProxy()
	go func() {
		if err := srv.Serve(ctx); err != nil {
			log.Error().Err(err).Msg("dnsproxy: forwarder stopped")
		}
	}()
	log.Info().
		Str("mode", mode).
		Str("listen", listen).
		Int("upstreams", len(servers)).
		Msg("DNS forwarder started (encrypted resolution for sessions)")
	return nil
}
