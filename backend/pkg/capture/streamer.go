package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamerClient talks to the in-container capture endpoints over the
// loopback-published streamer port. Every response is bounded and every hex
// parameter is validated before it is put into a URL.
type streamerClient struct {
	base string
	http *http.Client
}

func newStreamerClient(addr string) *streamerClient {
	return &streamerClient{
		base: "http://" + addr,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

type streamerMeta struct {
	SchemaVersion int              `json:"schema_version"`
	RecordID      string           `json:"record_id"`
	SessionID     string           `json:"session_id"`
	StartedAt     string           `json:"started_at"`
	EndedAt       *string          `json:"ended_at"`
	Recording     bool             `json:"recording"`
	StopReason    string           `json:"stop_reason"`
	Counts        map[string]int64 `json:"counts"`
	Sizes         map[string]int64 `json:"sizes"`
	Truncated     bool             `json:"truncated"`
}

type streamerItem struct {
	ID                string `json:"id"`
	URL               string `json:"url"`
	FinalURL          string `json:"final_url"`
	Method            string `json:"method"`
	Status            int    `json:"status"`
	Mime              string `json:"mime"`
	Type              string `json:"type"`
	SizeWire          int64  `json:"size_wire"`
	SizeDecoded       int64  `json:"size_decoded"`
	SHA256            string `json:"sha256"`
	SHA1              string `json:"sha1"`
	Filename          string `json:"filename"`
	Tab               int    `json:"tab"`
	OriginHost        string `json:"origin_host"`
	TSRequested       int64  `json:"ts_requested"`
	TSFinished        int64  `json:"ts_finished"`
	TSSaved           int64  `json:"ts_saved"`
	FromDiskCache     bool   `json:"from_disk_cache"`
	FromServiceWorker bool   `json:"from_service_worker"`
	State             string `json:"state"`
}

type streamerManifest struct {
	Meta  streamerMeta   `json:"meta"`
	Items []streamerItem `json:"items"`
}

type streamerStart struct {
	OK        bool   `json:"ok"`
	RecordID  string `json:"record_id"`
	StartedAt string `json:"started_at"`
}

type streamerStop struct {
	Items     int    `json:"items"`
	Bytes     int64  `json:"bytes"`
	Deduped   int64  `json:"deduped"`
	Complete  bool   `json:"complete"`
	Truncated bool   `json:"truncated"`
	EndedAt   string `json:"ended_at"`
}

type streamerStatus struct {
	Recording bool             `json:"recording"`
	SessionID string           `json:"session_id"`
	StartedAt string           `json:"started_at"`
	Items     int              `json:"items"`
	Bytes     int64            `json:"bytes"`
	Deduped   int64            `json:"deduped"`
	Truncated bool             `json:"truncated"`
	Counts    map[string]int64 `json:"counts"`
}

func (c *streamerClient) do(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("capture: streamer %s %s: HTTP %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return resp, nil
}

func (c *streamerClient) start(ctx context.Context, sessionID string, maxFile, maxRecord int64) (streamerStart, error) {
	resp, err := c.do(ctx, http.MethodPost, "/capture/start", map[string]any{
		"session_id":       sessionID,
		"max_file_bytes":   maxFile,
		"max_record_bytes": maxRecord,
	})
	if err != nil {
		return streamerStart{}, err
	}
	defer resp.Body.Close()
	var out streamerStart
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return streamerStart{}, fmt.Errorf("capture: decode start response: %w", err)
	}
	return out, nil
}

func (c *streamerClient) stop(ctx context.Context) (streamerStop, error) {
	resp, err := c.do(ctx, http.MethodPost, "/capture/stop", map[string]any{})
	if err != nil {
		return streamerStop{}, err
	}
	defer resp.Body.Close()
	var out streamerStop
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return streamerStop{}, fmt.Errorf("capture: decode stop response: %w", err)
	}
	return out, nil
}

func (c *streamerClient) status(ctx context.Context) (streamerStatus, error) {
	resp, err := c.do(ctx, http.MethodGet, "/capture/status", nil)
	if err != nil {
		return streamerStatus{}, err
	}
	defer resp.Body.Close()
	var out streamerStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return streamerStatus{}, fmt.Errorf("capture: decode status response: %w", err)
	}
	return out, nil
}

func (c *streamerClient) manifest(ctx context.Context) (streamerManifest, error) {
	resp, err := c.do(ctx, http.MethodGet, "/capture/manifest", nil)
	if err != nil {
		return streamerManifest{}, err
	}
	defer resp.Body.Close()
	var out streamerManifest
	// The manifest can be large (metadata for thousands of requests), so it is
	// bounded generously rather than by the tiny control responses' limit.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return streamerManifest{}, fmt.Errorf("capture: decode manifest: %w", err)
	}
	return out, nil
}

// blob fetches one verified-on-arrival blob. The body is bounded by the caller's
// max file size; the streamer's Content-Type/download headers are irrelevant.
func (c *streamerClient) blob(ctx context.Context, sha string, maxBytes int64) ([]byte, error) {
	if !validSHA256(sha) {
		return nil, ErrInvalidHash
	}
	resp, err := c.do(ctx, http.MethodGet, "/capture/blob?sha256="+sha, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("capture: read blob %s: %w", sha, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%w: %s exceeds %d bytes", errBlobTooLarge, sha, maxBytes)
	}
	return data, nil
}
