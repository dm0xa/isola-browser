package api

import (
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/orchestrator"
)

// Live custom data: cookies, extra request headers and web storage pushed into
// an already-running page. The streamer owns validation and the CDP work; this
// handler adds the two things only the control plane knows — ownership and the
// session's target URL (used to default a cookie's domain).
//
// Values are secrets: the request body is never logged and the response only
// carries counts. The forward's deadline lives in orchestrator.ApplySeed (its
// HTTP client), so a timeout is classified there rather than masked by a
// slightly larger outer context.

// decodeSeedJSON is decodeJSON with unknown-field rejection: a typo like
// "httpOnly" would otherwise silently drop a security attribute the caller
// believed was set.
func decodeSeedJSON(r *http.Request, dst interface{}) error {
	return decodeJSONBody(r, dst, true)
}

// ApplySessionSeed handles POST /api/sessions/{id}/seed.
func (h *Handler) ApplySessionSeed(w http.ResponseWriter, r *http.Request) {
	// Counts and errors only, but the endpoint is documented as no-store on
	// every response; set it before anything can write.
	setSensitiveNoStore(w)
	session, ok := h.ownedActiveSession(w, r)
	if !ok {
		return
	}

	var seed models.SessionSeed
	if err := decodeSeedJSON(r, &seed); err != nil {
		if errors.Is(err, errBodyTooLarge) {
			respondError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
			return
		}
		message := err.Error()
		if errors.Is(err, io.EOF) {
			message = "request body is required"
		}
		respondErrorMessage(w, http.StatusBadRequest, "invalid_seed", message)
		return
	}
	if err := seed.FillDefaults(session.TargetURL); err != nil {
		respondErrorMessage(w, http.StatusBadRequest, "invalid_seed", err.Error())
		return
	}

	addr, ok := h.sessionContainerAddr(w, r, session)
	if !ok {
		return
	}
	host, portStr, err := net.SplitHostPort(addr)
	port, portErr := strconv.Atoi(portStr)
	if err != nil || portErr != nil || port <= 0 {
		respondError(w, http.StatusConflict, "container_not_ready")
		return
	}

	res, err := h.orch.ApplySeed(r.Context(), host, port, &seed)
	if err != nil {
		respondSeedError(w, session, err)
		return
	}
	respondJSON(w, http.StatusOK, res)
}

// respondSeedError maps the streamer's status onto a stable API code. Its
// message is relayed because it names the offending field ("cookies[2]:
// invalid path") without echoing values.
func respondSeedError(w http.ResponseWriter, session *models.Session, err error) {
	var seedErr *orchestrator.SeedError
	if !errors.As(err, &seedErr) {
		log.Printf("seed: session %s: %v", session.ID, err)
		respondError(w, http.StatusBadGateway, "seed_failed")
		return
	}
	switch seedErr.Status {
	case http.StatusBadRequest:
		respondErrorMessage(w, http.StatusBadRequest, "invalid_seed", seedErr.Message)
	case http.StatusRequestEntityTooLarge:
		respondError(w, http.StatusRequestEntityTooLarge, "payload_too_large")
	case http.StatusConflict:
		respondErrorMessage(w, http.StatusConflict, "seed_origin_mismatch", seedErr.Message)
	case http.StatusServiceUnavailable:
		respondErrorMessage(w, http.StatusBadGateway, "seed_unavailable", seedErr.Message)
	case http.StatusGatewayTimeout:
		respondErrorMessage(w, http.StatusGatewayTimeout, "seed_timeout", seedErr.Message)
	default:
		log.Printf("seed: session %s: status=%d %s", session.ID, seedErr.Status, seedErr.Message)
		respondErrorMessage(w, http.StatusBadGateway, "seed_failed", seedErr.Message)
	}
}
