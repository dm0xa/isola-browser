package modulehost

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/rs/zerolog/log"
)

// pipeWebSocket relays an upgraded WebSocket connection to the module's /ws
// endpoint over its UDS. The host writes a reconstructed request head (the
// wire bytes including the upgrade headers must reach the module verbatim so
// its own upgrade succeeds), then moves raw bytes in both directions until
// either side closes. Everything after the module's 101 is opaque protocol
// data — the host never interprets it.
func pipeWebSocket(w http.ResponseWriter, r *http.Request, m *Module) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "webserver does not support connection hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack failed", http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	moduleConn, err := m.dialModule(r.Context())
	if err != nil {
		log.Warn().Str("module", m.man.Module).Err(err).Msg("modulehost: ws dial")
		// Nothing has been written to the client yet, so a plain error is
		// still possible (the upgrade handshake never started).
		http.Error(w, "module unavailable", http.StatusServiceUnavailable)
		return
	}
	defer moduleConn.Close()

	head, err := relayRequestHead(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := moduleConn.Write(head); err != nil {
		log.Warn().Str("module", m.man.Module).Err(err).Msg("modulehost: ws write")
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(moduleConn, clientBuf)
		moduleConn.Close()
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, moduleConn)
		clientConn.Close()
		done <- struct{}{}
	}()
	<-done
}

// relayRequestHead serializes the incoming request as a GET /ws?session_id=…
// HTTP/1.1 head addressed to the module over the UDS.
func relayRequestHead(r *http.Request) ([]byte, error) {
	reqURI := "/ws"
	q := url.Values{}
	if sid := r.URL.Query().Get("session_id"); sid != "" {
		q.Set("session_id", sid)
	}
	if len(q) > 0 {
		reqURI += "?" + q.Encode()
	}

	var head bytes.Buffer
	fmt.Fprintf(&head, "GET %s HTTP/1.1\r\n", reqURI)
	for k, vs := range r.Header {
		for _, v := range vs {
			fmt.Fprintf(&head, "%s: %s\r\n", k, v)
		}
	}
	if r.Host != "" {
		fmt.Fprintf(&head, "Host: %s\r\n", r.Host)
	} else {
		head.WriteString("Host: isola-module\r\n")
	}
	head.WriteString("\r\n")
	return head.Bytes(), nil
}
