package main

import (
	"fmt"
	"log"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	cdpModAlt   = 1
	cdpModCtrl  = 2
	cdpModMeta  = 4
	cdpModShift = 8
)

type cdpKeyDef struct {
	key  string
	code string
	vk   int
}

var cdpKeyDefs = map[string]cdpKeyDef{
	"Enter":      {key: "Enter", code: "Enter", vk: 13},
	"Return":     {key: "Enter", code: "Enter", vk: 13},
	"Escape":     {key: "Escape", code: "Escape", vk: 27},
	"Tab":        {key: "Tab", code: "Tab", vk: 9},
	"Backspace":  {key: "Backspace", code: "Backspace", vk: 8},
	"BackSpace":  {key: "Backspace", code: "Backspace", vk: 8},
	"Delete":     {key: "Delete", code: "Delete", vk: 46},
	"Insert":     {key: "Insert", code: "Insert", vk: 45},
	"Home":       {key: "Home", code: "Home", vk: 36},
	"End":        {key: "End", code: "End", vk: 35},
	"PageUp":     {key: "PageUp", code: "PageUp", vk: 33},
	"PageDown":   {key: "PageDown", code: "PageDown", vk: 34},
	"ArrowUp":    {key: "ArrowUp", code: "ArrowUp", vk: 38},
	"ArrowDown":  {key: "ArrowDown", code: "ArrowDown", vk: 40},
	"ArrowLeft":  {key: "ArrowLeft", code: "ArrowLeft", vk: 37},
	"ArrowRight": {key: "ArrowRight", code: "ArrowRight", vk: 39},
	"Up":         {key: "ArrowUp", code: "ArrowUp", vk: 38},
	"Down":       {key: "ArrowDown", code: "ArrowDown", vk: 40},
	"Left":       {key: "ArrowLeft", code: "ArrowLeft", vk: 37},
	"Right":      {key: "ArrowRight", code: "ArrowRight", vk: 39},
	" ":          {key: " ", code: "Space", vk: 32},
	"space":      {key: " ", code: "Space", vk: 32},
	"Shift":      {key: "Shift", code: "ShiftLeft", vk: 16},
	"Shift_L":    {key: "Shift", code: "ShiftLeft", vk: 16},
	"Control":    {key: "Control", code: "ControlLeft", vk: 17},
	"Control_L":  {key: "Control", code: "ControlLeft", vk: 17},
	"Alt":        {key: "Alt", code: "AltLeft", vk: 18},
	"Alt_L":      {key: "Alt", code: "AltLeft", vk: 18},
	"Alt_R":      {key: "Alt", code: "AltRight", vk: 18},
	"F1":         {key: "F1", code: "F1", vk: 112},
	"F2":         {key: "F2", code: "F2", vk: 113},
	"F3":         {key: "F3", code: "F3", vk: 114},
	"F4":         {key: "F4", code: "F4", vk: 115},
	"F5":         {key: "F5", code: "F5", vk: 116},
	"F6":         {key: "F6", code: "F6", vk: 117},
	"F7":         {key: "F7", code: "F7", vk: 118},
	"F8":         {key: "F8", code: "F8", vk: 119},
	"F9":         {key: "F9", code: "F9", vk: 120},
	"F10":        {key: "F10", code: "F10", vk: 121},
	"F11":        {key: "F11", code: "F11", vk: 122},
	"F12":        {key: "F12", code: "F12", vk: 123},
}

func cdpButtonName(btn string) string {
	switch btn {
	case "middle":
		return "middle"
	case "right":
		return "right"
	default:
		return "left"
	}
}

func parseChordModifiers(parts []string) (mods int, keyToken string) {
	for _, p := range parts {
		switch strings.ToLower(p) {
		case "ctrl", "control":
			mods |= cdpModCtrl
		case "shift":
			mods |= cdpModShift
		case "alt":
			mods |= cdpModAlt
		case "meta", "super":
			mods |= cdpModMeta
		default:
			keyToken = p
		}
	}
	return mods, keyToken
}

