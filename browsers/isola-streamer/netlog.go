package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Network monitor: taps CDP Network.* events (via the CDP client's event
// tap), aggregates per-request metadata, and streams compact entries to
// connected viewers. Metadata is all that is broadcast — no bodies, cookies or
// headers reach the panel. The one exception is a transient body read: to give
// a loaded file a content hash, the tap asks Chromium for the body right after
// the request finishes, hashes it, and keeps only the digest (never the body).

const (
	// netBodyHashMaxBytes caps what we read back to hash, using the encoded
	// (on-the-wire) length as the cheap proxy. Compressed bodies decode a little
	// larger than that number; the cap is a cost guard, not an exact limit.
	netBodyHashMaxBytes = 2 << 20 // 2 MiB
	// netBodyHashTimeout bounds how long a finished request waits for its body
	// before the entry is published without a hash.
	netBodyHashTimeout = 1500 * time.Millisecond
	// netBodyHashWorkers bounds concurrent body fetches so a page with hundreds
	// of resources does not queue hundreds of CDP commands at once.
	netBodyHashWorkers = 6
)

type netRequest struct {
	id           string
	cdpID        string // CDP requestId (per target) — the key for getResponseBody
	url          string
	method       string
	typ          string
	mime         string
	status       int
	ip           string
	protocol     string
	connectionID int64
	bytes        int64
	sha1         string // hex SHA-1 of the response body, once fetched
	hashPending  bool   // body fetch in flight; the entry is published when it settles
	failed       bool
	err          string
	tab          int   // active browser tab at capture time (client tab strip id)
	startedAt    int64 // wall-clock capture time, Unix ms (CDP wallTime, else local)
}

// netHashJob is a finished request whose body still has to be fetched and
// hashed before its entry is published.
type netHashJob struct {
	r         *netRequest
	key       string
	sessionID string
}

// netBodyHashSlots is the semaphore behind netBodyHashWorkers.
var netBodyHashSlots = make(chan struct{}, netBodyHashWorkers)

var (
	netMu       sync.Mutex
	netRequests = map[string]*netRequest{} // CDP requestId → state
	netOrder    []string                   // completion order, capped
	netSeenSeq  int
)

const (
	// netMaxEntriesDefault is the capture window used when NET_LOG_MAX_ENTRIES
	// is unset. It mirrors config.NetworkLogMaxEntriesDefault in the backend;
	// the two modules are separate and cannot share the constant.
	netMaxEntriesDefault = 1000
	// netMaxEntriesCap bounds an operator-configured window. Entries are
	// metadata only, but an absurd value would still let one session pin
	// memory for its whole lifetime.
	netMaxEntriesCap = 10000
)

// netMaxEntries reports how many network entries the capture keeps. The
// orchestrator passes NET_LOG_MAX_ENTRIES per container from
// session.network_log_max_entries (ISOLA_NETWORK_LOG_MAX_ENTRIES on the
// backend). An unset or invalid value keeps the default so a session never
// starts with an unbounded or zero window.
func netMaxEntries() int {
	v := os.Getenv("NET_LOG_MAX_ENTRIES")
	if v == "" {
		return netMaxEntriesDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return netMaxEntriesDefault
	}
	if n > netMaxEntriesCap {
		return netMaxEntriesCap
	}
	return n
}

func netMonitorTap(scope, sessionID, method string, params json.RawMessage, tab int) {
	var p netEvent
	if json.Unmarshal(params, &p) != nil {
		return
	}

	// CDP request ids are per target, so two tabs can both have a request
	// "1". Scope the map key by target to keep them from overwriting each
	// other, and give the UI a scoped id so its row keys stay unique too.
	key := scope + "|" + p.RequestId
	uiID := shortTargetID(scope) + ":" + p.RequestId

	netMu.Lock()
	broadcast, hashJob := applyNetEvent(key, uiID, method, &p, tab, sessionID)
	netMu.Unlock()

	if broadcast != nil {
		broadcastNetEntry(broadcast)
	}
	if hashJob != nil {
		go hashNetBody(hashJob)
	}
}

// netEvent is the union of the CDP Network.* params the monitor reads.
type netEvent struct {
	RequestId string `json:"requestId"`
	Type      string `json:"type"`
	ErrorText string `json:"errorText"`
	Canceled  bool   `json:"canceled"`
	Request   struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	} `json:"request"`
	Response struct {
		Status          int    `json:"status"`
		MimeType        string `json:"mimeType"`
		Protocol        string `json:"protocol"`
		RemoteIPAddress string `json:"remoteIPAddress"`
		FromDiskCache   bool   `json:"fromDiskCache"`
	} `json:"response"`
	ConnectionId      int64   `json:"connectionId"`
	EncodedDataLength float64 `json:"encodedDataLength"`
	WallTime          float64 `json:"wallTime"`
}

