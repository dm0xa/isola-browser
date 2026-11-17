// Package dnsproxy: a stdlib-only DNS forwarder. Browser containers send it
// plaintext DNS (--dns); it forwards over DoT (RFC 7858) or DoH (RFC 8484).
// Queries stay opaque, so client EDNS0 is honoured end-to-end.
package dnsproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Protocol is the wire protocol used to reach an upstream resolver.
type Protocol string

const (
	// ProtoPlain is classic cleartext DNS over UDP/TCP port 53.
	ProtoPlain Protocol = "plain"
	// ProtoTLS is DNS-over-TLS (RFC 7858), typically port 853.
	ProtoTLS Protocol = "tls"
	// ProtoHTTPS is DNS-over-HTTPS (RFC 8484), typically port 443.
	ProtoHTTPS Protocol = "https"
)

const (
	// DefaultTimeout bounds a single upstream exchange.
	DefaultTimeout = 5 * time.Second

	plainPort = "53"
	dotPort   = "853"

	// maxMessage is the largest DNS message we will read from an upstream.
	// 64 KiB is the hard DNS-over-TCP limit.
	maxMessage = 65535

	// maxConcurrentQueries bounds in-flight UDP queries. A sandboxed page can
	// trigger a flood of distinct lookups; without a ceiling each datagram would
	// spawn a goroutine and a fresh upstream connection in the shared backend.
	maxConcurrentQueries = 512
)

// Upstream is a single resolver endpoint.
type Upstream struct {
	proto      Protocol
	address    string // host:port for plain/TLS
	serverName string // TLS SNI (DoT) / expected name (DoH)
	dohURL     string // full URL for DoH

	// Test seams. Production leaves these nil.
	tlsConfig  *tls.Config
	httpClient *http.Client
}

// String renders the upstream for logs.
func (u Upstream) String() string {
	if u.proto == ProtoHTTPS {
		return u.dohURL
	}
	return string(u.proto) + "://" + u.address
}

// ParseUpstream: bare host → mode's protocol; an explicit scheme always wins, so
// one list can mix plain/tls/https endpoints.
func ParseUpstream(raw string, mode Protocol) (Upstream, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Upstream{}, fmt.Errorf("empty DNS endpoint")
	}

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return Upstream{}, fmt.Errorf("parse DNS endpoint %q: %w", raw, err)
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "doh":
			return parseDoH(raw, u)
		case "tls", "dot":
			return parseDoT(u.Host, "")
		case "plain", "udp", "tcp", "dns":
			return parsePlain(u.Host)
		default:
			return Upstream{}, fmt.Errorf("unsupported DNS endpoint scheme %q in %q", u.Scheme, raw)
		}
	}

	switch mode {
	case ProtoTLS:
		return parseDoT(raw, "")
	case ProtoHTTPS:
		return parseDoH("", &url.URL{Scheme: "https", Host: raw, Path: "/dns-query"})
	default:
		return parsePlain(raw)
	}
}

func parsePlain(hostport string) (Upstream, error) {
	addr, err := ensurePort(hostport, plainPort)
	if err != nil {
		return Upstream{}, err
	}
	return Upstream{proto: ProtoPlain, address: addr}, nil
}

func parseDoT(hostport, _ string) (Upstream, error) {
	addr, err := ensurePort(hostport, dotPort)
	if err != nil {
		return Upstream{}, err
	}
	host, _, _ := net.SplitHostPort(addr)
	return Upstream{proto: ProtoTLS, address: addr, serverName: host}, nil
}

