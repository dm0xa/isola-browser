package main

import (
	"encoding/json"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Tab control: the electron runner owns the actual tabs and listens on a
// Unix control socket (/run/isola/ctrl.sock). Viewers send tab commands
// (tab_new/tab_switch/tab_close/get_tabs) through their input WS, this
// relays them to the runner, and "tabs" state broadcasts flow back out to
// every connected viewer so the client can render a tab strip.
// When the active tab changes we point the CDP input client at the new
// page target (matched by URL prefix) and force a reconnect.

// unknownTab is the id capture events carry until the runner reports its tab
// strip. It is never a real tab id (those are positive and monotonic), so it
// can be recognised and rewritten later — see reassignUnknownTab.
const unknownTab = -1

var (
	tabMu          sync.Mutex
	tabListeners   = map[int]func(map[string]any){}
	tabListenerSeq int
	ctrlConn       net.Conn
	activeTabID    = unknownTab
	lastTabsMsg    map[string]any // last broadcast, replayed to new listeners
	runnerTabs     []tabInfo      // last known tab strip state (id/url/title/active)
)

// tabInfo mirrors one entry of the runner's `tabs` broadcast.
type tabInfo struct {
	ID     int    `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
}

// currentTabID returns the id of the currently active tab. Network/console
// entries are attributed to it at event time: the CDP client attaches to
// exactly one page target (the active tab), so every captured event belongs
// to whatever tab was active when it arrived.
func currentTabID() int {
	tabMu.Lock()
	defer tabMu.Unlock()
	return activeTabID
}

// activeTabURL returns the URL of the currently active tab ("" while the
// runner has not reported a tab strip yet). The input-target watchdog and the
// debug overlay compare the CDP binding against it.
func activeTabURL() string {
	tabMu.Lock()
	defer tabMu.Unlock()
	for _, t := range runnerTabs {
		if t.ID == activeTabID && t.URL != "" {
			return t.URL
		}
	}
	return ""
}

// tabForTarget maps a CDP page target to the runner tab that owns it.
//
// Capture is per target (see cdp_capture.go), but the panel filters by tab, so
// every target needs a tab id. Matching is by URL — exact first, then
// prefix, with the title as a tiebreak. When two tabs are indistinguishable
// (same URL and title, e.g. the same page opened twice) the active tab wins,
// which is exactly what the pre-auto-attach code did with every event.
// Targets that match nothing also fall back to the active tab, so the common
// single-tab case never regresses to "unattributed".
func tabForTarget(targetURL, targetTitle string) int {
	tabMu.Lock()
	tabs := runnerTabs
	active := activeTabID
	tabMu.Unlock()
	if len(tabs) == 0 {
		return active
	}
	if isBlankTargetURL(targetURL) {
		return active
	}

	exact := -1
	exactTitle := -1
	prefix := -1
	prefixTitle := -1
	exactCount := 0
	prefixCount := 0
	for _, t := range tabs {
		titleMatch := targetTitle != "" && t.Title == targetTitle
		switch {
		case t.URL != "" && t.URL == targetURL:
			exactCount++
			if exact == -1 {
				exact = t.ID
			}
			if titleMatch && exactTitle == -1 {
				exactTitle = t.ID
			}
		case t.URL != "" && (strings.HasPrefix(targetURL, t.URL) || strings.HasPrefix(t.URL, targetURL)):
			prefixCount++
			if prefix == -1 {
				prefix = t.ID
			}
			if titleMatch && prefixTitle == -1 {
				prefixTitle = t.ID
			}
		}
	}
	if exactCount == 1 {
		return exact
	}
	if exactCount > 1 {
		if exactTitle != -1 {
			return exactTitle
		}
		return active
	}
	if prefixCount == 1 {
		return prefix
	}
	if prefixCount > 1 {
		if prefixTitle != -1 {
			return prefixTitle
		}
		return active
	}
	return active
}

func registerTabListener(fn func(map[string]any)) func() {
	tabMu.Lock()
	tabListenerSeq++
	id := tabListenerSeq
	tabListeners[id] = fn
	last := lastTabsMsg
	tabMu.Unlock()
	// Replay the current state so newly attached viewers render the strip
	// without waiting for the next change.
	if last != nil {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("tabs: listener replay panic: %v", r)
				}
			}()
			fn(last)
		}()
	}
	return func() {
		tabMu.Lock()
		delete(tabListeners, id)
		tabMu.Unlock()
	}
}

// broadcastToInputWS fans a message out to every connected viewer's input
// WebSocket (tab state, cursor shape, network entries).
func broadcastToInputWS(msg map[string]any) {
	tabMu.Lock()
	fns := make([]func(map[string]any), 0, len(tabListeners))
	for _, fn := range tabListeners {
		fns = append(fns, fn)
	}
	tabMu.Unlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("inputws: listener panic: %v", r)
				}
			}()
			fn(msg)
		}()
	}
}

func broadcastTabs(msg map[string]any) {
	tabMu.Lock()
	fns := make([]func(map[string]any), 0, len(tabListeners))
	for _, fn := range tabListeners {
		fns = append(fns, fn)
	}
	tabMu.Unlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("tabs: listener panic: %v", r)
				}
			}()
			fn(msg)
		}()
	}
}

// applyTabsState installs one runner `tabs` broadcast: it becomes the attribution
// source for later capture events and the state replayed to new viewers.
func applyTabsState(tabsOut []tabInfo) {
	newActive := -1
	var activeURL string
	for _, t := range tabsOut {
		if t.Active {
			newActive = t.ID
			activeURL = t.URL
		}
	}

	tabMu.Lock()
	switched := newActive != activeTabID
	// Capture starts before this socket is up, so anything captured until now
	// carries the "no tab known yet" id (see reassignUnknownTab).
	wasUnknown := activeTabID < 0
	activeTabID = newActive
	runnerTabs = tabsOut
	tabMu.Unlock()

	if wasUnknown && newActive > 0 {
		reassignUnknownTab(newActive)
	}
	if switched && activeURL != "" && cdpClient != nil {
		// Input must land in the newly active tab: point the
		// CDP client at its page target and reattach.
		cdpClient.SetPreferredTarget(activeURL)
		cdpClient.ForceReconnect()
	}
	msg := map[string]any{"type": "tabs", "tabs": tabsOut}
	tabMu.Lock()
	lastTabsMsg = msg
	tabMu.Unlock()
	broadcastTabs(msg)
}

// reassignUnknownTab rewrites traffic captured before the runner reported its
// tab strip onto the tab that is now active, and re-publishes the corrected
// entries.
//
// activeTabID starts at -1, so the START_URL load — usually the largest burst
// of a session — is captured before any tab id exists. The panel merges the tab
// strip with the ids it sees in entries, so that -1 bucket rendered as a second,
// phantom tab named after its busiest host ("1 tab in the browser, 2 in the
// panel"). Folding it into the active tab is the same best-effort choice
// tabForTarget makes for a target it cannot match. Re-publishing is safe: the
// viewer merges entries by id, so a row is corrected in place rather than
// duplicated.
func reassignUnknownTab(tab int) {
	if tab <= 0 {
		return
	}

	var updated []netRequest
	netMu.Lock()
	for _, r := range netRequests {
		if r.tab == unknownTab {
			r.tab = tab
			// Copy under the lock: a body hash may settle concurrently, and the
			// copy must not be read while that writes the same struct.
			updated = append(updated, *r)
		}
	}
	netMu.Unlock()
	for i := range updated {
		broadcastNetEntry(&updated[i])
	}

	var lines []consoleEntry
	consoleMu.Lock()
	for i := range consoleBuffer {
		if consoleBuffer[i].Tab != unknownTab {
			continue
		}
		consoleBuffer[i].Tab = tab
		// The dedupe key embeds the tab, so carry the rewritten entry over or
		// the same line logged again would be added a second time.
		consoleSentKeys[consoleKey(consoleBuffer[i])] = true
		lines = append(lines, consoleBuffer[i])
	}
	consoleMu.Unlock()
	for _, e := range lines {
		broadcastToInputWS(map[string]any{"type": "console", "l": e.Level, "t": e.Text, "tb": e.Tab, "ts": e.TS})
	}
}

func forwardTabCommand(data []byte) {
	// Client messages use {"type":"tab_new",...}; the runner protocol is
	// {"cmd":"tab_new",...}. Translate instead of forwarding raw.
	var base struct {
		Type string  `json:"type"`
		ID   float64 `json:"id"`
		URL  string  `json:"url"`
	}
	if json.Unmarshal(data, &base) != nil || base.Type == "" {
		return
	}
	cmd := map[string]any{"cmd": base.Type}
	if base.ID > 0 {
		cmd["id"] = int(base.ID)
	}
	if base.URL != "" {
		cmd["url"] = base.URL
	}
	out, _ := json.Marshal(cmd)
	log.Printf("tabs: cmd → %s", out)
	if !writeCtrlCommand(cmd) {
		log.Printf("tabs: runner ctrl socket not connected, dropping command")
	}
}

// writeCtrlCommand sends one JSON-lines command to the runner's ctrl socket.
// It reports whether the socket was connected: a command written while the
// runner is down is lost, so callers whose state must converge re-send it when
// the socket comes up (see startTabCtrl).
func writeCtrlCommand(cmd map[string]any) bool {
	out, err := json.Marshal(cmd)
	if err != nil {
		return false
	}
	tabMu.Lock()
	conn := ctrlConn
	tabMu.Unlock()
	if conn == nil {
		return false
	}
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(append(out, '\n')); err != nil {
		log.Printf("tabs: ctrl write failed: %v", err)
		return false
	}
	return true
}

func startTabCtrl() {
	go func() {
		for {
			conn, err := net.Dial("unix", "/run/isola/ctrl.sock")
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			log.Printf("tabs: connected to runner ctrl socket")
			tabMu.Lock()
			ctrlConn = conn
			tabMu.Unlock()

			// Ask for the current state immediately. The viewer gate is
			// re-asserted too: the runner that just came up has no idea
			// whether anyone is watching.
			_, _ = conn.Write([]byte(`{"cmd":"get_tabs"}` + "\n"))
			syncProducerCapture(true)

			dec := json.NewDecoder(conn)
			for {
				var ev struct {
					Ev         string `json:"ev"`
					Stalled    bool   `json:"stalled"`
					Ms         int64  `json:"ms"`
					Recoveries int    `json:"recoveries"`
					// capture_stats
					EmptyTotal  int64 `json:"empty_total"`
					EmptyWindow int64 `json:"empty_window"`
					Fallback    bool  `json:"fallback"`
					LastPaintMS int64 `json:"last_paint_ms"`
					LastFrameMS int64 `json:"last_frame_ms"`
					LastSentMS  int64 `json:"last_sent_ms"`
					SentTotal   int64 `json:"sent_total"`
					DedupTotal  int64 `json:"dedup_total"`
					Tabs        []struct {
						ID     int    `json:"id"`
						URL    string `json:"url"`
						Title  string `json:"title"`
						Active bool   `json:"active"`
					} `json:"tabs"`
				}
				if err := dec.Decode(&ev); err != nil {
					break
				}
				if ev.Ev == "renderer_stall" {
					// The runner replaced a wedged renderer (or frames came back).
					// Relay to viewers so the UI can show it, and log it here: this
					// is the only place the stall is explained rather than merely
					// observed as "fps 1".
					log.Printf("renderer: runner reported stalled=%v ms=%d recoveries=%d", ev.Stalled, ev.Ms, ev.Recoveries)
					noteRunnerStall(ev.Stalled, ev.Ms, ev.Recoveries)
					continue
				}
				if ev.Ev == "capture_stats" {
					// Capture-path health: the runner counts empty paint images and
					// whether it had to fall back to screenshot-driven frames. Kept
					// as metrics only — the runner already tells viewers when the
					// fallback engages.
					noteCaptureStats(captureHealthReport{
						EmptyTotal:     ev.EmptyTotal,
						EmptyWindow:    ev.EmptyWindow,
						FallbackActive: ev.Fallback,
						LastPaintMS:    ev.LastPaintMS,
						LastFrameMS:    ev.LastFrameMS,
						LastSentMS:     ev.LastSentMS,
						SentTotal:      ev.SentTotal,
						DedupTotal:     ev.DedupTotal,
					})
					continue
				}
				if ev.Ev != "tabs" {
					continue
				}
				tabsOut := make([]tabInfo, 0, len(ev.Tabs))
				for _, t := range ev.Tabs {
					tabsOut = append(tabsOut, tabInfo{ID: t.ID, URL: t.URL, Title: t.Title, Active: t.Active})
				}
				applyTabsState(tabsOut)
			}

			tabMu.Lock()
			if ctrlConn == conn {
				ctrlConn = nil
			}
			tabMu.Unlock()
			conn.Close()
			log.Printf("tabs: ctrl socket lost, retrying in 1s")
			time.Sleep(time.Second)
		}
	}()
}