func cdpKeyDefForToken(token string) (cdpKeyDef, bool) {
	if def, ok := cdpKeyDefs[token]; ok {
		return def, true
	}
	lower := strings.ToLower(token)
	switch lower {
	case "shift":
		return cdpKeyDefs["Shift"], true
	case "ctrl", "control":
		return cdpKeyDefs["Control"], true
	case "alt":
		return cdpKeyDefs["Alt"], true
	}
	if runes := []rune(token); len(runes) == 1 {
		ch := runes[0]
		switch {
		case ch >= 'a' && ch <= 'z':
			return cdpKeyDef{key: token, code: "Key" + strings.ToUpper(token), vk: int(unicode.ToUpper(ch))}, true
		case ch >= 'A' && ch <= 'Z':
			return cdpKeyDef{key: token, code: "Key" + token, vk: int(ch)}, true
		case ch >= '0' && ch <= '9':
			return cdpKeyDef{key: token, code: "Digit" + token, vk: int(ch)}, true
		}
		if def, ok := asciiPunctKeys[token]; ok {
			// Punctuation maps to the PHYSICAL key ("!" is Shift+Digit1),
			// not the character: Chromium resolves insertion from `text`,
			// while code/vk decide which key the page sees held down.
			return cdpKeyDef{key: token, code: def.code, vk: def.vk}, true
		}
		if ch < utf8.RuneSelf {
			return cdpKeyDef{key: token, code: "", vk: int(ch)}, true
		}
		// Non-ASCII printable (é, ß, €): no physical code to report; the
		// character rides insertText instead of a key event.
		return cdpKeyDef{key: token, code: "", vk: 0}, true
	}
	if mapped := mapKey(token); mapped != "" {
		if def, ok := cdpKeyDefs[mapped]; ok {
			return def, true
		}
		if def, ok := cdpKeyDefs[token]; ok {
			return def, true
		}
	}
	return cdpKeyDef{}, false
}

func cdpButtonsMask(button, eventType string) int {
	if eventType == "mouseReleased" {
		return 0
	}
	switch cdpButtonName(button) {
	case "right":
		return 2
	case "middle":
		return 4
	default:
		return 1
	}
}

// dispatchMouseMove: `buttons` is the live bitmask of depressed buttons —
// without it Chromium treats every move as hover and drag-selection never
// starts. The `button` field must also carry the held button ("left")
// during a drag: "none" makes Chromium treat the move as hover even with
// buttons set.
func (c *CDPClient) dispatchMouseMove(x, y, buttons int) error {
	c.setMousePosition(x, y)
	return c.send("Input.dispatchMouseEvent", mouseMovedParams(x, y, buttons))
}

func (c *CDPClient) dispatchMouseButton(eventType, button string, x, y int) error {
	if x >= 0 && y >= 0 {
		c.setMousePosition(x, y)
	} else {
		fx, fy := c.mousePosition()
		x, y = int(fx), int(fy)
	}
	return c.send("Input.dispatchMouseEvent", mouseButtonParams(eventType, button, x, y))
}

func (c *CDPClient) dispatchMouseWheel(deltaX, deltaY, x, y int) error {
	if x >= 0 && y >= 0 {
		c.setMousePosition(x, y)
	} else {
		fx, fy := c.mousePosition()
		x, y = int(fx), int(fy)
	}
	return c.send("Input.dispatchMouseEvent", mouseWheelParams(deltaX, deltaY, x, y))
}

// dispatchKeyEvent sends a key event. text is the character the key would
// insert ("a", "A", " ", "\r" for Enter) — WITHOUT it, Chromium processes
// the key (accelerators like Ctrl+A work) but generates NO character
// input, so typing into any field silently does nothing. keyUp never
// carries text.
func (c *CDPClient) dispatchKeyEvent(eventType string, def cdpKeyDef, modifiers int, text string) error {
	params := map[string]interface{}{
		"type":                  eventType,
		"key":                   def.key,
		"windowsVirtualKeyCode": def.vk,
		"nativeVirtualKeyCode":  def.vk,
		"modifiers":             modifiers,
	}
	if def.code != "" {
		params["code"] = def.code
	}
	if text != "" && eventType == "keyDown" {
		params["text"] = text
		params["unmodifiedText"] = strings.ToLower(text)
	}
	return c.sendKeys("Input.dispatchKeyEvent", params)
}

// chordKeyText is the character a chord's key token types, if any. Tokens
// arrive as KeyboardEvent.key, so a single rune already reflects Shift
// ("!" for Shift+1, "A" for Shift+a).
func chordKeyText(token string) string {
	switch token {
	case "Return", "Enter":
		return "\r"
	case "space", " ":
		return " "
	}
	if utf8.RuneCountInString(token) == 1 {
		return token
	}
	return ""
}