func parseDoH(raw string, u *url.URL) (Upstream, error) {
	// A mode-derived endpoint arrives with raw empty and u populated; a
	// user-provided one arrives with raw set.
	if raw == "" {
		raw = u.String()
	}
	if u.Host == "" {
		return Upstream{}, fmt.Errorf("DNS-over-HTTPS endpoint %q has no host", raw)
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	if u.Path == "" {
		u.Path = "/dns-query"
	}
	raw = u.String()
	return Upstream{proto: ProtoHTTPS, dohURL: raw, serverName: u.Hostname()}, nil
}

func ensurePort(hostport, defaultPort string) (string, error) {
	if hostport == "" {
		return "", fmt.Errorf("DNS endpoint has no host")
	}
	if _, _, err := net.SplitHostPort(hostport); err == nil {
		return hostport, nil
	}
	// No port: append the default. A bare IPv6 literal needs brackets.
	if strings.Contains(hostport, ":") {
		return net.JoinHostPort(hostport, defaultPort), nil
	}
	return hostport + ":" + defaultPort, nil
}

// exchange sends one raw DNS query upstream and returns the raw response.
func (u Upstream) exchange(ctx context.Context, query []byte) ([]byte, error) {
	switch u.proto {
	case ProtoTLS:
		return u.exchangeTLS(ctx, query)
	case ProtoHTTPS:
		return u.exchangeHTTPS(ctx, query)
	default:
		return u.exchangePlain(ctx, query)
	}
}

func (u Upstream) exchangePlain(ctx context.Context, query []byte) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", u.address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline(ctx))

	if _, err := conn.Write(query); err != nil {
		return nil, err
	}
	buf := make([]byte, maxMessage)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	resp := make([]byte, n)
	copy(resp, buf[:n])
	// A truncated answer must be retried over TCP.
	if len(resp) >= 4 && resp[2]&0x02 != 0 {
		return u.exchangeTCP(ctx, query)
	}
	return resp, nil
}

func (u Upstream) exchangeTCP(ctx context.Context, query []byte) ([]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", u.address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline(ctx))
	if err := writeFramed(conn, query); err != nil {
		return nil, err
	}
	return readFramed(conn)
}

func (u Upstream) exchangeTLS(ctx context.Context, query []byte) ([]byte, error) {
	cfg := u.tlsConfig
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: u.serverName}
	}
	d := &tls.Dialer{Config: cfg}
	conn, err := d.DialContext(ctx, "tcp", u.address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline(ctx))
	if err := writeFramed(conn, query); err != nil {
		return nil, err
	}
	return readFramed(conn)
}

func (u Upstream) exchangeHTTPS(ctx context.Context, query []byte) ([]byte, error) {
	client := u.httpClient
	if client == nil {
		client = defaultDoHClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.dohURL, bytes.NewReader(query))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH upstream returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage))
	if err != nil {
		return nil, err
	}
	if len(body) < 12 {
		return nil, fmt.Errorf("DoH upstream returned a short DNS message (%d bytes)", len(body))
	}
	return body, nil
}

var defaultDoHClient = &http.Client{Timeout: DefaultTimeout}

func deadline(ctx context.Context) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now().Add(DefaultTimeout)
}

func writeFramed(conn net.Conn, msg []byte) error {
	out := make([]byte, 2+len(msg))
	binary.BigEndian.PutUint16(out, uint16(len(msg)))
	copy(out[2:], msg)
	_, err := conn.Write(out)
	return err
}

func readFramed(conn net.Conn) ([]byte, error) {
	var lenBuf [2]byte
	if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(lenBuf[:]))
	if n == 0 {
		return nil, fmt.Errorf("empty DNS response")
	}
	resp := make([]byte, n)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// Server accepts plaintext DNS and forwards to the configured upstreams.
type Server struct {
	listenAddrs []string
	upstreams   []Upstream
	timeout     time.Duration

	udpConns []net.PacketConn
	tcpLns   []net.Listener

	// sem caps in-flight UDP queries so a lookup flood cannot exhaust
	// goroutines and upstream connections in this shared process.
	sem chan struct{}
}

// New builds a server. listenAddrs are bound on both UDP and TCP; upstreams are
// tried in order until one answers.
func New(listenAddrs []string, upstreams []Upstream) *Server {
	return &Server{listenAddrs: listenAddrs, upstreams: upstreams, timeout: DefaultTimeout}
}

// NewServerForMode: bare hosts become tls://host:853 (or DoH); full URLs used as-is.
// Error for plain mode or no usable upstreams.
func NewServerForMode(mode string, servers, listenAddrs []string) (*Server, error) {
	proto := Protocol(strings.ToLower(strings.TrimSpace(mode)))
	if proto != ProtoTLS && proto != ProtoHTTPS {
		return nil, fmt.Errorf("dnsproxy: mode %q needs to be %q or %q", mode, ProtoTLS, ProtoHTTPS)
	}
	upstreams := make([]Upstream, 0, len(servers))
	for _, raw := range servers {
		u, err := ParseUpstream(raw, proto)
		if err != nil {
			return nil, fmt.Errorf("dnsproxy: %w", err)
		}
		upstreams = append(upstreams, u)
	}
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("dnsproxy: no upstreams for mode %q (set dns.servers)", mode)
	}
	return New(listenAddrs, upstreams), nil
}

