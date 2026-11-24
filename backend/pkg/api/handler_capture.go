package api

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/dm0xa/isola-browser/backend/pkg/capture"
)

// Resource capture: the per-session routes live in the 60s REST group beside
// the graph/intel proxies; the archive routes are mounted outside it because a
// zip or blob download may outlive any REST timeout. Those handlers extend the
// connection write deadline themselves.

const (
	captureBlobWriteDeadline = 10 * time.Minute
	captureZipWriteDeadline  = 30 * time.Minute
)

// RegisterCaptureRoutes mounts the archive routes. They are deliberately not
// under chi's 60s Timeout group (see internal/app/app.go).
func (h *Handler) RegisterCaptureRoutes(r chi.Router) {
	r.Get("/api/captures", h.ListCaptures)
	r.Get("/api/captures/{archiveID}/blobs/{sha256}", h.GetArchiveBlob)
	r.Get("/api/captures/{archiveID}/files/*", h.GetArchiveFile)
	r.Get("/api/captures/{archiveID}/download", h.DownloadArchive)
	r.Get("/api/captures/{archiveID}", h.GetArchive)
	r.Delete("/api/captures/{archiveID}", h.DeleteArchive)
}

// captureEnabled writes capture_disabled and reports false when the operator
// has not turned capture on (or no manager was built).
func (h *Handler) captureEnabled(w http.ResponseWriter) bool {
	if h.Capture == nil || !h.Capture.Enabled() {
		respondError(w, http.StatusForbidden, "capture_disabled")
		return false
	}
	return true
}

// StartCapture begins recording for one running session.
func (h *Handler) StartCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	session, ok := h.ownedRunningSession(w, r)
	if !ok {
		return
	}
	sid := session.ID.String()
	addr, err := h.cache.Get(r.Context(), "session:"+sid)
	if err != nil || addr == "" {
		respondError(w, http.StatusConflict, "container_not_ready")
		return
	}

	res, err := h.Capture.Start(r.Context(), sid, session.TargetURL, addr)
	if err != nil {
		respondCaptureError(w, err)
		return
	}
	setSensitiveNoStore(w)
	respondJSON(w, http.StatusOK, map[string]any{
		"archive_id":  res.ArchiveID,
		"archive_key": res.ArchiveKey,
		"started_at":  res.StartedAt.UTC().Format(time.RFC3339Nano),
	})
}

// StopCapture ends recording, performs the final pull and returns the totals.
func (h *Handler) StopCapture(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	session, ok := h.ownedRunningSession(w, r)
	if !ok {
		return
	}

	res, err := h.Capture.Stop(r.Context(), session.ID.String())
	if err != nil {
		respondCaptureError(w, err)
		return
	}
	setSensitiveNoStore(w)
	respondJSON(w, http.StatusOK, map[string]any{
		"items":    res.Items,
		"bytes":    res.Bytes,
		"deduped":  res.Deduped,
		"complete": res.Complete,
	})
}

