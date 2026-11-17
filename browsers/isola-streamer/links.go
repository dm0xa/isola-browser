package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	linksDefaultLimit = 200
	linksMaxLimit     = 1000
	linksTextMax      = 200
	linksTimeout      = 5 * time.Second
	linksSelector     = "a[href]"
)

// buildLinksExpression returns the JavaScript that reads the active page's
// anchors in document order. Each entry carries its index in that order so the
// Go side can map it to the same element through the DOM domain
// (querySelectorAll over the same selector returns the same order) and thus to
// a snapshot ref. Nothing is filtered here — filtering, dedupe and the final
// cap happen in selectLinks.
func buildLinksExpression(max int) string {
	return fmt.Sprintf(`(function(){
		var out = [], as = document.querySelectorAll(%q);
		for (var i = 0; i < as.length && out.length < %d; i++) {
			var a = as[i];
			var href = a.href || '';
			var text = (a.innerText || a.textContent || a.getAttribute('aria-label') || a.title || '');
			text = text.replace(/\s+/g, ' ').trim().slice(0, %d);
			out.push({i: i, href: href, text: text});
		}
		return JSON.stringify({url: location.href, title: document.title, links: out});
	})()`, linksSelector, max, linksTextMax)
}

// rawLink is one anchor as the page reported it: idx is its position among
// document.querySelectorAll(linksSelector), used to find the matching DOM node.
type rawLink struct {
	Idx  int    `json:"i"`
	Href string `json:"href"`
	Text string `json:"text"`
}

// linkOut is one returned link. Ref is filled in when the anchor could be
// matched to a snapshot ref, so click and navigate are interchangeable.
type linkOut struct {
	Href string `json:"href"`
	Text string `json:"text"`
	Ref  string `json:"ref,omitempty"`
	idx  int
}

// selectLinks drops empty and duplicate hrefs, keeps only URLs containing
// hostContains (when set), and caps the result, preserving document order.
func selectLinks(raw []rawLink, hostContains string, limit int) []linkOut {
	seen := make(map[string]bool, len(raw))
	out := make([]linkOut, 0, limit)
	for _, r := range raw {
		if r.Href == "" {
			continue
		}
		if hostContains != "" && !strings.Contains(r.Href, hostContains) {
			continue
		}
		if seen[r.Href] {
			continue
		}
		seen[r.Href] = true
		out = append(out, linkOut{Href: r.Href, Text: strings.TrimSpace(r.Text), idx: r.Idx})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// backendIDToRef inverts the snapshot ref map (ref -> backendNodeId) so a DOM
// node can be matched back to the ref snapshot/click use.
func backendIDToRef() map[int64]string {
	snapshotMu.Lock()
	defer snapshotMu.Unlock()
	rev := make(map[int64]string, len(snapshotRefs))
	for ref, id := range snapshotRefs {
		rev[id] = ref
	}
	return rev
}

// attachLinkRefs fills linkOut.Ref from the snapshot refs, best-effort: links
// are still returned without a ref when the DOM lookup fails or the anchor has
// no matching accessibility node.
func attachLinkRefs(links []linkOut) {
	if len(links) == 0 || cdpClient == nil || !cdpClient.Available() {
		return
	}
	rev := backendIDToRef()
	if len(rev) == 0 {
		return
	}
	nodeIDs, err := anchorNodeIDs(linksSelector)
	if err != nil {
		log.Printf("links: dom query: %v", err)
		return
	}
	for i := range links {
		idx := links[i].idx
		if idx < 0 || idx >= len(nodeIDs) {
			continue
		}
		backendID, err := backendIDForNode(nodeIDs[idx])
		if err != nil {
			continue
		}
		if ref, ok := rev[backendID]; ok {
			links[i].Ref = ref
		}
	}
}

// anchorNodeIDs returns the DOM nodeId of every element matching selector, in
// document order — the same order document.querySelectorAll uses in the page.
func anchorNodeIDs(selector string) ([]int64, error) {
	raw, err := cdpClient.callWithResult("DOM.getDocument", map[string]interface{}{"depth": 0}, 3*time.Second)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Root struct {
			NodeID int64 `json:"nodeId"`
		} `json:"root"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Root.NodeID == 0 {
		return nil, errBadResultShape
	}
	raw, err = cdpClient.callWithResult("DOM.querySelectorAll", map[string]interface{}{
		"nodeId":   doc.Root.NodeID,
		"selector": selector,
	}, 3*time.Second)
	if err != nil {
		return nil, err
	}
	var q struct {
		NodeIDs []int64 `json:"nodeIds"`
	}
	if json.Unmarshal(raw, &q) != nil {
		return nil, errBadResultShape
	}
	return q.NodeIDs, nil
}

func backendIDForNode(nodeID int64) (int64, error) {
	raw, err := cdpClient.callWithResult("DOM.describeNode", map[string]interface{}{
		"nodeId": nodeID,
		"depth":  0,
	}, 3*time.Second)
	if err != nil {
		return 0, err
	}
	var d struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
		} `json:"node"`
	}
	if json.Unmarshal(raw, &d) != nil {
		return 0, errBadResultShape
	}
	return d.Node.BackendNodeID, nil
}

// linksHandler returns the active page's links as JSON so an agent can decide
// where to navigate next. Each link carries a snapshot ref when it can be
// matched, so click (by ref) and navigate (by href) are interchangeable. It is
// a read-only probe: it refreshes the accessibility refs but never clicks or
// navigates.
func linksHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if cdpClient == nil || !cdpClient.Available() {
		http.Error(w, "cdp not available", http.StatusServiceUnavailable)
		return
	}
	limit := linksDefaultLimit
	if rawLimit := strings.TrimSpace(r.URL.Query().Get("limit")); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n <= 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	if limit > linksMaxLimit {
		limit = linksMaxLimit
	}

	// Refresh refs first so any ref we return is valid for click on this page
	// state. Best-effort: links are still returned without refs if it fails.
	if _, err := refreshSnapshotRefs(); err != nil {
		log.Printf("links: refresh: %v", err)
	}

	body, err := cdpClient.evaluateStringTimeout(buildLinksExpression(linksMaxLimit), linksTimeout)
	if err != nil {
		log.Printf("links: evaluate: %v", err)
		http.Error(w, "links failed", http.StatusInternalServerError)
		return
	}
	var page struct {
		URL   string    `json:"url"`
		Title string    `json:"title"`
		Links []rawLink `json:"links"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		log.Printf("links: parse: %v", err)
		http.Error(w, "links parse failed", http.StatusInternalServerError)
		return
	}

	selected := selectLinks(page.Links, strings.TrimSpace(r.URL.Query().Get("host_contains")), limit)
	attachLinkRefs(selected)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"url":   page.URL,
		"title": page.Title,
		"count": len(selected),
		"links": selected,
	})
}
