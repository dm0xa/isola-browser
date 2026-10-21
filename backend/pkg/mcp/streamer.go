package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// defaultStreamerTimeout bounds one streamer HTTP call. Without it a wedged
// renderer pins the MCP handler until the client gives up.
const defaultStreamerTimeout = 15 * time.Second

// ErrResponseTooLarge reports that a streamer response exceeded the caller's
// byte budget. Callers turn it into an actionable tool error.
var ErrResponseTooLarge = errors.New("streamer response too large")

func controlTokenKey(sessionID string) string {
	return fmt.Sprintf("session:%s:control_token", sessionID)
}

func sessionAddrKey(sessionID string) string {
	return fmt.Sprintf("session:%s", sessionID)
}

func (s *Service) containerAddr(ctx context.Context, sessionID string) (string, error) {
	addr, err := s.deps.Cache.Get(ctx, sessionAddrKey(sessionID))
	if store.IsKeyNotFound(err) {
		return "", fmt.Errorf("session %s has no container address (not running yet?)", sessionID)
	}
	if err != nil {
		return "", err
	}
	return addr, nil
}

func (s *Service) streamerGET(ctx context.Context, sessionID, path string) ([]byte, int, error) {
	return s.streamerGETLimited(ctx, sessionID, path, defaultStreamerTimeout, 0)
}

// streamerGETLimited is streamerGET with an explicit deadline and byte cap.
// maxBytes <= 0 means unbounded.
func (s *Service) streamerGETLimited(ctx context.Context, sessionID, path string, timeout time.Duration, maxBytes int64) ([]byte, int, error) {
	addr, err := s.containerAddr(ctx, sessionID)
	if err != nil {
		return nil, 0, err
	}

	reqCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	var body []byte
	if maxBytes > 0 {
		body, err = io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
		if err == nil && int64(len(body)) > maxBytes {
			return body, resp.StatusCode, fmt.Errorf("%w: %s exceeds %d bytes", ErrResponseTooLarge, path, maxBytes)
		}
	} else {
		body, err = io.ReadAll(resp.Body)
	}
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return body, resp.StatusCode, fmt.Errorf("streamer %s: %s", path, strings.TrimSpace(string(body)))
	}
	return body, resp.StatusCode, nil
}

func (s *Service) streamerPOST(ctx context.Context, sessionID, path string, payload any) ([]byte, int, error) {
	addr, err := s.containerAddr(ctx, sessionID)
	if err != nil {
		return nil, 0, err
	}
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		bodyReader = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path, bodyReader)
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 300 {
		return body, resp.StatusCode, fmt.Errorf("streamer %s: %s", path, strings.TrimSpace(string(body)))
	}
	return body, resp.StatusCode, nil
}

func (s *Service) navigateStreamer(ctx context.Context, sessionID, targetURL string) error {
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		status, err := s.navigateStreamerOnce(ctx, sessionID, targetURL)
		if err == nil {
			return nil
		}
		lastErr = err
		if status == http.StatusServiceUnavailable {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return err
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("navigate: timed out waiting for runner ctrl socket")
}

func (s *Service) navigateStreamerOnce(ctx context.Context, sessionID, targetURL string) (int, error) {
	addr, err := s.containerAddr(ctx, sessionID)
	if err != nil {
		return 0, err
	}
	u := fmt.Sprintf("http://%s/navigate?url=%s", addr, url.QueryEscape(targetURL))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("navigate: %s", strings.TrimSpace(string(b)))
	}
	return resp.StatusCode, nil
}

// waitAutomationReady blocks until the streamer can serve snapshot (CDP + AX tree).
// Right after container start, /navigate may return 503 until the runner ctrl socket connects.
func (s *Service) waitAutomationReady(ctx context.Context, sessionID string) error {
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, code, err := s.streamerGET(ctx, sessionID, "/snapshot")
		if err == nil && code == http.StatusOK {
			return nil
		}
		if code == http.StatusServiceUnavailable {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for automation (snapshot/CDP not ready)")
}

// waitForDocumentNetwork polls until a Document request appears in the network log
// (status may be 403 on bot-protected sites — that still counts as loaded).
func (s *Service) waitForDocumentNetwork(ctx context.Context, sessionID string, maxWait time.Duration) bool {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return false
		}
		body, code, err := s.streamerGET(ctx, sessionID, "/network")
		if err == nil && code == http.StatusOK {
			var parsed struct {
				Entries []struct {
					Type   string `json:"type"`
					Status int    `json:"status"`
				} `json:"entries"`
			}
			if json.Unmarshal(body, &parsed) == nil {
				for _, e := range parsed.Entries {
					if strings.EqualFold(e.Type, "Document") && e.Status > 0 {
						return true
					}
				}
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return false
}

func (s *Service) sessionInspectMap(ctx context.Context, sessionID string) (map[string]any, error) {
	snapBody, _, err := s.streamerGET(ctx, sessionID, "/snapshot")
	if err != nil {
		return nil, err
	}
	netBody, _, err := s.streamerGET(ctx, sessionID, "/network")
	if err != nil {
		return nil, err
	}
	return buildInspectSummary(snapBody, netBody), nil
}

const waitRunningTimeout = 90 * time.Second

func (s *Service) waitSessionRunning(ctx context.Context, sessionID string) error {
	deadline := time.Now().Add(waitRunningTimeout)
	id, err := parseUUID(sessionID)
	if err != nil {
		return err
	}
	for time.Now().Before(deadline) {
		sess, err := s.deps.Store.Sessions.GetSession(ctx, id)
		if err != nil || sess == nil {
			return fmt.Errorf("session not found")
		}
		switch sess.Status {
		case "running":
			if _, err := s.containerAddr(ctx, sessionID); err == nil {
				return nil
			}
		case "error", "terminated":
			return fmt.Errorf("session entered status %s", sess.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for session to become running")
}
