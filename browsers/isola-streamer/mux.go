package main

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/soheilhy/cmux"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: checkOrigin,
}

// pendingResize is the last /resize the runner was not connected to receive.
// The ctrl socket is dialled asynchronously, so a viewer that attaches during
// a cold start asks for its window size before the runner can hear it.
var (
	pendingResizeMu sync.Mutex
	pendingResize   *[2]int
)

// setPendingResize records (or clears) a resize the runner has not been told
// about. Clearing on a successful send keeps the pending value from
// overwriting a later, larger request.
func setPendingResize(width, height int, pending bool) {
	pendingResizeMu.Lock()
	defer pendingResizeMu.Unlock()
	if !pending {
		pendingResize = nil
		return
	}
	pendingResize = &[2]int{width, height}
}

// flushPendingResize sends the remembered resize once the runner is reachable.
// Called on every ctrl connect, which is the only way a dropped resize can be
// delivered: the request itself has already returned.
func flushPendingResize() {
	pendingResizeMu.Lock()
	pending := pendingResize
	pendingResize = nil
	pendingResizeMu.Unlock()
	if pending == nil {
		return
	}
	if !writeCtrlCommand(map[string]any{"cmd": "resize", "width": pending[0], "height": pending[1]}) {
		setPendingResize(pending[0], pending[1], true)
		return
	}
	log.Printf("tabs: flushed pending resize %dx%d", pending[0], pending[1])
}

// checkOrigin (VULN-17): no Origin is accepted (the backend relay sends none);
// a browser's Origin must match the Host.
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
	mux.HandleFunc("/seed", handleSeed)
	mux.HandleFunc("/identity", handleIdentity)
	mux.HandleFunc("/network", networkDumpHandler)
	mux.HandleFunc("/posture", postureHandler)
	mux.HandleFunc("/trackers", trackersHandler)
	mux.HandleFunc("/intel", intelHandler)
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
	registerCaptureRoutes(mux)
	mux.HandleFunc("/resize", func(w http.ResponseWriter, r *http.Request) {
		// Resizing is a state change for every viewer: POST only, or a prefetch re-keys every encoder.
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		widthStr := r.URL.Query().Get("width")
		heightStr := r.URL.Query().Get("height")
		width, errW := strconv.Atoi(widthStr)
		height, errH := strconv.Atoi(heightStr)
		if errW != nil || errH != nil || width <= 0 || height <= 0 ||
			width > maxSurfaceDim || height > maxSurfaceDim {
			http.Error(w, "invalid dimensions", http.StatusBadRequest)
			return
		}
		// Even dimensions only (yuv420p); round before the range check or 1x1 becomes 0x0
		// and the next viewer falls back to the env default against a stale surface.
		width &^= 1
		height &^= 1
		if width < minSurfaceDim || height < minSurfaceDim {
			http.Error(w, "invalid dimensions", http.StatusBadRequest)
			return
		}
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
		// The ctrl socket is dialled asynchronously at startup and a viewer
		// asks for its window size as soon as it attaches, so on a cold start
		// this call can arrive before the runner can hear it. Dropping it left
		// the surface at the launch resolution while every encoder waited for
		// the requested size, and the stream never started; remember it and
		// let startTabCtrl send it once the runner is reachable.
		setPendingResize(width, height, !notified)
		// Every attached viewer decodes an encoder whose output size was fixed
		// when it spawned, so each run is replaced and its viewers reset onto
		// the new geometry (stream_reset reason=resize).
		hubFor(globalBroadcaster).resizeAll(width, height)
		log.Printf("mux: resize %dx%d (notified=%v)", width, height, notified)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	httpSrv := &http.Server{Handler: mux}
	go httpSrv.Serve(httpL)

	go m.Serve()
	log.Printf("streamer :%s", port)
	return l, nil
}

// navigateHandler: drive the active tab via the runner's ctrl socket (also the
// warm-claim navigation). Same trust boundary as /audio and /resize.
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
