package main

import (
	"errors"
	"fmt"
	"testing"
)

func TestRetryableScreenshotError(t *testing.T) {
	t.Parallel()
	retryable := []string{
		"Page.captureScreenshot: connection closed",
		"cdp not connected",
		"write Page.captureScreenshot: write tcp 1.2.3.4:1->5.6.7.8:2: use of closed network connection",
		"write Page.captureScreenshot: websocket: close 1006 (abnormal closure)",
		"Page.captureScreenshot: unexpected EOF",
	}
	for _, msg := range retryable {
		if !retryableScreenshotError(errors.New(msg)) {
			t.Errorf("%q: want retryable", msg)
		}
	}
	permanent := []string{
		"Page.captureScreenshot: cdp error -32602: Invalid parameters",
		"Page.captureScreenshot: cdp error -32000: Not attached to an active page",
	}
	for _, msg := range permanent {
		if retryableScreenshotError(errors.New(msg)) {
			t.Errorf("%q: want permanent", msg)
		}
	}
	if retryableScreenshotError(nil) {
		t.Error("nil error classified as retryable")
	}
	if !retryableScreenshotError(fmt.Errorf("wrapped: %w", errors.New("connection closed"))) {
		t.Error("wrapped transient error not detected")
	}
}