// Listen binds UDP+TCP up front and returns the real addresses ("host:0" resolves).
func (s *Server) Listen() ([]string, error) {
	bound := make([]string, 0, len(s.listenAddrs))
	for _, addr := range s.listenAddrs {
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			s.closeAll()
			return nil, fmt.Errorf("dnsproxy: udp listen %s: %w", addr, err)
		}
		s.udpConns = append(s.udpConns, pc)

		// Bind TCP to the address the UDP socket actually resolved, so the two
		// share a port when the request used port 0.
		tcpAddr := pc.LocalAddr().String()
		ln, err := net.Listen("tcp", tcpAddr)
		if err != nil {
			s.closeAll()
			return nil, fmt.Errorf("dnsproxy: tcp listen %s: %w", tcpAddr, err)
		}
		s.tcpLns = append(s.tcpLns, ln)

		// Report the resolved address (a :0 port is chosen by the kernel).
		bound = append(bound, tcpAddr)
	}
	return bound, nil
}

// Serve serves on the listeners opened by Listen until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	if s.sem == nil {
		s.sem = make(chan struct{}, maxConcurrentQueries)
	}
	var wg sync.WaitGroup
	for _, pc := range s.udpConns {
		wg.Add(1)
		go func(pc net.PacketConn) {
			defer wg.Done()
			s.serveUDP(ctx, pc)
		}(pc)
	}
	for _, ln := range s.tcpLns {
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			s.serveTCP(ctx, ln)
		}(ln)
	}

	<-ctx.Done()
	s.closeAll()
	wg.Wait()
	return nil
}

func (s *Server) closeAll() {
	for _, pc := range s.udpConns {
		pc.Close()
	}
	for _, ln := range s.tcpLns {
		ln.Close()
	}
}

// ListenAndServe binds the configured addresses and serves until ctx is
// cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if _, err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
}

func (s *Server) serveUDP(ctx context.Context, pc net.PacketConn) {
	buf := make([]byte, maxMessage)
	for {
		n, clientAddr, err := pc.ReadFrom(buf)
		if err != nil {
			return // listener closed
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		select {
		case s.sem <- struct{}{}:
			go func() {
				defer func() { <-s.sem }()
				resp := s.resolve(ctx, query)
				if resp != nil {
					_, _ = pc.WriteTo(resp, clientAddr)
				}
			}()
		default:
			// Saturated: answer SERVFAIL inline (no goroutine, no upstream) so
			// the client fails fast and the process stays bounded.
			if resp := servfail(query); resp != nil {
				_, _ = pc.WriteTo(resp, clientAddr)
			}
		}
	}
}

func (s *Server) serveTCP(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handleTCPConn(ctx, conn)
	}
}

func (s *Server) handleTCPConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.timeout))
		query, err := readFramed(conn)
		if err != nil {
			return
		}
		resp := s.resolve(ctx, query)
		if resp == nil {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(s.timeout))
		if err := writeFramed(conn, resp); err != nil {
			return
		}
	}
}

// resolve forwards a query, trying each upstream in order and returning the
// first usable answer. When every upstream fails it returns a SERVFAIL built
// from the query so the client gets a well-formed failure instead of a timeout.
func (s *Server) resolve(ctx context.Context, query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	for _, u := range s.upstreams {
		resp, err := u.exchange(ctx, query)
		if err == nil && len(resp) >= 12 {
			return resp
		}
		if err != nil {
			log.Debug().Err(err).Str("upstream", u.String()).Msg("dnsproxy: upstream")
		} else {
			log.Debug().Str("upstream", u.String()).Msg("dnsproxy: short response")
		}
		if ctx.Err() != nil {
			break
		}
	}
	return servfail(query)
}

// servfail turns a query into a minimal SERVFAIL response: same header and
// question, no records. This is preferred over silence so a browser fails fast
// instead of retrying until its own timeout.
func servfail(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	resp := make([]byte, len(query))
	copy(resp, query)
	resp[2] |= 0x80                            // QR: this is a response
	resp[2] &^= 0x02                           // clear TC
	resp[3] = 0x80 | 0x02                      // RA=1, RCODE=2 (SERVFAIL)
	binary.BigEndian.PutUint16(resp[6:8], 0)   // ANCOUNT
	binary.BigEndian.PutUint16(resp[8:10], 0)  // NSCOUNT
	binary.BigEndian.PutUint16(resp[10:12], 0) // ARCOUNT
	return resp
}
