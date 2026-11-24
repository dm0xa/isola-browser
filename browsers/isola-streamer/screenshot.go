package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func screenshotHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "png"
	}
	if format != "png" && format != "jpeg" {
		http.Error(w, "format must be png or jpeg", http.StatusBadRequest)
		return
	}
	params := map[string]interface{}{
		"format":                format,
		"fromSurface":           true,
		"captureBeyondViewport": false,
	}
	if format == "jpeg" {
		q := 85
		if qs := strings.TrimSpace(r.URL.Query().Get("quality")); qs != "" {
			if n, err := strconv.Atoi(qs); err == nil && n >= 1 && n <= 100 {
				q = n
			}
		}
		params["quality"] = q
	}

	raw, err := captureScreenshotWithRetry(params, screenshotRetryBudget)
	if err != nil {
		log.Printf("screenshot: captureScreenshot: %v", err)
		if errors.Is(err, errCDPUnavailable) {
			http.Error(w, "cdp not available", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "screenshot failed", http.StatusInternalServerError)
		return
	}
	var parsed struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Data == "" {
		http.Error(w, "screenshot empty", http.StatusInternalServerError)
		return
	}

	width, height := currentCaptureSize()
	mime := "image/png"
	if format == "jpeg" {
		mime = "image/jpeg"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"format":      format,
		"mime":        mime,
		"width":       width,
		"height":      height,
		"data_base64": parsed.Data,
	})
}

func currentCaptureSize() (int, int) {
	currentMu.Lock()
	w, h := currentWidth, currentHeight
	currentMu.Unlock()
	if w > 0 && h > 0 {
		return w, h
	}
	return 1280, 720
}

// screenshotRetryBudget covers the gap while the CDP client re-attaches to the
// new page after a navigation. A screenshot taken in that window used to fail
// with the opaque "screenshot failed" and no hint to retry.
const screenshotRetryBudget = 4 * time.Second

var errCDPUnavailable = errors.New("cdp not available")

// captureScreenshotWithRetry waits out a CDP re-attach instead of failing the
// first attempt. Non-transient CDP errors (an unknown method, a bad parameter)
// return immediately.
func captureScreenshotWithRetry(params map[string]interface{}, budget time.Duration) ([]byte, error) {
	deadline := time.Now().Add(budget)
	var lastErr error = errCDPUnavailable
	for {
		client := cdpClient
		if client != nil && client.Available() {
			raw, err := client.callWithResult("Page.captureScreenshot", params, 15*time.Second)
			if err == nil {
				return raw, nil
			}
			lastErr = err
			if !retryableScreenshotError(err) {
				return nil, err
			}
		}
		if time.Now().After(deadline) {
			return nil, lastErr
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// retryableScreenshotError matches the connection-level failures a navigation's
// CDP re-attach produces.
func retryableScreenshotError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"connection closed",
		"cdp not connected",
		"broken pipe",
		"use of closed network connection",
		"websocket: close",
		"unexpected eof",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}
