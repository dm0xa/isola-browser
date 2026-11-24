package main

import (
	"strings"
	"unicode/utf8"
)

// splitChord splits a combo on "+" while keeping a literal "+" key intact:
// "shift++" is Shift and the plus key, not Shift and two empty tokens.
func splitChord(chord string) []string {
	parts := strings.Split(chord, "+")
	if len(parts) > 1 && parts[len(parts)-1] == "" {
		// Collapse the trailing run of empty segments into the "+" key.
		key := len(parts) - 1
		for key > 0 && parts[key] == "" {
			key--
		}
		parts = append(parts[:key+1], "+")
	}
	return parts
}

// translateChord normalizes chord key names (e.g. "Enter" -> "Return").
func translateChord(chord string) string {
	parts := splitChord(chord)
	for i, p := range parts {
		switch p {
		case "ctrl", "shift", "alt", "meta", "super", "hyper":
			continue
		}
		// A single rune IS the key: translating it to a key name ("," ->
		// "comma") leaves the resolver with a token it cannot map back, and
		// the character is silently dropped.
		if utf8.RuneCountInString(p) == 1 {
			continue
		}
		if mapped := mapKey(p); mapped != "" {
			parts[i] = mapped
		}
	}
	return strings.Join(parts, "+")
}

// mapKey translates UI key names to CDP/generic key names.
func mapKey(k string) string {
	switch k {
	case "Enter":
		return "Return"
	case "Escape":
		return "Escape"
	case "Tab":
		return "Tab"
	case "Backspace":
		return "BackSpace"
	case "Delete":
		return "Delete"
	case "Insert":
		return "Insert"
	case "Home":
		return "Home"
	case "End":
		return "End"
	case "PageUp":
		return "PageUp"
	case "PageDown":
		return "PageDown"
	case "ArrowUp":
		return "Up"
	case "ArrowDown":
		return "Down"
	case "ArrowLeft":
		return "Left"
	case "ArrowRight":
		return "Right"
	case "CapsLock":
		return "Caps_Lock"
	case "NumLock":
		return "Num_Lock"
	case "ScrollLock":
		return "Scroll_Lock"
	case "Shift":
		return "Shift_L"
	case "Control":
		return "Control_L"
	case "Alt":
		return "Alt_L"
	case "Meta":
		return ""
	case "AltGraph":
		return "Alt_R"
	case "OS":
		return ""
	case "F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8", "F9", "F10", "F11", "F12":
		return k
	case "NumpadDivide":
		return "KP_Divide"
	case "NumpadMultiply":
		return "KP_Multiply"
	case "NumpadSubtract":
		return "KP_Subtract"
	case "NumpadAdd":
		return "KP_Add"
	case "NumpadEnter":
		return "KP_Enter"
	case "NumpadDecimal":
		return "KP_Decimal"
	case "Numpad0", "Numpad1", "Numpad2", "Numpad3", "Numpad4", "Numpad5", "Numpad6", "Numpad7", "Numpad8", "Numpad9":
		return "KP_" + k[6:]
	case "PrintScreen":
		return "Print"
	case "Pause":
		return "Pause"
	case " ":
		return "space"
	case "ContextMenu":
		return "Menu"
	case "AudioVolumeMute":
		return "XF86AudioMute"
	case "AudioVolumeDown":
		return "XF86AudioLowerVolume"
	case "AudioVolumeUp":
		return "XF86AudioRaiseVolume"
	case "MediaPlayPause":
		return "XF86AudioPlay"
	case "MediaTrackNext":
		return "XF86AudioNext"
	case "MediaTrackPrevious":
		return "XF86AudioPrev"
	case "MediaStop":
		return "XF86AudioStop"
	case "BrowserSearch":
		return "XF86Search"
	case "BrowserHome":
		return "XF86HomePage"
	case "BrowserBack":
		return "XF86Back"
	case "BrowserForward":
		return "XF86Forward"
	case "BrowserRefresh":
		return "XF86Refresh"
	case "Clear":
		return "Clear"
	}
	return ""
}

// asciiPunctKey is the physical key that produces a printable ASCII
// punctuation character: code is Chromium's KeyboardEvent.code and vk its
// Windows virtual key code. Shifted and unshifted characters share one
// physical key ("1" and "!" are both Digit1/49), which is what a real
// keypress reports.
type asciiPunctKey struct {
	code string
	vk   int
}

// asciiPunctKeys maps ASCII punctuation to its physical key.
var asciiPunctKeys = map[string]asciiPunctKey{
	"!":  {code: "Digit1", vk: 49},
	"@":  {code: "Digit2", vk: 50},
	"#":  {code: "Digit3", vk: 51},
	"$":  {code: "Digit4", vk: 52},
	"%":  {code: "Digit5", vk: 53},
	"^":  {code: "Digit6", vk: 54},
	"&":  {code: "Digit7", vk: 55},
	"*":  {code: "Digit8", vk: 56},
	"(":  {code: "Digit9", vk: 57},
	")":  {code: "Digit0", vk: 48},
	"-":  {code: "Minus", vk: 189},
	"_":  {code: "Minus", vk: 189},
	"=":  {code: "Equal", vk: 187},
	"+":  {code: "Equal", vk: 187},
	"[":  {code: "BracketLeft", vk: 219},
	"{":  {code: "BracketLeft", vk: 219},
	"]":  {code: "BracketRight", vk: 221},
	"}":  {code: "BracketRight", vk: 221},
	"\\": {code: "Backslash", vk: 220},
	"|":  {code: "Backslash", vk: 220},
	";":  {code: "Semicolon", vk: 186},
	":":  {code: "Semicolon", vk: 186},
	"'":  {code: "Quote", vk: 222},
	"\"": {code: "Quote", vk: 222},
	",":  {code: "Comma", vk: 188},
	"<":  {code: "Comma", vk: 188},
	".":  {code: "Period", vk: 190},
	">":  {code: "Period", vk: 190},
	"/":  {code: "Slash", vk: 191},
	"?":  {code: "Slash", vk: 191},
	"`":  {code: "Backquote", vk: 192},
	"~":  {code: "Backquote", vk: 192},
}