func (c *CDPClient) dispatchChord(combo string) error {
	parts := splitChord(combo)
	mods, keyToken := parseChordModifiers(parts)
	if keyToken == "" {
		if len(parts) == 1 {
			if def, ok := cdpKeyDefForToken(parts[0]); ok {
				if err := c.dispatchKeyEvent("keyDown", def, mods, ""); err != nil {
					return err
				}
				return c.dispatchKeyEvent("keyUp", def, mods, "")
			}
		}
		return nil
	}
	def, ok := cdpKeyDefForToken(keyToken)
	if !ok {
		log.Printf("input: unknown key %q in %q", keyToken, combo)
		return nil
	}
	// Printable keys carry their character as `text` — this is what makes
	// Chromium actually insert it.
	text := chordKeyText(keyToken)
	// A non-ASCII character has no physical key code; insert its text
	// directly. AltGr/Option symbols (€, ß) arrive this way.
	if def.vk == 0 && text != "" {
		return c.insertText(text)
	}
	if err := c.dispatchKeyEvent("keyDown", def, mods, text); err != nil {
		return err
	}
	return c.dispatchKeyEvent("keyUp", def, mods, "")
}

func (c *CDPClient) insertText(text string) error {
	return c.sendKeys("Input.insertText", map[string]interface{}{
		"text": text,
	})
}

// selectionContainsPoint reports whether a non-collapsed text selection
// exists and covers the given point (capture coordinates — same space as
// select_range's caretRangeFromPoint calls). Used to swallow synthetic
// right-clicks that would otherwise collapse that selection: unlike a
// native right-click, a CDP mousePressed doesn't preserve a selection it
// lands on, which broke the viewer's right-click Copy. Fail-open — any
// error or timeout forwards the press unchanged.
func (c *CDPClient) selectionContainsPoint(x, y int) bool {
	expr := fmt.Sprintf(`(function(){
		var sel = window.getSelection();
		if (!sel || sel.rangeCount === 0 || sel.isCollapsed) return "0";
		var p = document.caretRangeFromPoint(%d,%d);
		if (!p) return "0";
		return sel.getRangeAt(0).isPointInRange(p.startContainer, p.startOffset) ? "1" : "0";
	})()`, x, y)
	text, err := c.evaluateStringTimeout(expr, 150*time.Millisecond)
	return err == nil && text == "1"
}

// swallowRightPress checks-and-sets suppression for a right mousedown.
func (c *CDPClient) swallowRightPress(x, y int) bool {
	if !c.selectionContainsPoint(x, y) {
		return false
	}
	c.mu.Lock()
	c.suppressRightUp = true
	c.mu.Unlock()
	return true
}

func (c *CDPClient) swallowingRightUp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.suppressRightUp {
		c.suppressRightUp = false
		return true
	}
	return false
}

func (c *CDPClient) suppressingRightDrag(buttons int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.suppressRightUp && buttons&2 != 0 && buttons&1 == 0
}

func (c *CDPClient) handleInput(ev inputEvent) bool {
	switch ev.Type {
	case "mousemove":
		if c.suppressingRightDrag(ev.Buttons) {
			return true
		}
		if c.dispatchViaIframe(ev) {
			return true
		}
		return c.dispatchMouseMove(ev.X, ev.Y, ev.Buttons) == nil
	case "mousedown":
		if c.dispatchViaIframe(ev) {
			return true
		}
		// Right press inside the current selection: swallow it (and the
		// matching release) — dispatching it would collapse the selection
		// before the viewer's context-menu Copy can read it.
		if ev.Button == "right" && c.swallowRightPress(ev.X, ev.Y) {
			return true
		}
		return c.dispatchMouseButton("mousePressed", ev.Button, ev.X, ev.Y) == nil
	case "mouseup":
		if c.dispatchViaIframe(ev) {
			return true
		}
		if ev.Button == "right" && c.swallowingRightUp() {
			return true
		}
		return c.dispatchMouseButton("mouseReleased", ev.Button, ev.X, ev.Y) == nil
	case "mousewheel":
		if c.dispatchViaIframe(ev) {
			return true
		}
		return c.dispatchMouseWheel(ev.DeltaX, ev.DeltaY, ev.X, ev.Y) == nil
	case "chord":
		if ev.Combo == "" {
			return true
		}
		combo := translateChord(ev.Combo)
		if combo == "" {
			return true
		}
		if !c.iframeHasKeyFocus() {
			_ = c.ensureFocus()
		}
		return c.dispatchChord(combo) == nil
	case "type":
		if ev.Text == "" {
			return true
		}
		if !c.iframeHasKeyFocus() {
			_ = c.ensureFocus()
		}
		return c.insertText(ev.Text) == nil
	case "clipboard_set", "clipboard_sync":
		if ev.Text == "" {
			return true
		}
		return c.insertText(ev.Text) == nil
	default:
		return false
	}
}
