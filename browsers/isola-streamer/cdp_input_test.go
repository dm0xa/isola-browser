package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/gorilla/websocket"
)

// resolveChord runs the same steps dispatchChord does, without a CDP socket.
func resolveChord(combo string) (int, string, cdpKeyDef, bool) {
	parts := splitChord(translateChord(combo))
	mods, token := parseChordModifiers(parts)
	def, ok := cdpKeyDefForToken(token)
	return mods, token, def, ok
}

// Every printable ASCII character the viewer can send (KeyboardEvent.key)
// must resolve to a key definition that types that exact character. This is
// the regression that silently dropped "!@#,.?" from the sandbox: translateChord
// turned "," into the key name "comma", which cdpKeyDefForToken could not map
// back, so the event was logged as unknown and discarded.
func TestEveryPrintableASCIIResolves(t *testing.T) {
	t.Parallel()
	for r := rune(33); r <= 126; r++ {
		ch := string(r)
		combo := ch
		// "+" is both a key and the chord separator.
		if ch == "+" {
			combo = "shift++"
		}
		_, token, def, ok := resolveChord(combo)
		if !ok {
			t.Errorf("%q (combo %q): unresolved key token %q", ch, combo, token)
			continue
		}
		if def.key != ch {
			t.Errorf("%q (combo %q): key = %q, want %q", ch, combo, def.key, ch)
		}
		if got := chordKeyText(token); got != ch {
			t.Errorf("%q (combo %q): text = %q, want %q", ch, combo, got, ch)
		}
	}
	for _, combo := range []string{" ", "ctrl+ "} {
		_, token, def, ok := resolveChord(combo)
		if !ok || def.key != " " || chordKeyText(token) != " " {
			t.Errorf("%q: resolved=%v key=%q text=%q", combo, ok, def.key, chordKeyText(token))
		}
	}
}

func TestPunctuationUsesPhysicalKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		combo string
		key   string
		code  string
		vk    int
	}{
		{"shift+!", "!", "Digit1", 49},
		{"shift+@", "@", "Digit2", 50},
		{"shift+#", "#", "Digit3", 51},
		{",", ",", "Comma", 188},
		{".", ".", "Period", 190},
		{"shift+?", "?", "Slash", 191},
		{"shift++", "+", "Equal", 187},
		{"shift+_", "_", "Minus", 189},
		{"shift+$", "$", "Digit4", 52},
	}
	for _, tc := range cases {
		_, _, def, ok := resolveChord(tc.combo)
		if !ok {
			t.Errorf("%q: unresolved", tc.combo)
			continue
		}
		if def.key != tc.key || def.code != tc.code || def.vk != tc.vk {
			t.Errorf("%q: got key=%q code=%q vk=%d, want key=%q code=%q vk=%d",
				tc.combo, def.key, def.code, def.vk, tc.key, tc.code, tc.vk)
		}
	}
}

func TestNonASCIISymbolIsTyped(t *testing.T) {
	t.Parallel()
	// Option/AltGr symbols arrive as KeyboardEvent.key with no physical key
	// on a US layout; they must reach the page via insertText.
	for _, combo := range []string{"€", "alt+€", "ß", "alt+ß"} {
		_, token, def, ok := resolveChord(combo)
		if !ok {
			t.Errorf("%q: unresolved", combo)
			continue
		}
		if def.vk != 0 {
			t.Errorf("%q: vk = %d, want 0 (no physical key)", combo, def.vk)
		}
		if got := chordKeyText(token); got != token {
			t.Errorf("%q: text = %q, want %q", combo, got, token)
		}
	}
}

func TestSplitChordKeepsLiteralPlus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		combo string
		want  []string
	}{
		{"a", []string{"a"}},
		{"ctrl+a", []string{"ctrl", "a"}},
		{"+", []string{"", "+"}},
		{"shift++", []string{"shift", "+"}},
		{"ctrl+shift++", []string{"ctrl", "shift", "+"}},
	}
	for _, tc := range cases {
		got := splitChord(tc.combo)
		if len(got) != len(tc.want) {
			t.Errorf("splitChord(%q) = %q, want %q", tc.combo, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitChord(%q) = %q, want %q", tc.combo, got, tc.want)
				break
			}
		}
	}
}

// --- dispatch-level tests: the viewer's wire messages → CDP commands ---------

type capturedCommand struct {
	Method string
	Params map[string]any
}

type cdpCapture struct {
	mu   sync.Mutex
	cmds []capturedCommand
}

func (c *cdpCapture) snapshot() []capturedCommand {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedCommand(nil), c.cmds...)
}

// newCaptureCDP connects a CDPClient to a fake page target that records every
// command instead of answering it. handleInput only writes, so the test sees
// exactly what would reach Chromium.
func newCaptureCDP(t *testing.T) (*CDPClient, *cdpCapture) {
	t.Helper()
	capture := &cdpCapture{}
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var msg struct {
				Method string         `json:"method"`
				Params map[string]any `json:"params"`
			}
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			capture.mu.Lock()
			capture.cmds = append(capture.cmds, capturedCommand{Method: msg.Method, Params: msg.Params})
			capture.mu.Unlock()
		}
	}))
	t.Cleanup(srv.Close)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial fake CDP: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	c := newCDPClient()
	c.mu.Lock()
	c.conn = conn
	c.connected = true
	c.mu.Unlock()
	return c, capture
}

