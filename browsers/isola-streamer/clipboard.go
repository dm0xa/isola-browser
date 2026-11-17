package main

import (
	"log"
	"strings"
	"time"
)

// Sandbox → host clipboard bridge for the isola (Electron/CDP) transport.
//
// Ctrl+C / Ctrl+X run correctly inside the sandbox — the renderer fires the
// copy/cut command and Electron updates the sandbox clipboard — but without
// this producer nothing read it back, so the text never reached the host
// clipboard. From the user's seat that reads as "Ctrl+C doesn't copy,
// Ctrl+X doesn't cut".
//
// The text is read back through the page (navigator.clipboard.readText over
// CDP), not from a separate clipboard tool: no extra binaries in the image, it
// reflects exactly what the renderer just copied, and it reuses the same
// Runtime.evaluate channel get_selection already uses.
const (
	clipboardReadExpr = `(function(){
		if (!navigator.clipboard || !navigator.clipboard.readText) return "";
		return navigator.clipboard.readText();
	})()`
	// Short: this runs on a worker per copy/cut, and a page that never
	// resolves must not pile up goroutines.
	clipboardReadTimeout = 700 * time.Millisecond
)

// chordWritesClipboard reports whether a chord performs a copy or cut in the
// remote browser, i.e. whether the sandbox clipboard changes as a result and
// therefore needs mirroring to the host.
//
// Ctrl/Cmd+C and Ctrl/Cmd+X only. Paste (Ctrl/Cmd+V) travels the other way
// (viewer → sandbox, handled in cdp_input.go), and the chords that merely look
// similar are not clipboard writes: Ctrl+Shift+C is DevTools/inspect,
// Ctrl+Alt+C and Ctrl+Insert are undocumented in Chromium.
func chordWritesClipboard(combo string) bool {
	mods, keyToken := parseChordModifiers(strings.Split(combo, "+"))
	if mods&(cdpModCtrl|cdpModMeta) == 0 {
		return false
	}
	if mods&(cdpModShift|cdpModAlt) != 0 {
		return false
	}
	switch strings.ToLower(keyToken) {
	case "c", "x":
		return true
	}
	return false
}

// readSandboxClipboard reads the sandbox clipboard text.
//
// navigator.clipboard only exists in a secure context, so a plain http:// page
// (or one that refused the clipboard-read permission) yields "" here instead of
// an error — Ctrl+C on such a page then stays a silent no-op rather than
// logging on every keystroke.
func readSandboxClipboard() (string, error) {
	if cdpClient == nil || !cdpClient.Available() {
		return "", nil
	}
	return cdpClient.evaluateStringAwaitTimeout(clipboardReadExpr, clipboardReadTimeout)
}

// syncSandboxClipboard mirrors the sandbox clipboard onto the requesting
// viewer, which writes it to the host clipboard (see useIsolaStream's
// clipboard_sync handler). Call it after the copy/cut chord has been
// dispatched: CDP answers Input.dispatchKeyEvent only once the renderer has
// handled the key, so the clipboard already holds the new text.
func syncSandboxClipboard(writeJSON func(map[string]any)) {
	text, err := readSandboxClipboard()
	if err != nil {
		log.Printf("clipboard: read: %v", err)
		return
	}
	if text == "" {
		return
	}
	writeJSON(map[string]any{"type": "clipboard_sync", "text": text})
}
