package main

import (
	"log"
	"strings"
	"time"
)

// CDP input binds to exactly one page target — the active tab (see startTabCtrl
// in tabs.go). Video does not follow that binding: frames come from the Electron
// window's offscreen producer, which always shows the active tab. So when the
// input binding goes stale the viewer gets a live, normal-looking page whose
// mouse and keyboard do nothing, and the only repair used to be the user
// switching tabs by hand, because a tab switch is the one event that re-points
// the binding.
//
// Two ways the binding goes stale on its own:
//
//   - The active tab navigates. Its id does not change, so no tab switch is
//     reported, and preferredURL keeps the URL the tab had at switch time. A
//     later CDP reconnect then matches nothing and falls through to connect()'s
//     fallback: the first non-blank page target in /json/list, which in a
//     multi-tab session is usually a background tab.
//   - A reconnect happens while connect()'s 1.5 s preferred-target window has
//     expired, with the same fallback outcome.
//
// Both leave input aimed at the wrong page for the rest of the session — until
// the user tabs away and back. This watchdog closes that gap by re-aligning the
// binding with the active tab, so input recovers on its own within a tick.

// inputTargetCheckInterval is the re-alignment period. A tick costs one string
// comparison unless the binding is actually wrong.
func inputTargetCheckInterval() time.Duration {
	return time.Duration(parseIntEnv("ISOLA_INPUT_TARGET_CHECK_MS", 2000)) * time.Millisecond
}

func startInputTargetWatchdog() {
	go func() {
		t := time.NewTicker(inputTargetCheckInterval())
		defer t.Stop()
		for range t.C {
			if cdpClient == nil {
				continue
			}
			active := activeTabURL()
			if active == "" {
				continue // no tab strip yet: chromium has not reported tabs
			}
			attached := cdpClient.AttachedURL()
			if !cdpClient.AlignTarget(active) {
				continue
			}
			log.Printf("input: realign %q (was %q)", active, attached)
		}
	}()
}

// matchTabURL reports whether a CDP target URL plausibly belongs to the same
// tab as the runner-reported URL. A prefix match in either direction folds the
// forms the two sides disagree on (a trailing path, or the target URL lagging a
// navigation behind). Two sibling paths under one origin are NOT a match: that
// is the case where input may genuinely be bound to a different tab of the same
// site, and AlignTarget can repair it because hasTargetMatching() only lets it
// reconnect when a target really serves the active URL. Blank URLs only ever
// match each other, so a target sitting on about:blank is correctly seen as NOT
// serving a real active tab.
func matchTabURL(targetURL, tabURL string) bool {
	if targetURL == "" || tabURL == "" {
		return targetURL == tabURL
	}
	return strings.HasPrefix(targetURL, tabURL) || strings.HasPrefix(tabURL, targetURL)
}

// attachedTargetURL is the nil-safe accessor used by the metrics payload.
func attachedTargetURL() string {
	if cdpClient == nil {
		return ""
	}
	return cdpClient.AttachedURL()
}

// inputTargetOK reports whether the page input drives is still the page the
// runner calls active. False is the "clicks do nothing" state: input is
// landing in a tab the viewer cannot see. Unknown on either side reports true —
// this is a diagnostic, not a gate, and must not cry wolf.
func inputTargetOK() bool {
	target := attachedTargetURL()
	active := activeTabURL()
	if target == "" || active == "" {
		return true
	}
	return matchTabURL(target, active)
}
