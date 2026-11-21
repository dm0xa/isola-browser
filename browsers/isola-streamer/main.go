package main

import (
	"log"
	"net/http"
	"os"

	"github.com/gorilla/websocket"
)

var (
	// globalFrameBroadcaster is started once in main() and shared across all
	// viewer sessions. The UDS listener must be live before chromium-isola
	// launches, so frames are captured from container start.
	globalBroadcaster *frameBroadcaster
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "5900"
	}

	frameSock := os.Getenv("ISOLA_FRAME_SOCK")
	if frameSock == "" {
		frameSock = "/run/isola/frame.sock"
	}

	currentWidth = parseIntEnv("ISOLA_WIDTH", 1280)
	currentHeight = parseIntEnv("ISOLA_HEIGHT", 720)
	currentFPS = parseIntEnv("ISOLA_FPS", 30)
	currentBitrateKbps = parseIntEnv("ISOLA_BITRATE_KBPS", 4000)

	// Start the frame UDS listener at container boot — before chromium-isola
	// launches. This is critical: chromium-isola tries to connect immediately
	// and will hang/fail if nothing is listening.
	log.Printf("mux: frame listener %s", frameSock)
	incoming, err := listenFrames(frameSock)
	if err != nil {
		log.Fatalf("frame listener %s: %v", frameSock, err)
	}
	globalBroadcaster = newFrameBroadcaster(incoming)

	initInput()
	initCapture()
	initActivityTracker()
	startTabCtrl()
	startInputTargetWatchdog()
	startRendererWatchdog()
	go readAudioRTP()

	l, err := setupMux(port, handleRoot)
	if err != nil {
		log.Fatalf("mux: %v", err)
	}
	defer l.Close()

	// Block forever
	select {}
}

// handleRoot serves the catch-all route: viewer WebSocket upgrades, the root
// status probe, and 404 for any other HTTP path.
//
// The 404 matters beyond cleanliness. The backend feature-detects the warm
// pool's /navigate endpoint by status code, so a catch-all 200 would make an
// older image look capable and hand the user a container stuck on
// about:blank. Shared with tests so the contract cannot drift.
func handleRoot(w http.ResponseWriter, r *http.Request) {
	if websocket.IsWebSocketUpgrade(r) {
		handleViewerWS(w, r)
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}