func inputCommands(cmds []capturedCommand) []capturedCommand {
	out := make([]capturedCommand, 0, len(cmds))
	for _, cmd := range cmds {
		switch cmd.Method {
		case "Input.dispatchKeyEvent", "Input.insertText":
			out = append(out, cmd)
		}
	}
	return out
}

func waitInputCommands(t *testing.T, capture *cdpCapture, want int) []capturedCommand {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := inputCommands(capture.snapshot())
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("captured %d input commands, want >= %d: %+v", len(got), want, got)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// wireText is the characters a real Chromium would insert, in order: keyDown
// text plus insertText payloads.
func wireText(cmds []capturedCommand) string {
	var b strings.Builder
	for _, cmd := range cmds {
		switch cmd.Method {
		case "Input.dispatchKeyEvent":
			if cmd.Params["type"] == "keyDown" {
				if text, ok := cmd.Params["text"].(string); ok {
					b.WriteString(text)
				}
			}
		case "Input.insertText":
			if text, ok := cmd.Params["text"].(string); ok {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

// shiftedASCII is what a US layout types with Shift held.
const shiftedASCII = "ABCDEFGHIJKLMNOPQRSTUVWXYZ!@#$%^&*()_+{}|:\"<>?~"

// viewerChord is the combo the bundled viewer sends for one typed character.
func viewerChord(r rune) string {
	ch := string(r)
	if strings.ContainsRune(shiftedASCII, r) {
		return "shift+" + ch
	}
	return ch
}

func expectedInputCommands(text string) int {
	n := 0
	for _, r := range text {
		if r > unicode.MaxASCII {
			n++ // Input.insertText, no key pair
		} else {
			n += 2 // keyDown + keyUp
		}
	}
	return n
}

// The user-facing contract: everything a person types into a sandbox form
// arrives in the page exactly as typed. Covers the strings that broke — email
// addresses, passwords with Shift symbols, paths with backslashes, prose
// punctuation, and non-ASCII text.
func TestInputDispatchTypesUserStrings(t *testing.T) {
	t.Parallel()
	c, capture := newCaptureCDP(t)

	typed := "user@example.com P@ssw0rd!2026 cafe_2026-11-24 " +
		`C:\Users\me a+b=c, d.e; f:g? "quoted" [brackets] {braces} tag#1 ` +
		"café €5 100% ~/dir <tag> | pipe"

	for _, r := range typed {
		combo := viewerChord(r)
		if !c.handleInput(inputEvent{Type: "chord", Combo: combo}) {
			t.Fatalf("handleInput rejected %q while typing %q", combo, string(r))
		}
	}

	cmds := waitInputCommands(t, capture, expectedInputCommands(typed))
	if got := wireText(cmds); got != typed {
		t.Fatalf("typed text = %q, want %q", got, typed)
	}

	// Every keyDown is paired with a keyUp, or a held key would stay down.
	downs, ups := 0, 0
	for _, cmd := range cmds {
		if cmd.Method != "Input.dispatchKeyEvent" {
			continue
		}
		switch cmd.Params["type"] {
		case "keyDown":
			downs++
		case "keyUp":
			ups++
		}
	}
	if downs != ups {
		t.Fatalf("dispatched %d keyDown vs %d keyUp", downs, ups)
	}

	// The shifted symbols must carry the physical key, not just the character:
	// pages branch on KeyboardEvent.code and on Shift.
	for _, tc := range []struct {
		key  string
		code string
		mods int
	}{
		{"!", "Digit1", 8},
		{"@", "Digit2", 8},
		{"#", "Digit3", 8},
		{"+", "Equal", 8},
		{"%", "Digit5", 8},
		{",", "Comma", 0},
		{".", "Period", 0},
		{"?", "Slash", 8},
		{`\`, "Backslash", 0},
	} {
		cmd, ok := firstKeyDown(cmds, tc.key)
		if !ok {
			t.Errorf("no keyDown for %q", tc.key)
			continue
		}
		if cmd.Params["code"] != tc.code {
			t.Errorf("key %q code = %v, want %q", tc.key, cmd.Params["code"], tc.code)
		}
		if mods := int(cmd.Params["modifiers"].(float64)); mods != tc.mods {
			t.Errorf("key %q modifiers = %d, want %d", tc.key, mods, tc.mods)
		}
	}
}

// chordDef resolves the key definition dispatchChord will use for a combo.
func chordDef(combo string) (cdpKeyDef, bool) {
	_, token := parseChordModifiers(splitChord(translateChord(combo)))
	return cdpKeyDefForToken(token)
}

// firstKeyDown finds the keyDown for a key token.
func firstKeyDown(cmds []capturedCommand, key string) (capturedCommand, bool) {
	for _, cmd := range cmds {
		if cmd.Method == "Input.dispatchKeyEvent" && cmd.Params["type"] == "keyDown" && cmd.Params["key"] == key {
			return cmd, true
		}
	}
	return capturedCommand{}, false
}

// Paste/autofill/API type events use Input.insertText, which must carry the
// whole string verbatim — including characters that need no key events.
func TestInputDispatchTypesTextVerbatim(t *testing.T) {
	t.Parallel()
	c, capture := newCaptureCDP(t)

	text := "user@example.com\tP@ss w0rd! €5"
	if !c.handleInput(inputEvent{Type: "type", Text: text}) {
		t.Fatal("handleInput rejected a type event")
	}
	cmds := waitInputCommands(t, capture, 1)
	if len(cmds) != 1 || cmds[0].Method != "Input.insertText" {
		t.Fatalf("expected one Input.insertText, got %+v", cmds)
	}
	if got := cmds[0].Params["text"]; got != text {
		t.Fatalf("insertText = %q, want %q", got, text)
	}
}

// Shortcuts must stay shortcuts: modifiers on the key, no inserted text.
func TestInputDispatchShortcutChords(t *testing.T) {
	t.Parallel()
	c, capture := newCaptureCDP(t)

	shortcuts := []string{"ctrl+a", "ctrl+c", "ctrl+v", "ctrl+z", "ctrl+shift+t", "alt+ArrowLeft"}
	for _, combo := range shortcuts {
		if !c.handleInput(inputEvent{Type: "chord", Combo: combo}) {
			t.Fatalf("handleInput rejected %q", combo)
		}
	}
	cmds := waitInputCommands(t, capture, 2*len(shortcuts))
	for _, cmd := range cmds {
		if cmd.Method == "Input.insertText" {
			t.Fatalf("shortcut inserted text: %+v", cmd)
		}
	}

	wantMods := map[string]int{
		"ctrl+a":        cdpModCtrl,
		"ctrl+c":        cdpModCtrl,
		"ctrl+v":        cdpModCtrl,
		"ctrl+z":        cdpModCtrl,
		"ctrl+shift+t":  cdpModCtrl | cdpModShift,
		"alt+ArrowLeft": cdpModAlt,
	}
	for _, combo := range shortcuts {
		def, ok := chordDef(combo)
		if !ok {
			t.Errorf("%q: unresolved", combo)
			continue
		}
		cmd, ok := firstKeyDown(cmds, def.key)
		if !ok {
			t.Errorf("%q: no keyDown for key %q", combo, def.key)
			continue
		}
		if cmd.Params["text"] != nil {
			t.Errorf("%q carries text %v; a shortcut must not type", combo, cmd.Params["text"])
			continue
		}
		if mods := int(cmd.Params["modifiers"].(float64)); mods != wantMods[combo] {
			t.Errorf("%q modifiers = %d, want %d", combo, mods, wantMods[combo])
		}
	}
}

// Editing and navigation keys are chords too; they must map to named keys
// without inventing text.
func TestInputDispatchEditingKeys(t *testing.T) {
	t.Parallel()
	c, capture := newCaptureCDP(t)

	wantText := map[string]string{
		"Backspace":  "",
		"Delete":     "",
		"Tab":        "",
		"ArrowLeft":  "",
		"ArrowRight": "",
		"Home":       "",
		"End":        "",
		"Enter":      "\r",
	}
	order := []string{"Backspace", "Delete", "Tab", "ArrowLeft", "ArrowRight", "Home", "End", "Enter"}
	for _, combo := range order {
		if !c.handleInput(inputEvent{Type: "chord", Combo: combo}) {
			t.Fatalf("handleInput rejected %q", combo)
		}
	}
	cmds := waitInputCommands(t, capture, 2*len(order))

	for _, combo := range order {
		def, ok := chordDef(combo)
		if !ok {
			t.Errorf("%q: unresolved", combo)
			continue
		}
		cmd, ok := firstKeyDown(cmds, def.key)
		if !ok {
			t.Errorf("%q: no keyDown for key %q", combo, def.key)
			continue
		}
		gotText, _ := cmd.Params["text"].(string)
		if gotText != wantText[combo] {
			t.Errorf("%q text = %q, want %q", combo, gotText, wantText[combo])
		}
		if cmd.Params["code"] == nil || cmd.Params["code"] == "" {
			t.Errorf("%q dispatched without a code", combo)
		}
	}
}

// An unknown key name must be ignored, never half-dispatched or panicking.
func TestInputDispatchIgnoresUnknownKey(t *testing.T) {
	t.Parallel()
	c, capture := newCaptureCDP(t)

	if !c.handleInput(inputEvent{Type: "chord", Combo: "ctrl+NotAKey"}) {
		t.Fatal("handleInput returned false for an unknown chord")
	}
	time.Sleep(50 * time.Millisecond)
	if cmds := inputCommands(capture.snapshot()); len(cmds) != 0 {
		t.Fatalf("unknown key dispatched commands: %+v", cmds)
	}
}
