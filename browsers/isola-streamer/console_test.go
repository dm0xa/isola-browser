package main

import (
	"encoding/json"
	"testing"
)

// resetConsoleCapture snapshots and clears the console buffer and dedup set so a
// test starts clean and does not leak state into its siblings (mirrors
// resetNetCapture in cdp_capture_test.go).
func resetConsoleCapture() func() {
	consoleMu.Lock()
	prevBuffer, prevKeys, prevSeq := consoleBuffer, consoleSentKeys, consoleSeq
	consoleBuffer, consoleSentKeys, consoleSeq = nil, map[string]bool{}, 0
	consoleMu.Unlock()
	return func() {
		consoleMu.Lock()
		consoleBuffer, consoleSentKeys, consoleSeq = prevBuffer, prevKeys, prevSeq
		consoleMu.Unlock()
	}
}

func consoleEntriesSnapshot() []consoleEntry {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	out := make([]consoleEntry, len(consoleBuffer))
	copy(out, consoleBuffer)
	return out
}

// The console panel must relay a page's own console output — not only Electron's
// built-in security warning — with level, text and tab attribution intact. This
// is the ordinary path the viewer depends on when the panel otherwise looks
// empty because a page simply has not logged anything yet.
func TestConsoleTapCapturesPageConsole(t *testing.T) {
	defer resetConsoleCapture()()

	// Args arrive as raw JSON, so string values keep their surrounding quotes.
	// This matches what the viewer renders for e.g. console.log('x', 42).
	consoleTap("Runtime.consoleAPICalled",
		json.RawMessage(`{"type":"log","args":[{"value":"hello"},{"value":42}]}`), 2)

	got := consoleEntriesSnapshot()
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].Level != "log" || got[0].Text != `"hello" 42` || got[0].Tab != 2 {
		t.Fatalf("unexpected entry: %+v", got[0])
	}
	if got[0].TS == 0 {
		t.Fatalf("entry has no capture timestamp: %+v", got[0])
	}
}

// A console event with no explicit type still lands as "log", and thrown errors
// are captured as "exception" from the exception description.
func TestConsoleTapDefaultsLevelAndHandlesExceptions(t *testing.T) {
	defer resetConsoleCapture()()

	consoleTap("Runtime.consoleAPICalled", json.RawMessage(`{"args":[{"value":"no type"}]}`), 1)
	consoleTap("Runtime.exceptionThrown",
		json.RawMessage(`{"exceptionDetails":{"text":"Uncaught","description":"TypeError: boom"}}`), 1)

	got := consoleEntriesSnapshot()
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Level != "log" {
		t.Fatalf("missing type should default to log, got %q", got[0].Level)
	}
	if got[1].Level != "exception" || got[1].Text != "TypeError: boom" {
		t.Fatalf("exception entry wrong: %+v", got[1])
	}
}

// A repeat at the same millisecond on the same tab is the same event and is
// collapsed; the same text on a different tab, or at a later millisecond, is a
// distinct line and must not be dropped.
func TestConsoleTapDedupsPerTab(t *testing.T) {
	defer resetConsoleCapture()()
	oldNow := consoleNow
	consoleNow = func() int64 { return 1000 }
	defer func() { consoleNow = oldNow }()

	params := json.RawMessage(`{"type":"warning","args":[{"value":"same"}]}`)
	consoleTap("Runtime.consoleAPICalled", params, 1)
	consoleTap("Runtime.consoleAPICalled", params, 1) // same ms, same tab → dropped
	consoleTap("Runtime.consoleAPICalled", params, 2) // other tab → kept

	got := consoleEntriesSnapshot()
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2 (same-ms repeat deduped, other tab kept)", len(got))
	}
	if got[0].Tab != 1 || got[1].Tab != 2 {
		t.Fatalf("tab attribution wrong: %+v", got)
	}

	// A genuine repeat later in the session is a new line, not a duplicate.
	consoleNow = func() int64 { return 1001 }
	consoleTap("Runtime.consoleAPICalled", params, 1)
	if got := consoleEntriesSnapshot(); len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (later repeat must be kept)", len(got))
	}
}

// A re-published line keeps its id, so the viewer folds the corrected tab into
// the row instead of appending a second copy.
func TestConsoleRepublishKeepsLineID(t *testing.T) {
	defer resetConsoleCapture()()
	defer snapshotTabState()()

	oldNow := consoleNow
	consoleNow = func() int64 { return 7 }
	defer func() { consoleNow = oldNow }()

	got, unregister := collectBroadcasts("console")
	defer unregister()

	consoleTap("Runtime.consoleAPICalled", json.RawMessage(`{"type":"log","args":[{"value":"hello"}]}`), unknownTab)
	reassignUnknownTab(3)

	if len(*got) != 2 {
		t.Fatalf("broadcasts = %d, want 2 (original + republish)", len(*got))
	}
	first, _ := (*got)[0]["i"].(int64)
	second, _ := (*got)[1]["i"].(int64)
	if first != second {
		t.Fatalf("line id changed on republish: %v -> %v", first, second)
	}
	if (*got)[1]["tb"] != 3 {
		t.Fatalf("republished tab = %v, want 3", (*got)[1]["tb"])
	}
}

// consoleTap must ignore CDP methods it does not own rather than buffering junk.
func TestConsoleTapIgnoresUnrelatedMethods(t *testing.T) {
	defer resetConsoleCapture()()

	consoleTap("Network.requestWillBeSent", json.RawMessage(`{"requestId":"1"}`), 1)
	if got := consoleEntriesSnapshot(); len(got) != 0 {
		t.Fatalf("non-console method produced %d entries", len(got))
	}
}
