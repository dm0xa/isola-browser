package main

import (
	"testing"
)

// snapshotTabState keeps the package-level tab state a test mutates out of its
// siblings, which all share one process.
func snapshotTabState() func() {
	tabMu.Lock()
	prevActive, prevTabs, prevMsg := activeTabID, runnerTabs, lastTabsMsg
	tabMu.Unlock()
	prevClient := cdpClient
	cdpClient = nil
	return func() {
		tabMu.Lock()
		activeTabID, runnerTabs, lastTabsMsg = prevActive, prevTabs, prevMsg
		tabMu.Unlock()
		cdpClient = prevClient
	}
}

// collectBroadcasts registers a viewer listener and returns the messages it saw
// of one type, filtering out the tab-state replay registerTabListener performs.
func collectBroadcasts(msgType string) (*[]map[string]any, func()) {
	var got []map[string]any
	unregister := registerTabListener(func(m map[string]any) {
		if m["type"] == msgType {
			got = append(got, m)
		}
	})
	return &got, unregister
}

// Traffic captured before the runner reports its tab strip carries unknownTab.
// Left alone it renders as a phantom second tab in the panel's filter, named
// after its busiest host.
func TestReassignUnknownTabRewritesPreStripNetEntries(t *testing.T) {
	defer resetNetCapture()()

	netMu.Lock()
	netRequests["a|1"] = &netRequest{id: "a:1", url: "https://duckduckgo.com/", tab: unknownTab}
	netRequests["a|2"] = &netRequest{id: "a:2", url: "https://www.unibet.se/", tab: 2}
	netOrder = []string{"a|1", "a|2"}
	netMu.Unlock()

	got, unregister := collectBroadcasts("net")
	defer unregister()

	reassignUnknownTab(2)

	netMu.Lock()
	first, second := netRequests["a|1"].tab, netRequests["a|2"].tab
	netMu.Unlock()
	if first != 2 || second != 2 {
		t.Fatalf("tabs after rewrite = %d, %d; want 2, 2", first, second)
	}

	// Only the rewritten row is re-published — the viewer merges by id, and a
	// spurious re-send of every other entry would be wasted traffic.
	if len(*got) != 1 {
		t.Fatalf("re-published %d entries, want 1", len(*got))
	}
	entry, _ := (*got)[0]["e"].(map[string]any)
	if entry["id"] != "a:1" || entry["tb"] != 2 {
		t.Fatalf("re-published entry = %v, want id a:1 with tb 2", entry)
	}
}

func TestReassignUnknownTabRewritesConsoleLines(t *testing.T) {
	defer resetConsoleCapture()()

	consoleMu.Lock()
	// The tap keeps a string argument's surrounding JSON quotes, so the seeded
	// text carries them too.
	consoleBuffer = []consoleEntry{
		{Level: "log", Text: `"from start page"`, Tab: unknownTab},
		{Level: "log", Text: `"already attributed"`, Tab: 2},
	}
	consoleSentKeys = map[string]bool{consoleKey(consoleBuffer[0]): true}
	consoleMu.Unlock()

	got, unregister := collectBroadcasts("console")
	defer unregister()

	reassignUnknownTab(2)

	lines := consoleEntriesSnapshot()
	if len(lines) != 2 || lines[0].Tab != 2 || lines[1].Tab != 2 {
		t.Fatalf("console tabs after rewrite = %+v, want both 2", lines)
	}
	if len(*got) != 1 {
		t.Fatalf("re-published %d console lines, want 1", len(*got))
	}
	if (*got)[0]["tb"] != 2 {
		t.Fatalf("re-published line tb = %v, want 2", (*got)[0]["tb"])
	}

	// The dedupe key embeds the tab, so the old key must be gone and the
	// rewritten line must stay deduped under its new id.
	consoleMu.Lock()
	_, oldKey := consoleSentKeys[consoleKey(consoleEntry{Level: "log", Text: `"from start page"`, Tab: unknownTab})]
	_, newKey := consoleSentKeys[consoleKey(consoleEntry{Level: "log", Text: `"from start page"`, Tab: 2})]
	consoleMu.Unlock()
	if oldKey || !newKey {
		t.Fatalf("dedupe keys after rewrite: old=%v new=%v, want false/true", oldKey, newKey)
	}

	// A duplicate event with the same timestamp is dropped; a later repeat is a
	// new line (the tap must not swallow legitimate repeats).
	oldNow := consoleNow
	consoleNow = func() int64 { return 0 }
	defer func() { consoleNow = oldNow }()
	consoleTap(`Runtime.consoleAPICalled`, []byte(`{"type":"log","args":[{"value":"from start page"}]}`), 2)
	if lines := consoleEntriesSnapshot(); len(lines) != 2 {
		t.Fatalf("buffer grew to %d entries; same-ms rewritten line was not deduped", len(lines))
	}
	consoleNow = func() int64 { return 1 }
	consoleTap(`Runtime.consoleAPICalled`, []byte(`{"type":"log","args":[{"value":"from start page"}]}`), 2)
	if lines := consoleEntriesSnapshot(); len(lines) != 3 {
		t.Fatalf("buffer has %d entries; later repeat was dropped", len(lines))
	}
}