// applyNetEvent folds one CDP event into the per-request state. Callers hold
// netMu. It returns the entry to publish now (nil when nothing should be sent
// yet) and, for a finished request worth hashing, the job that fetches and
// hashes the body before publishing.
func applyNetEvent(key, uiID, method string, p *netEvent, tab int, sessionID string) (*netRequest, *netHashJob) {
	switch method {
	case "Network.requestWillBeSent":
		// Redirects reuse the requestId with a redirectResponse — treat as
		// an update to the same entry, keeping the original id.
		r := netRequests[key]
		if r == nil {
			r = &netRequest{id: uiID, cdpID: p.RequestId, tab: tab, startedAt: captureTime(p.WallTime)}
			netRequests[key] = r
			rememberOrder(key) // insertion order for replay
		}
		r.url = p.Request.URL
		r.method = p.Request.Method
		if p.Type != "" {
			r.typ = p.Type
		}
		return r, nil
	case "Network.responseReceived":
		r := netRequests[key]
		if r == nil {
			// Attached after requestWillBeSent (e.g. mid-navigation
			// reconnect) — attribute to whatever tab the target belongs to.
			r = &netRequest{id: uiID, cdpID: p.RequestId, tab: tab, startedAt: captureTime(p.WallTime)}
			netRequests[key] = r
		}
		r.status = p.Response.Status
		r.ip = p.Response.RemoteIPAddress
		r.protocol = p.Response.Protocol
		r.connectionID = p.ConnectionId
		if p.Response.MimeType != "" {
			r.mime = p.Response.MimeType
			r.typ = p.Type
		}
		return r, nil
	case "Network.loadingFinished":
		r := netRequests[key]
		if r == nil {
			return nil, nil
		}
		r.bytes = int64(p.EncodedDataLength)
		rememberOrder(key)
		// A file's hash needs its body, which can only be read back right now,
		// before the browser evicts it — so the final entry is published once
		// the hash settles, or after a short grace if it never does.
		if shouldHashBody(r, sessionID) {
			r.hashPending = true
			return nil, &netHashJob{r: r, key: key, sessionID: sessionID}
		}
		return r, nil
	case "Network.loadingFailed":
		r := netRequests[key]
		if r == nil {
			return nil, nil
		}
		r.failed = true
		r.err = p.ErrorText
		rememberOrder(key)
		return r, nil
	}
	return nil, nil
}

// hashableType reports whether a loaded file is a kind worth hashing: the
// static/text resources a user identifies by content (js, css, html, images,
// fonts) plus API payloads. Streams and media are excluded — they are large,
// often chunked, and a body read is least likely to succeed for them.
func hashableType(typ string) bool {
	switch typ {
	case "Document", "Script", "Stylesheet", "Font", "Image", "XHR", "Fetch", "Manifest", "TextTrack", "Prefetch", "Other":
		return true
	default:
		return false
	}
}

// shouldHashBody decides, for a finished request, whether to read its body back
// for hashing. Callers hold netMu.
func shouldHashBody(r *netRequest, sessionID string) bool {
	if sessionID == "" || activeCapture == nil {
		return false
	}
	if r.sha1 != "" || r.hashPending || r.failed {
		return false
	}
	if !hashableType(r.typ) || !strings.HasPrefix(r.url, "http") {
		return false
	}
	return r.bytes <= netBodyHashMaxBytes
}

// hashNetBody fetches one response body, stores its hex SHA-1 on the entry and
// publishes the finished entry. A body that cannot be read (evicted, rejected,
// timed out) simply yields no hash — the entry still goes out, just without one.
func hashNetBody(job *netHashJob) {
	netBodyHashSlots <- struct{}{}
	defer func() { <-netBodyHashSlots }()

	body, ok := fetchResponseBody(job.sessionID, job.r.cdpID, netBodyHashTimeout)

	netMu.Lock()
	job.r.hashPending = false
	if ok {
		sum := sha1.Sum(body)
		job.r.sha1 = hex.EncodeToString(sum[:])
	}
	// Only publish if this is still the live entry — a reconnect or buffer
	// eviction may have dropped it while the body was in flight.
	if netRequests[job.key] == job.r {
		broadcastNetEntry(job.r)
	}
	netMu.Unlock()
}

// fetchResponseBody blocks until the capture client answers with the body (or
// the timeout fires) and reports whether a non-empty body came back.
func fetchResponseBody(sessionID, requestID string, timeout time.Duration) ([]byte, bool) {
	c := activeCapture
	if c == nil {
		return nil, false
	}
	done := make(chan []byte, 1)
	if err := c.requestResponseBody(sessionID, requestID, func(b []byte) { done <- b }); err != nil {
		return nil, false
	}
	select {
	case b := <-done:
		if len(b) == 0 {
			return nil, false
		}
		return b, true
	case <-time.After(timeout):
		return nil, false
	}
}

