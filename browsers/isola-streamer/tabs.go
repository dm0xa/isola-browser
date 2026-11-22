package main

import (
	"encoding/json"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Tab control: viewers send tab commands over the input WS, this relays them to
// the runner's ctrl UDS, and "tabs" broadcasts flow back out. On an active-tab
// change the CDP input client re-points at the new page target.

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

// currentTabID: capture events are attributed to whichever tab was active when they arrived.
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

// tabForTarget: match a CDP target to a runner tab by URL (exact, then prefix, title
// tiebreak). Indistinguishable or unmatched targets fall back to the active tab.
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
					log.Printf("input: listener panic: %v", r)
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
	previous := activeTabID
	// Capture starts before this socket is up, so anything captured until now
	// carries the "no tab known yet" id (see reassignUnknownTab).
	wasUnknown := previous < 0
	// A strip may briefly carry no active tab (the runner is switching tabs).
	// Assigning -1 would re-arm the backfill and re-open the unknown bucket, so
	// keep the last known attribution until a real tab replaces it.
	if newActive > 0 {
		activeTabID = newActive
	}
	// `switched` only means the strip named a different tab; an empty strip
	// must not re-point input.
	switched := newActive > 0 && newActive != previous
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

// reassignUnknownTab: the START_URL load is captured before any tab id exists
// (activeTabID starts at -1); fold that bucket into the active tab. Re-publish
// merges by entry id, so rows are corrected, not duplicated.
func reassignUnknownTab(tab int) {
	if tab <= 0 {
		return
	}

	var msgs []map[string]any
	netMu.Lock()
	for _, r := range netRequests {
		if r.tab == unknownTab {
			r.tab = tab
			// Serialise while the lock is held; see netWireMessage.
			msgs = append(msgs, netWireMessage(r))
		}
	}
	netMu.Unlock()
	for _, msg := range msgs {
		broadcastNetMessage(msg)
	}

	var lines []consoleEntry
	consoleMu.Lock()
	for i := range consoleBuffer {
		if consoleBuffer[i].Tab != unknownTab {
			continue
		}
		// The dedupe key embeds the tab, so drop the old key and carry the
		// rewritten entry over; otherwise the same line logged again would be
		// added a second time and the old key would leak.
		delete(consoleSentKeys, consoleKey(consoleBuffer[i]))
		consoleBuffer[i].Tab = tab
		consoleSentKeys[consoleKey(consoleBuffer[i])] = true
		lines = append(lines, consoleBuffer[i])
	}
	consoleMu.Unlock()
	for _, e := range lines {
		broadcastToInputWS(consoleWire(e))
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
	if !writeCtrlCommand(cmd) {
		log.Printf("tabs: no ctrl socket, dropped")
	}
}

// writeCtrlCommand: reports whether the socket was connected — convergence-critical
// callers re-send on reconnect (see startTabCtrl).
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
		log.Printf("tabs: ctrl write: %v", err)
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
			log.Printf("tabs: ctrl connected")
			tabMu.Lock()
			ctrlConn = conn
			tabMu.Unlock()

			// Ask for the current state immediately. The viewer gate is
			// re-asserted too: the runner that just came up has no idea
			// whether anyone is watching.
			_, _ = conn.Write([]byte(`{"cmd":"get_tabs"}` + "\n"))
			// A viewer that attached before this dial got its resize dropped;
			// deliver it now, or every encoder will wait for a geometry the
			// surface never reaches.
			flushPendingResize()
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
					// The only place a renderer stall is explained rather than observed as "fps 1".
					log.Printf("renderer: runner reported stalled=%v ms=%d recoveries=%d", ev.Stalled, ev.Ms, ev.Recoveries)
					noteRunnerStall(ev.Stalled, ev.Ms, ev.Recoveries)
					continue
				}
				if ev.Ev == "capture_stats" {
					// Capture-path health; metrics only — the runner tells viewers when fallback engages.
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
			log.Printf("tabs: ctrl lost, retry 1s")
			time.Sleep(time.Second)
		}
	}()
}
