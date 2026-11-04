package main

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// TestX264ParamsWellFormed guards the x264 parameter string (VULN of the
// silent kind): ffmpeg rejects the *entire* -x264-params value when one token
// is malformed, logs one line, and carries on with the encoder's defaults. A
// stray ':' instead of '=' therefore disables bframes/rc-lookahead/scenecut/
// aq-mode/force-cfr without failing anything that was watching — it was caught
// only because a benchmark test happened to grep the encoder's stderr.
func TestX264ParamsWellFormed(t *testing.T) {
	params := fmt.Sprintf(libx264Quality.params, 50, 50)
	for _, tok := range strings.Split(params, ":") {
		if tok == "" {
			t.Fatalf("empty token in %q", params)
		}
		key, _, ok := strings.Cut(tok, "=")
		if !ok || key == "" {
			t.Fatalf("token %q in %q is not key=value; ffmpeg rejects the whole option", tok, params)
		}
		if strings.ContainsAny(key, " =") {
			t.Fatalf("token %q has a malformed key", tok)
		}
	}
}

// TestX264ParamsAcceptedByFFmpeg runs the production parameter string through
// ffmpeg and fails on the parser line that a malformed token produces.
func TestX264ParamsAcceptedByFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	params := fmt.Sprintf(libx264Quality.params, 50, 50)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-v", "verbose",
		"-f", "lavfi", "-i", "color=c=gray:s=320x240:r=25", "-frames:v", "2",
		"-c:v", "libx264", "-preset", libx264Quality.preset, "-tune", libx264Quality.tune,
		"-x264-params", params, "-f", "null", "-")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg failed: %v\n%s", err, out)
	}
	if strings.Contains(string(out), "Error parsing option") {
		t.Fatalf("ffmpeg rejected the x264 params %q:\n%s", params, out)
	}
}
