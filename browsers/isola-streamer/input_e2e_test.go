// Real-browser typing regression test for a sandbox session.
//
// The unit suites prove which CDP commands the streamer builds; only a real
// Electron/Chromium can prove those commands actually type. This test boots
// the sandbox image, installs a focused textarea in the page, sends the same
// JSON input events the bundled viewer sends, and asserts the field's value.
//
// It exists because punctuation used to be dropped between the viewer and
// Chromium: "!@#,.?" never reached the page, so an email address or password
// could not be typed into a sandbox form.
//
// Opt-in because it needs Docker and a built sandbox image:
//
//	ISOLA_INPUT_E2E=1 go test -run TestSandboxTypingOnRealSite -v .
//
// Overrides: ISOLA_INPUT_E2E_IMAGE (default isola-browser:electron),
// ISOLA_INPUT_E2E_RESOLUTION (default 1280x720), ISOLA_INPUT_E2E_KEEP=1 keeps
// the container for inspection.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	inputE2EEnv   = "ISOLA_INPUT_E2E"
	inputE2EImage = "ISOLA_INPUT_E2E_IMAGE"
	inputE2ERes   = "ISOLA_INPUT_E2E_RESOLUTION"
	inputE2ESite  = "https://example.com/"
)

func TestSandboxTypingOnRealSite(t *testing.T) {
	if os.Getenv(inputE2EEnv) != "1" {
		t.Skipf("set %s=1 to run the real-browser typing test (needs Docker)", inputE2EEnv)
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("docker not on PATH: %v", err)
	}
	image := envOrE2E(inputE2EImage, "isola-browser:electron")
	resolution := envOrE2E(inputE2ERes, "1280x720")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	name := fmt.Sprintf("isola-input-e2e-%d", time.Now().UnixNano())
	runE2E(ctx, t, "docker", fpsE2EDockerRunArgs(image, name, inputE2ESite, resolution)...)
	if os.Getenv("ISOLA_INPUT_E2E_KEEP") == "1" {
		t.Logf("keeping container %s", name)
	} else {
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	}

	streamPort := waitE2EPort(ctx, t, name, "5900/tcp")
	cdpPort := waitE2EPort(ctx, t, name, "9223/tcp")
	waitE2EStreamer(t, ctx, streamPort)
	page := dialE2EPage(t, ctx, cdpPort, inputE2ESite)
	waitE2EPageLoaded(t, page)

	// A focused form control, plus a keydown log so the test can check the
	// event identity the page sees (KeyboardEvent.code / shiftKey), not just
	// the resulting value.
	fixture := `(() => {
		document.body.innerHTML = '';
		const t = document.createElement('textarea');
		t.id = 'probe';
		t.style.cssText = 'position:fixed;left:10px;top:10px;width:900px;height:400px;z-index:2147483647;font-size:24px';
		document.body.appendChild(t);
		window.__probe = t;
		window.__keys = [];
		t.addEventListener('keydown', (e) => window.__keys.push({
			key: e.key, code: e.code, shift: e.shiftKey, ctrl: e.ctrlKey || e.metaKey,
		}));
		t.focus();
		return document.activeElement === t ? 'ok' : 'not-focused';
	})()`
	// The runner may still be committing a navigation when the target first
	// reports readyState=complete; install the fixture until it sticks.
	installed := false
	for attempt := 0; attempt < 20 && !installed; attempt++ {
		if got := mustEval(t, page, fixture); got == "ok" {
			time.Sleep(300 * time.Millisecond)
			installed = mustEval(t, page, `window.__probe ? 'ok' : 'gone'`) == "ok"
		}
		if !installed {
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !installed {
		t.Fatalf("fixture did not stick (href=%q readyState=%q)",
			mustEval(t, page, `location.href`), mustEval(t, page, `document.readyState`))
	}

	input := dialE2EInput(t, streamPort)

	// The streamer may still be attaching its CDP client when the page first
	// reports loaded; events sent in that window are queued and can be
	// stale-dropped. Prove the key path is live with one character first.
	live := false
	for attempt := 0; attempt < 40 && !live; attempt++ {
		mustEval(t, page, `window.__probe.value=''; window.__probe.focus(); 'ok'`)
		sendE2EInput(t, input, inputEvent{Type: "chord", Combo: "x"})
		live = waitProbeValue(t, page, "x", 500*time.Millisecond)
		if !live {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if !live {
		t.Fatal("the sandbox never accepted a keystroke")
	}

	// The user's strings: an email address, a password with Shift symbols,
	// a Windows path (backslash), prose punctuation, a URL fragment, and
	// non-ASCII text.
	typed := "user@example.com P@ssw0rd!2026 !@#$%^&*() " +
		`C:\Users\me a+b=c, d.e; f:g? "quoted" [brackets] {braces} tag#1 ` +
		"café €5 100% ~/dir <tag> | pipe"
	// A reconnect mid-typing can drop a queued prefix; retry the whole string
	// once the path is live rather than reporting a harness race as a defect.
	typedOK := false
	for attempt := 0; attempt < 3 && !typedOK; attempt++ {
		mustEval(t, page, `window.__probe.value=''; window.__probe.focus(); window.__probe.setSelectionRange(0,0); 'ok'`)
		for _, r := range typed {
			sendE2EInput(t, input, inputEvent{Type: "chord", Combo: viewerChord(r)})
		}
		typedOK = waitProbeValue(t, page, typed, 5*time.Second)
		if !typedOK {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !typedOK {
		t.Fatalf("field value = %q, want %q (href=%q readyState=%q)",
			probeValue(t, page), typed,
			mustEval(t, page, `location.href`), mustEval(t, page, `document.readyState`))
	}
	t.Logf("typed %d characters via chords", len([]rune(typed)))

	// Event identity: "!" is Shift+Digit1, "+" is Shift+Equal, "," is Comma.
	keys := readE2EKeys(t, page)
	for _, want := range []struct {
		key   string
		code  string
		shift bool
	}{
		{"!", "Digit1", true},
		{"@", "Digit2", true},
		{"#", "Digit3", true},
		{"+", "Equal", true},
		{",", "Comma", false},
		{".", "Period", false},
		{"?", "Slash", true},
		{`\`, "Backslash", false},
	} {
		got, ok := findE2EKey(keys, want.key)
		if !ok {
			t.Errorf("page saw no keydown for %q", want.key)
			continue
		}
		if got.Code != want.code || got.Shift != want.shift {
			t.Errorf("keydown %q = code %q shift %v, want code %q shift %v",
				want.key, got.Code, got.Shift, want.code, want.shift)
		}
	}
	if _, ok := findE2EKey(keys, "€"); ok {
		t.Error("€ arrived as a keydown; it has no physical key and must be inserted as text")
	}

	// Paste/autofill path: a whole string in one message.
	mustEval(t, page, `window.__probe.value=''; window.__probe.focus(); 'ok'`)
	const pasted = "pasted@example.com P@ss w0rd! €5"
	sendE2EInput(t, input, inputEvent{Type: "type", Text: pasted})
	awaitProbeValue(t, page, pasted)

	// Editing: caret movement, Backspace and Enter behave like a keyboard.
	mustEval(t, page, `window.__probe.value='abc'; window.__probe.focus(); window.__probe.setSelectionRange(3,3); 'ok'`)
	sendE2EInput(t, input, inputEvent{Type: "chord", Combo: "ArrowLeft"})
	sendE2EInput(t, input, inputEvent{Type: "chord", Combo: "Backspace"})
	awaitProbeValue(t, page, "ac")
	sendE2EInput(t, input, inputEvent{Type: "chord", Combo: "Enter"})
	// After Backspace the caret sits between "a" and "c", so Enter splits them.
	awaitProbeValue(t, page, "a\nc")

	// Ctrl+A selects everything, and the next typed character replaces it —
	// the shortcut must not also insert "a".
	sendE2EInput(t, input, inputEvent{Type: "chord", Combo: "ctrl+a"})
	sendE2EInput(t, input, inputEvent{Type: "chord", Combo: "shift+X"})
	awaitProbeValue(t, page, "X")

	t.Log("sandbox typing verified: symbols, paste, editing and Ctrl+A")
}

// dialE2EInput opens the session's input socket. A goroutine drains replies
// (keepalives, replayed events) so the streamer never blocks on a full socket.
func dialE2EInput(t *testing.T, streamPort string) *websocket.Conn {
	t.Helper()
	wsURL := "ws://127.0.0.1:" + streamPort + "/input"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial input %s: %v", wsURL, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	go func() {
		for {
			if _, _, rerr := conn.ReadMessage(); rerr != nil {
				return
			}
		}
	}()
	return conn
}

func sendE2EInput(t *testing.T, conn *websocket.Conn, ev inputEvent) {
	t.Helper()
	if err := conn.WriteJSON(ev); err != nil {
		t.Fatalf("write input %+v: %v", ev, err)
	}
}

func mustEval(t *testing.T, page *e2ePage, expr string) string {
	t.Helper()
	v, err := page.eval(t, expr, 10*time.Second)
	if err != nil {
		t.Fatalf("eval %s: %v", expr, err)
	}
	return v
}

func probeValue(t *testing.T, page *e2ePage) string {
	t.Helper()
	return mustEval(t, page, `window.__probe ? window.__probe.value : '__MISSING__'`)
}

// waitProbeValue polls the field until it holds want, reporting whether it got
// there within timeout.
func waitProbeValue(t *testing.T, page *e2ePage, want string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if probeValue(t, page) == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func awaitProbeValue(t *testing.T, page *e2ePage, want string) {
	t.Helper()
	if waitProbeValue(t, page, want, 5*time.Second) {
		return
	}
	t.Fatalf("field value = %q, want %q (href=%q readyState=%q)",
		probeValue(t, page), want,
		mustEval(t, page, `location.href`), mustEval(t, page, `document.readyState`))
}

type e2eKey struct {
	Key   string `json:"key"`
	Code  string `json:"code"`
	Shift bool   `json:"shift"`
	Ctrl  bool   `json:"ctrl"`
}

func readE2EKeys(t *testing.T, page *e2ePage) []e2eKey {
	t.Helper()
	raw := mustEval(t, page, `JSON.stringify(window.__keys || [])`)
	var keys []e2eKey
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		t.Fatalf("parse keydown log %q: %v", raw, err)
	}
	return keys
}

func findE2EKey(keys []e2eKey, key string) (e2eKey, bool) {
	for _, k := range keys {
		if k.Key == key {
			return k, true
		}
	}
	return e2eKey{}, false
}