// captureTime turns a CDP wallTime (UTC seconds since the epoch, 0 when the
// event omits it) into Unix milliseconds, falling back to local clock time.
func captureTime(wallTime float64) int64 {
	if wallTime > 0 {
		return int64(wallTime * 1000)
	}
	return time.Now().UnixMilli()
}

func rememberOrder(key string) {
	for _, seen := range netOrder {
		if seen == key {
			return
		}
	}
	netOrder = append(netOrder, key)
	if len(netOrder) > netMaxEntries() {
		old := netOrder[0]
		netOrder = netOrder[1:]
		delete(netRequests, old)
	}
}

// replayNetTo sends the current capture snapshot to a newly attached
// listener — requests that happened before the viewer connected must still
// reach the panel (the first page load beats the input WS by ~2s).
func replayNetTo(fn func(map[string]any)) {
	fn(map[string]any{"type": "net_init", "entries": []any{}})
	netMu.Lock()
	entries := make([]netRequest, 0, len(netOrder))
	for _, id := range netOrder {
		if r, ok := netRequests[id]; ok {
			entries = append(entries, *r)
		}
	}
	netMu.Unlock()
	for _, r := range entries {
		fn(map[string]any{"type": "net", "e": netEntryWire(&r)})
	}
}

func clip(u string) string {
	if len(u) > 300 {
		return u[:300]
	}
	return u
}

// netEntryWire is the compact entry shape the panel consumes. `h` (hex SHA-1 of
// the body) is omitted until the hash is known so the common payload stays
// small.
func netEntryWire(r *netRequest) map[string]any {
	e := map[string]any{
		"id":  r.id,
		"url": clip(r.url),
		"m":   r.method,
		"s":   r.status,
		"t":   r.typ,
		"ip":  r.ip,
		"b":   r.bytes,
		"f":   r.failed,
		"e":   r.err,
		"tb":  r.tab,
		"ts":  r.startedAt,
	}
	if r.sha1 != "" {
		e["h"] = r.sha1
	}
	return e
}

// broadcastNetEntry ships one compact entry to every viewer input WS.
func broadcastNetEntry(r *netRequest) {
	msg := map[string]any{
		"type": "net",
		"e":    netEntryWire(r),
	}
	tabMu.Lock()
	fns := make([]func(map[string]any), 0, len(tabListeners))
	for _, fn := range tabListeners {
		fns = append(fns, fn)
	}
	tabMu.Unlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("net: listener panic: %v", r)
				}
			}()
			fn(msg)
		}()
	}
}

// isNetEventType filters the CDP events the monitor cares about (cheap
// prefix check before JSON parse).
func isNetEventType(method string) bool {
	return strings.HasPrefix(method, "Network.")
}

// netDumpEntry is the full-fidelity shape returned by GET /network (MCP/API).
type netDumpEntry struct {
	ID           string `json:"id"`
	URL          string `json:"url"`
	Method       string `json:"method"`
	Status       int    `json:"status"`
	Type         string `json:"type"`
	Mime         string `json:"mime,omitempty"`
	IP           string `json:"ip,omitempty"`
	Protocol     string `json:"protocol,omitempty"`
	ConnectionID int64  `json:"connection_id,omitempty"`
	Bytes        int64  `json:"bytes"`
	SHA1         string `json:"sha1,omitempty"`
	Failed       bool   `json:"failed"`
	Error        string `json:"error,omitempty"`
	Tab          int    `json:"tab"`
	TS           int64  `json:"ts"`
}

func netRequestToDump(r *netRequest) netDumpEntry {
	return netDumpEntry{
		ID:           r.id,
		URL:          r.url,
		Method:       r.method,
		Status:       r.status,
		Type:         r.typ,
		Mime:         r.mime,
		IP:           r.ip,
		Protocol:     r.protocol,
		ConnectionID: r.connectionID,
		Bytes:        r.bytes,
		SHA1:         r.sha1,
		Failed:       r.failed,
		Error:        r.err,
		Tab:          r.tab,
		TS:           r.startedAt,
	}
}

func snapshotNetworkLog() []netDumpEntry {
	netMu.Lock()
	entries := make([]netDumpEntry, 0, len(netOrder))
	for _, id := range netOrder {
		if r, ok := netRequests[id]; ok {
			entries = append(entries, netRequestToDump(r))
		}
	}
	netMu.Unlock()
	return entries
}

func networkDumpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": snapshotNetworkLog()})
}
