package main

import (
	"testing"
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