// GetCaptureStatus reports the live recording state for one session.
func (h *Handler) GetCaptureStatus(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	session, ok := h.ownedRunningSession(w, r)
	if !ok {
		return
	}
	sid := session.ID.String()
	st, err := h.Capture.Status(r.Context(), sid)
	if err != nil {
		respondCaptureError(w, err)
		return
	}
	body := map[string]any{
		"archive_id": sid,
		"session_id": st.SessionID,
		"recording":  st.Recording,
		"items":      st.Items,
		"bytes":      st.Bytes,
		"deduped":    st.Deduped,
		"truncated":  st.Truncated,
	}
	if st.ArchiveKey != "" {
		// Owner-only handler: the key lets a viewer that attached mid-recording
		// browse the archive after Stop. It is not persisted server-side.
		body["archive_key"] = st.ArchiveKey
	}
	if !st.StartedAt.IsZero() {
		body["started_at"] = st.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	setSensitiveNoStore(w)
	respondJSON(w, http.StatusOK, body)
}

type captureSummaryJSON struct {
	ArchiveID string `json:"archive_id"`
	SessionID string `json:"session_id"`
	TargetURL string `json:"target_url,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	Complete  bool   `json:"complete"`
	Items     int    `json:"items"`
	Bytes     int64  `json:"bytes"`
}

// ListCaptures is the operator listing: same bearer posture as session creation.
func (h *Handler) ListCaptures(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	if err := h.authorizedForSessionCreate(r); err != nil {
		respondError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	list, err := h.Capture.List()
	if err != nil {
		respondCaptureError(w, err)
		return
	}
	out := make([]captureSummaryJSON, 0, len(list))
	for _, s := range list {
		row := captureSummaryJSON{
			ArchiveID: s.ArchiveID,
			SessionID: s.SessionID,
			TargetURL: s.TargetURL,
			Complete:  s.Complete,
			Items:     s.Items,
			Bytes:     s.Bytes,
		}
		if !s.CreatedAt.IsZero() {
			row.CreatedAt = s.CreatedAt.UTC().Format(time.RFC3339)
		}
		if !s.EndedAt.IsZero() {
			row.EndedAt = s.EndedAt.UTC().Format(time.RFC3339)
		}
		out = append(out, row)
	}
	var usedBytes int64
	for _, s := range list {
		usedBytes += s.Bytes
	}
	quotaBytes, retentionDays := h.Capture.Quota()
	setSensitiveNoStore(w)
	respondJSON(w, http.StatusOK, map[string]any{
		"captures": out,
		"usage": map[string]any{
			"used_bytes":     usedBytes,
			"quota_bytes":    quotaBytes,
			"retention_days": retentionDays,
		},
	})
}

// GetArchive returns meta.json for one archive. The archive key is the only
// credential; a missing archive answers 404 before the key is checked.
func (h *Handler) GetArchive(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	id := chi.URLParam(r, "archiveID")
	if _, err := h.Capture.Get(id); err != nil {
		respondCaptureError(w, err)
		return
	}
	if !h.Capture.CheckKey(id, archiveKeyFromRequest(r)) {
		respondError(w, http.StatusForbidden, "archive_key_invalid")
		return
	}
	archive, err := h.Capture.Get(id)
	if err != nil {
		respondCaptureError(w, err)
		return
	}
	setSensitiveNoStore(w)
	respondJSON(w, http.StatusOK, archive.Meta)
}

// GetArchiveBlob serves one archived body.
func (h *Handler) GetArchiveBlob(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	id := chi.URLParam(r, "archiveID")
	sha := chi.URLParam(r, "sha256")
	path, mime, filename, err := h.Capture.Blob(id, sha)
	if err != nil {
		respondCaptureError(w, err)
		return
	}
	if !h.Capture.CheckKey(id, archiveKeyFromRequest(r)) {
		respondError(w, http.StatusForbidden, "archive_key_invalid")
		return
	}
	h.serveArchiveFile(w, r, path, mime, filename)
}

// GetArchiveFile serves one archived body by its readable files/<rel> path.
func (h *Handler) GetArchiveFile(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	id := chi.URLParam(r, "archiveID")
	// The wildcard is everything after "/files/" (no prefix), but callers and
	// proxies may hand us the full archive-relative path, with a leading slash.
	rel := chi.URLParam(r, "*")
	if decoded, err := url.PathUnescape(rel); err == nil {
		rel = decoded
	}
	rel = strings.TrimPrefix(strings.TrimPrefix(rel, "/"), "files/")
	path, mime, filename, err := h.Capture.File(id, "files/"+rel)
	if err != nil {
		respondCaptureError(w, err)
		return
	}
	if !h.Capture.CheckKey(id, archiveKeyFromRequest(r)) {
		respondError(w, http.StatusForbidden, "archive_key_invalid")
		return
	}
	h.serveArchiveFile(w, r, path, mime, filename)
}

// serveArchiveFile writes one archive file with the archive download headers.
func (h *Handler) serveArchiveFile(w http.ResponseWriter, r *http.Request, path, mime, filename string) {
	f, err := os.Open(path)
	if err != nil {
		respondCaptureError(w, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		respondCaptureError(w, err)
		return
	}

	extendCaptureDeadline(w, captureBlobWriteDeadline)
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Disposition", contentDisposition(filename))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, filename, info.ModTime(), f)
}

// DownloadArchive streams a zip of meta.json plus every blob.
func (h *Handler) DownloadArchive(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	id := chi.URLParam(r, "archiveID")
	if _, err := h.Capture.Get(id); err != nil {
		respondCaptureError(w, err)
		return
	}
	if !h.Capture.CheckKey(id, archiveKeyFromRequest(r)) {
		respondError(w, http.StatusForbidden, "archive_key_invalid")
		return
	}

	extendCaptureDeadline(w, captureZipWriteDeadline)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(id+".zip"))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := h.Capture.Zip(w, id); err != nil {
		// Headers are already committed; the client sees a truncated archive.
		log.Printf("capture: zip %s: %v", id, err)
	}
}

// DeleteArchive removes an archive directory. The archive key or the operator
// bearer (the create-gate posture) authorizes it.
func (h *Handler) DeleteArchive(w http.ResponseWriter, r *http.Request) {
	if !h.captureEnabled(w) {
		return
	}
	id := chi.URLParam(r, "archiveID")
	if _, err := h.Capture.Get(id); err != nil {
		respondCaptureError(w, err)
		return
	}
	authorized := h.Capture.CheckKey(id, archiveKeyFromRequest(r))
	if !authorized && h.authorizedForSessionCreate(r) == nil {
		authorized = true
	}
	if !authorized {
		respondError(w, http.StatusForbidden, "archive_key_invalid")
		return
	}
	if err := h.Capture.Delete(id); err != nil {
		respondCaptureError(w, err)
		return
	}
	setSensitiveNoStore(w)
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// respondCaptureError maps capture sentinels onto the API error contract.
func respondCaptureError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, capture.ErrDisabled):
		respondError(w, http.StatusForbidden, "capture_disabled")
	case errors.Is(err, capture.ErrQuotaExceeded):
		respondError(w, http.StatusConflict, "capture_quota_exceeded")
	case errors.Is(err, capture.ErrNotRecording):
		respondError(w, http.StatusConflict, "capture_not_recording")
	case errors.Is(err, capture.ErrArchiveNotFound), errors.Is(err, capture.ErrInvalidArchiveID):
		respondError(w, http.StatusNotFound, "archive_not_found")
	case errors.Is(err, capture.ErrBlobNotFound), errors.Is(err, capture.ErrInvalidHash):
		respondError(w, http.StatusNotFound, "blob_not_found")
	case errors.Is(err, capture.ErrInvalidPath):
		respondError(w, http.StatusBadRequest, "invalid_path")
	case errors.Is(err, capture.ErrAlreadyRecording):
		respondError(w, http.StatusConflict, "capture_already_recording")
	case errors.Is(err, capture.ErrArchiveExists):
		respondError(w, http.StatusConflict, "archive_exists")
	default:
		respondError(w, http.StatusBadGateway, "capture_failed")
	}
}

// archiveKeyFromRequest reads `Authorization: Bearer <archive_key>`.
func archiveKeyFromRequest(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "bearer "
	if len(raw) <= len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(raw[len(prefix):])
}

// contentDisposition formats an attachment header without allowing header
// injection from an item filename.
func contentDisposition(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = strings.NewReplacer("\"", "", "\\", "", "\r", "", "\n", "").Replace(name)
	if name == "" || name == "." {
		name = "download"
	}
	return `attachment; filename="` + name + `"`
}

// extendCaptureDeadline lifts the server's 15s write deadline for one streamed
// response, mirroring GetResourceGraph.
func extendCaptureDeadline(w http.ResponseWriter, d time.Duration) {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d)); err != nil &&
		!errors.Is(err, http.ErrNotSupported) {
		log.Printf("capture: extend write deadline: %v", err)
	}
}