// The backfill runs once, when attribution becomes possible — not on every tab
// broadcast (navigation and title changes also produce one).
func TestApplyTabsStateBackfillsOnlyOnFirstStrip(t *testing.T) {
	defer resetNetCapture()()
	defer snapshotTabState()()

	tabMu.Lock()
	activeTabID, runnerTabs, lastTabsMsg = unknownTab, nil, nil
	tabMu.Unlock()

	netMu.Lock()
	netRequests["a|1"] = &netRequest{id: "a:1", tab: unknownTab}
	netOrder = []string{"a|1"}
	netMu.Unlock()

	applyTabsState([]tabInfo{{ID: 1, URL: "https://www.unibet.se/", Active: true}})

	tabMu.Lock()
	active, strip := activeTabID, append([]tabInfo(nil), runnerTabs...)
	tabMu.Unlock()
	if active != 1 || len(strip) != 1 {
		t.Fatalf("tab state = %d / %+v, want 1 / one tab", active, strip)
	}
	netMu.Lock()
	backfilled := netRequests["a|1"].tab
	netMu.Unlock()
	if backfilled != 1 {
		t.Fatalf("pre-strip entry tab = %d, want 1", backfilled)
	}

	// A later strip must leave an unknown-id row alone: attribution is possible
	// from that point on, so such a row is not ours to rewrite.
	netMu.Lock()
	netRequests["a|2"] = &netRequest{id: "a:2", tab: unknownTab}
	netOrder = append(netOrder, "a|2")
	netMu.Unlock()
	applyTabsState([]tabInfo{{ID: 1, URL: "https://www.unibet.se/", Title: "unibet", Active: true}})

	netMu.Lock()
	untouched := netRequests["a|2"].tab
	netMu.Unlock()
	if untouched != unknownTab {
		t.Fatalf("entry added after the first strip was rewritten to %d", untouched)
	}
}

// A strip can arrive with no active tab while the runner is switching. That
// must not regress attribution to -1 (which re-opens the phantom tab and
// re-arms the backfill).
func TestApplyTabsStateKeepsAttributionWhenStripHasNoActiveTab(t *testing.T) {
	defer resetNetCapture()()
	defer snapshotTabState()()

	tabMu.Lock()
	activeTabID, runnerTabs, lastTabsMsg = unknownTab, nil, nil
	tabMu.Unlock()

	applyTabsState([]tabInfo{{ID: 1, URL: "https://a.example/", Active: true}})
	applyTabsState([]tabInfo{{ID: 1, URL: "https://a.example/"}}) // switching, no active tab

	tabMu.Lock()
	active := activeTabID
	tabMu.Unlock()
	if active != 1 {
		t.Fatalf("active tab regressed to %d, want 1", active)
	}

	// Attribution already happened, so a later unknown-id row is not rewritten
	// by the next strip that does have an active tab.
	netMu.Lock()
	netRequests["late|1"] = &netRequest{id: "late:1", tab: unknownTab}
	netOrder = append(netOrder, "late|1")
	netMu.Unlock()
	applyTabsState([]tabInfo{{ID: 1, URL: "https://a.example/", Active: true}})
	netMu.Lock()
	late := netRequests["late|1"].tab
	netMu.Unlock()
	if late != unknownTab {
		t.Fatalf("backfill ran twice: late entry rewritten to %d", late)
	}
}
