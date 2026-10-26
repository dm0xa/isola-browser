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

// netRedirect is one hop of a redirect chain. The chain used to be lost: a
// redirect reuses the CDP requestId and the new URL overwrote the old one.
type netRedirect struct {
	Status int    `json:"status"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// netTLS summarises the certificate a document was served with. SecurityState
// is CDP's verdict on the connection, not our inference: Chromium reports
// "insecure" for a chain it accepted only because certificate errors were
// ignored.
type netTLS struct {
	Protocol      string   `json:"protocol,omitempty"`
	Cipher        string   `json:"cipher,omitempty"`
	Issuer        string   `json:"issuer,omitempty"`
	Subject       string   `json:"subject,omitempty"`
	ValidFrom     float64  `json:"valid_from,omitempty"`
	ValidTo       float64  `json:"valid_to,omitempty"`
	SANs          []string `json:"sans,omitempty"`
	SecurityState string   `json:"security_state,omitempty"`
}

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

	// Response metadata. Headers are kept only for the types where they carry
	// signal and are shed oldest-first past netHeaderEntries (see headers.go).
	headers   map[string]string
	tls       *netTLS
	tlsState  string // CDP securityState, kept even when there are no cert details
	ttfbMs    int64  // time to first byte, from CDP response.timing
	cached    bool   // served from the disk cache
	sw        bool   // served by a service worker
	posture   *postureReport
	redirects []netRedirect
	// originHost is the tab's document host when this entry was created. It is
	// what makes first- vs third-party classification possible without any DNS.
	originHost string
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
	netOrdered  map[string]bool            // membership index for netOrder
	netSeenSeq  int
	// netOriginByTab remembers the document host per browser tab, so a request
	// can be classified without re-parsing its tab's URL.
	netOriginByTab = map[int]string{}
	// netMainFrameByTarget remembers the first document frame seen per CDP
	// target — the page's main frame. Later Document requests from other frame
	// ids are iframes and do not move the tab's origin.
	netMainFrameByTarget = map[string]string{}
	// netHeadered counts entries currently holding headers, for the shed loop.
	netHeadered int
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
	// netWireURLMax is the displayed URL budget. Longer URLs (data: payloads,
	// tracking blobs) are clipped; the wire marks them so the viewer does not
	// present a clipped copy as the real address.
	netWireURLMax = 300
)

var (
	netMaxOnce sync.Once
	netMaxVal  int
)

// netMaxEntries reports how many network entries the capture keeps. The
// orchestrator passes NET_LOG_MAX_ENTRIES per container from
// session.network_log_max_entries (ISOLA_NETWORK_LOG_MAX_ENTRIES on the
// backend). An unset or invalid value keeps the default so a session never
// starts with an unbounded or zero window. The container environment is fixed
// at process start, so the value is read once instead of on every event.
func netMaxEntries() int {
	netMaxOnce.Do(func() { netMaxVal = readNetMaxEntries() })
	return netMaxVal
}

// resetNetMaxEntriesForTest re-arms the one-time environment read.
func resetNetMaxEntriesForTest() {
	netMaxOnce = sync.Once{}
	netMaxVal = 0
}

func readNetMaxEntries() int {
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
	broadcast, hashJob, emitHeaders := applyNetEvent(key, uiID, method, &p, tab, sessionID)
	// Serialise while the lock is held: the entry's string fields are written
	// by other goroutines (redirects, tab reassignment, body hashing), so a
	// wire map built after unlocking is a data race on a string header.
	var msg map[string]any
	if broadcast != nil {
		msg = netWireMessage(broadcast)
	}
	var hmsg map[string]any
	if emitHeaders && broadcast != nil {
		hmsg = netHeadersMessage(broadcast)
	}
	netMu.Unlock()

	if msg != nil {
		broadcastNetMessage(msg)
	}
	if hmsg != nil {
		broadcastNetMessage(hmsg)
	}
	if hashJob != nil {
		go hashNetBody(hashJob)
	}
}

// netEvent is the union of the CDP Network.* params the monitor reads.
type netEvent struct {
	RequestId string `json:"requestId"`
	Type      string `json:"type"`
	// FrameId identifies the frame that issued the request. Only the main
	// frame's document defines the tab's origin; an iframe document (a consent
	// wall, a payment widget) must not reclassify the whole page.
	FrameId   string `json:"frameId"`
	ErrorText string `json:"errorText"`
	Canceled  bool   `json:"canceled"`
	Request   struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	} `json:"request"`
	// RedirectResponse rides requestWillBeSent when this request replaces a
	// previous hop. It is the only place the hop's status is reported.
	RedirectResponse *struct {
		URL    string `json:"url"`
		Status int    `json:"status"`
	} `json:"redirectResponse"`
	Response struct {
		Status            int               `json:"status"`
		MimeType          string            `json:"mimeType"`
		Protocol          string            `json:"protocol"`
		RemoteIPAddress   string            `json:"remoteIPAddress"`
		FromDiskCache     bool              `json:"fromDiskCache"`
		FromServiceWorker bool              `json:"fromServiceWorker"`
		Headers           map[string]string `json:"headers"`
		SecurityDetails   *struct {
			Protocol    string   `json:"protocol"`
			Cipher      string   `json:"cipher"`
			Issuer      string   `json:"issuer"`
			SubjectName string   `json:"subjectName"`
			ValidFrom   float64  `json:"validFrom"`
			ValidTo     float64  `json:"validTo"`
			SANList     []string `json:"sanList"`
		} `json:"securityDetails"`
		// "secure", "neutral", "insecure", "insecure-broken", "info",
		// "unknown". Present for every response; the only signal that
		// distinguishes a verified chain from one Chromium was told to accept.
		SecurityState string `json:"securityState"`
		Timing        *struct {
			ReceiveHeadersEnd float64 `json:"receiveHeadersEnd"`
		} `json:"timing"`
	} `json:"response"`
	// Network.responseReceivedExtraInfo reports these at the top level. Its
	// header list is the raw wire form, including headers Chromium filters out
	// of response.headers — the better source when both arrive.
	Headers           map[string]string `json:"headers"`
	HeadersText       string            `json:"headersText"`
	StatusCode        int               `json:"statusCode"`
	ConnectionId      int64             `json:"connectionId"`
	EncodedDataLength float64           `json:"encodedDataLength"`
	WallTime          float64           `json:"wallTime"`
}

// applyNetEvent folds one CDP event into the per-request state. Callers hold
// netMu. It returns the entry to publish now (nil when nothing should be sent
// yet) and, for a finished request worth hashing, the job that fetches and
// hashes the body before publishing.
func applyNetEvent(key, uiID, method string, p *netEvent, tab int, sessionID string) (*netRequest, *netHashJob, bool) {
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
		if p.RedirectResponse != nil {
			// Record the hop before the URL is overwritten. Without this the
			// chain collapses to its final URL and the status codes are lost.
			from := r.url
			if from == "" {
				from = p.RedirectResponse.URL
			}
			r.redirects = append(r.redirects, netRedirect{
				Status: p.RedirectResponse.Status,
				From:   from,
				To:     p.Request.URL,
			})
		}
		r.url = p.Request.URL
		r.method = p.Request.Method
		if p.Type != "" {
			r.typ = p.Type
		}
		if strings.EqualFold(r.typ, "Document") {
			if isMainFrameDocument(scopeOf(key), p.FrameId) {
				r.originHost = hostOfURL(r.url)
				netOriginByTab[tab] = r.originHost
			} else if origin := netOriginByTab[tab]; origin != "" {
				// An iframe document inherits the page's origin instead of
				// replacing it.
				r.originHost = origin
			}
		} else if origin := netOriginByTab[tab]; origin != "" {
			r.originHost = origin
		}
		return r, nil, false
	case "Network.responseReceived":
		r := netRequests[key]
		if r == nil {
			// Attached after requestWillBeSent (e.g. mid-navigation
			// reconnect) — attribute to whatever tab the target belongs to.
			// It must enter the rolling window here: a response that never
			// produces loadingFinished/loadingFailed (streaming, SSE, a
			// closed tab) would otherwise stay outside netOrder forever and
			// escape the capture window.
			r = &netRequest{id: uiID, cdpID: p.RequestId, tab: tab, startedAt: captureTime(p.WallTime)}
			netRequests[key] = r
			rememberOrder(key)
		}
		r.status = p.Response.Status
		r.ip = p.Response.RemoteIPAddress
		r.protocol = p.Response.Protocol
		r.connectionID = p.ConnectionId
		if p.Response.MimeType != "" {
			r.mime = p.Response.MimeType
			r.typ = p.Type
		}
		r.cached = p.Response.FromDiskCache
		r.sw = p.Response.FromServiceWorker
		r.tlsState = p.Response.SecurityState
		if sd := p.Response.SecurityDetails; sd != nil {
			r.tls = &netTLS{
				Protocol:      sd.Protocol,
				Cipher:        sd.Cipher,
				Issuer:        sd.Issuer,
				Subject:       sd.SubjectName,
				ValidFrom:     sd.ValidFrom,
				ValidTo:       sd.ValidTo,
				SANs:          clipStrings(sd.SANList, 20),
				SecurityState: p.Response.SecurityState,
			}
		}
		if t := p.Response.Timing; t != nil && t.ReceiveHeadersEnd > 0 {
			r.ttfbMs = int64(t.ReceiveHeadersEnd)
		}
		if headersWanted(r.typ) {
			hadHeaders := r.headers != nil
			r.headers = mergeHeaders(r.headers, p.Response.Headers)
			if !hadHeaders && r.headers != nil {
				netHeadered++
			}
			shedHeaders()
		} else {
			if r.headers != nil {
				netHeadered--
			}
			r.headers = nil
		}
		if len(r.headers) > 0 && strings.EqualFold(r.typ, "Document") {
			r.posture = assessPosture(r.headers, r.tlsState)
			return r, nil, true
		}
		return r, nil, false
	case "Network.responseReceivedExtraInfo":
		// Set-Cookie rides here too, but cookies have their own tap; this case
		// exists for the raw header block (headersText) and for responses whose
		// header map carried nothing usable. It never *creates* a row: the cookie
		// tap must not leave blank entries in the panel, and requestWillBeSent
		// always precedes a real response.
		r := netRequests[key]
		if r == nil {
			return nil, nil, false
		}
		if p.StatusCode > 0 && r.status == 0 {
			r.status = p.StatusCode
		}
		if headersWanted(r.typ) {
			hadHeaders := r.headers != nil
			r.headers = mergeHeaders(r.headers, p.Headers)
			r.headers = mergeHeaderText(r.headers, p.HeadersText)
			if !hadHeaders && r.headers != nil {
				netHeadered++
			}
			shedHeaders()
		}
		if len(r.headers) > 0 && strings.EqualFold(r.typ, "Document") {
			r.posture = assessPosture(r.headers, r.tlsState)
			return r, nil, true
		}
		return r, nil, false
	case "Network.loadingFinished":
		r := netRequests[key]
		if r == nil {
			return nil, nil, false
		}
		r.bytes = int64(p.EncodedDataLength)
		rememberOrder(key)
		// A file's hash needs its body, which can only be read back right now,
		// before the browser evicts it — so the final entry is published once
		// the hash settles, or after a short grace if it never does.
		if shouldHashBody(r, sessionID) {
			r.hashPending = true
			return nil, &netHashJob{r: r, key: key, sessionID: sessionID}, false
		}
		return r, nil, false
	case "Network.loadingFailed":
		r := netRequests[key]
		if r == nil {
			return nil, nil, false
		}
		r.failed = true
		r.err = p.ErrorText
		rememberOrder(key)
		return r, nil, false
	}
	return nil, nil, false
}

// scopeOf recovers the CDP target id from a capture key ("scope|requestId").
func scopeOf(key string) string {
	scope, _, _ := strings.Cut(key, "|")
	return scope
}

// isMainFrameDocument reports whether this Document request belongs to the
// target's main frame. The first document frame seen for a fresh target is the
// main frame; anything else is an iframe. When CDP omits frameId, assume main
// so classification keeps working rather than silently freezing the origin.
func isMainFrameDocument(scope, frameID string) bool {
	if frameID == "" {
		return true
	}
	main, ok := netMainFrameByTarget[scope]
	if !ok {
		netMainFrameByTarget[scope] = frameID
		return true
	}
	return main == frameID
}

// shedHeaders keeps at most netHeaderEntries entries holding headers, dropping
// the oldest first. Callers hold netMu.
func shedHeaders() {
	if netHeadered <= netHeaderEntriesLimit {
		return
	}
	for _, key := range netOrder {
		r := netRequests[key]
		if r == nil || r.headers == nil {
			continue
		}
		r.headers = nil
		r.posture = nil
		netHeadered--
		if netHeadered <= netHeaderEntriesLimit {
			return
		}
	}
}

// clipStrings caps a slice copied onto the wire.
func clipStrings(in []string, max int) []string {
	if len(in) == 0 {
		return nil
	}
	if len(in) > max {
		in = in[:max]
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
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
	// eviction may have dropped it while the body was in flight. Build the
	// message under the lock; a listener callback (a viewer WS write) must
	// never run while netMu is held, or one slow viewer stalls capture.
	var msg map[string]any
	if netRequests[job.key] == job.r {
		msg = netWireMessage(job.r)
	}
	netMu.Unlock()
	if msg != nil {
		broadcastNetMessage(msg)
	}
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
	if netOrdered == nil {
		netOrdered = make(map[string]bool, len(netOrder)+1)
		for _, seen := range netOrder {
			netOrdered[seen] = true
		}
	}
	if netOrdered[key] {
		return
	}
	netOrdered[key] = true
	netOrder = append(netOrder, key)
	if len(netOrder) > netMaxEntries() {
		old := netOrder[0]
		netOrder = netOrder[1:]
		delete(netOrdered, old)
		if r := netRequests[old]; r != nil && r.headers != nil {
			netHeadered--
		}
		delete(netRequests, old)
	}
}

// replayNetTo sends the current capture snapshot to a newly attached
// listener — requests that happened before the viewer connected must still
// reach the panel (the first page load beats the input WS by ~2s).
func replayNetTo(fn func(map[string]any)) {
	fn(map[string]any{"type": "net_init", "entries": []any{}})
	netMu.Lock()
	msgs := make([]map[string]any, 0, len(netOrder))
	var headerMsgs []map[string]any
	for _, id := range netOrder {
		if r, ok := netRequests[id]; ok {
			msgs = append(msgs, netWireMessage(r))
			// Document headers are not part of the compact entry, so a viewer
			// that attaches after the first load would otherwise never get them.
			if len(r.headers) > 0 && strings.EqualFold(r.typ, "Document") {
				headerMsgs = append(headerMsgs, netHeadersMessage(r))
			}
		}
	}
	netMu.Unlock()
	for _, msg := range msgs {
		fn(msg)
	}
	for _, msg := range headerMsgs {
		fn(msg)
	}
}

func clip(u string) string {
	if len(u) > netWireURLMax {
		return u[:netWireURLMax]
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
	if len(r.url) > netWireURLMax {
		// The panel can show a clipped URL, but copying it must not look like
		// it produced the real address. Flag it so the viewer can say so.
		e["ut"] = true
	}
	if r.sha1 != "" {
		e["h"] = r.sha1
	}
	if len(r.redirects) > 0 {
		e["rd"] = r.redirects
	}
	if r.ttfbMs > 0 {
		e["ttfb"] = r.ttfbMs
	}
	if r.cached {
		e["dc"] = true
	}
	if r.sw {
		e["sw"] = true
	}
	if thirdParty, tracker := classifyEntry(r); thirdParty || tracker != "" {
		if thirdParty {
			e["tp"] = true
		}
		if tracker != "" {
			e["tr"] = tracker
		}
	}
	if isBeaconType(r.typ) {
		e["bc"] = true
	}
	return e
}

// classifyEntry derives first-/third-party status and a tracker label from the
// URL and the tab's document host. It never resolves DNS.
func classifyEntry(r *netRequest) (thirdParty bool, tracker string) {
	host := hostOfURL(r.url)
	if host != "" && r.originHost != "" {
		thirdParty = !strings.EqualFold(registrableDomain(host), registrableDomain(r.originHost))
	}
	return thirdParty, trackerForURL(r.url, host)
}

// netHeadersMessage is the document-header side channel. It is separate from
// the compact entry so subresources do not carry header maps on the wire; the
// full set stays available in the /network dump.
func netHeadersMessage(r *netRequest) map[string]any {
	e := map[string]any{
		"id":  r.id,
		"url": clip(r.url),
		"s":   r.status,
		"tb":  r.tab,
		"hd":  r.headers,
	}
	if r.posture != nil {
		e["pst"] = r.posture
	}
	if r.tls != nil {
		e["tls"] = r.tls
	}
	return map[string]any{"type": "headers", "e": e}
}

// netWireMessage wraps one serialised entry for the wire. Callers must hold
// netMu: netEntryWire reads fields other goroutines write.
func netWireMessage(r *netRequest) map[string]any {
	return map[string]any{
		"type": "net",
		"e":    netEntryWire(r),
	}
}

// broadcastNetMessage ships one already-serialised entry to every viewer input
// WS. It must be called without netMu held, so a slow viewer cannot stall the
// capture loop.
func broadcastNetMessage(msg map[string]any) {
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
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Method        string            `json:"method"`
	Status        int               `json:"status"`
	Type          string            `json:"type"`
	Mime          string            `json:"mime,omitempty"`
	IP            string            `json:"ip,omitempty"`
	Protocol      string            `json:"protocol,omitempty"`
	ConnectionID  int64             `json:"connection_id,omitempty"`
	Bytes         int64             `json:"bytes"`
	SHA1          string            `json:"sha1,omitempty"`
	Failed        bool              `json:"failed"`
	Error         string            `json:"error,omitempty"`
	Tab           int               `json:"tab"`
	TS            int64             `json:"ts"`
	TTFBMs        int64             `json:"ttfb_ms,omitempty"`
	Cached        bool              `json:"from_disk_cache,omitempty"`
	ServiceWorker bool              `json:"from_service_worker,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	TLS           *netTLS           `json:"tls,omitempty"`
	Redirects     []netRedirect     `json:"redirects,omitempty"`
	Posture       *postureReport    `json:"posture,omitempty"`
	ThirdParty    bool              `json:"third_party,omitempty"`
	Tracker       string            `json:"tracker,omitempty"`
	Beacon        bool              `json:"beacon,omitempty"`
}

func netRequestToDump(r *netRequest) netDumpEntry {
	thirdParty, tracker := classifyEntry(r)
	return netDumpEntry{
		ID:            r.id,
		URL:           r.url,
		Method:        r.method,
		Status:        r.status,
		Type:          r.typ,
		Mime:          r.mime,
		IP:            r.ip,
		Protocol:      r.protocol,
		ConnectionID:  r.connectionID,
		Bytes:         r.bytes,
		SHA1:          r.sha1,
		Failed:        r.failed,
		Error:         r.err,
		Tab:           r.tab,
		TS:            r.startedAt,
		TTFBMs:        r.ttfbMs,
		Cached:        r.cached,
		ServiceWorker: r.sw,
		Headers:       r.headers,
		TLS:           r.tls,
		Redirects:     r.redirects,
		Posture:       r.posture,
		ThirdParty:    thirdParty,
		Tracker:       tracker,
		Beacon:        isBeaconType(r.typ),
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
