package main

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/soheilhy/cmux"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: checkOrigin,
}

// checkOrigin validates WebSocket Origins (VULN-17). Requests without an
// Origin header are accepted because the backend relays viewer connections
// with a server-side client (websocket.DefaultDialer) that sends none.
// Browsers always send Origin, so a cross-site page is rejected unless the
// Origin host matches the Host it is connecting to.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// setupMux listens on the given port and routes HTTP/WS traffic to the
// provided handler via cmux byte 0x00 selector, matching the existing
// proxy selector byte for compatibility.
func setupMux(port string, wsHandler http.HandlerFunc) (net.Listener, error) {
	l, err := net.Listen("tcp", "0.0.0.0:"+port)
	if err != nil {
		return nil, err
	}

	m := cmux.New(l)
	httpL := m.Match(cmux.HTTP1Fast())
	// cmux requires every byte to be matched. HTTP is selected above;
	// anything else is closed so unmatched connections do not hang.
	otherL := m.Match(cmux.Any())
	go func() {
		for {
			conn, err := otherL.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/input", handleInputWS)
	mux.HandleFunc("/audio", audioToggleHandler)
	mux.HandleFunc("/navigate", navigateHandler)
	mux.HandleFunc("/network", networkDumpHandler)
	mux.HandleFunc("/graph.png", graphHandler)
	mux.HandleFunc("/links", linksHandler)
	mux.HandleFunc("/snapshot", snapshotHandler)
	mux.HandleFunc("/screenshot", screenshotHandler)
	mux.HandleFunc("/click", mcpClickHandler)
	mux.HandleFunc("/type", mcpTypeHandler)
	mux.HandleFunc("/", wsHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/debug/encoder", writeDebugEncoder)
	registerRecordRoutes(mux)
	mux.HandleFunc("/resize", func(w http.ResponseWriter, r *http.Request) {
		widthStr := r.URL.Query().Get("width")
		heightStr := r.URL.Query().Get("height")
		width, errW := strconv.Atoi(widthStr)
		height, errH := strconv.Atoi(heightStr)
		if errW != nil || errH != nil || width <= 0 || height <= 0 ||
			width > maxSurfaceDim || height > maxSurfaceDim {
			http.Error(w, "invalid dimensions", http.StatusBadRequest)
			return
		}
		// Even dimensions only: H.264 4:2:0 cannot encode odd ones.
		width &^= 1
		height &^= 1
		currentMu.Lock()
		currentWidth = width
		currentHeight = height
		currentMu.Unlock()
		// The runner owns the surface, so resize its offscreen windows first:
		// by the time the encoders are re-keyed below, frames at the new
		// geometry are already on their way.
		notified := writeCtrlCommand(map[string]any{
			"cmd": "resize", "width": width, "height": height,
		})
		// Every attached viewer decodes an encoder whose output size was fixed
		// when it spawned, so each run is replaced and its viewers reset onto
		// the new geometry (stream_reset reason=resize).
		hubFor(globalBroadcaster).resizeAll(width, height)
		log.Printf("mux: resize to %dx%d (runner notified=%v)", width, height, notified)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	httpSrv := &http.Server{Handler: mux}
	go httpSrv.Serve(httpL)

	go m.Serve()
	log.Printf("isola-streamer listening on :%s", port)
	return l, nil
}

// navigateHandler drives the active tab to a new URL through the runner's
// ctrl socket (same path as viewer address-bar navigation). Warm-pool
// containers boot on about:blank and are pointed at the session's target URL
// here at claim time. Trust boundary is identical to /audio and /resize: the
// port is reachable only from the backend proxy or the host loopback.
func navigateHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("url"))
	if target == "" {
		http.Error(w, "url required", http.StatusBadRequest)
		return
	}
	if !writeCtrlCommand(map[string]any{"cmd": "tab_navigate", "url": target}) {
		http.Error(w, "runner ctrl socket not connected", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
