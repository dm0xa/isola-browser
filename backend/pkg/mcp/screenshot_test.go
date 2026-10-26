package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A 1x1 PNG, the shape the streamer returns in data_base64.
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func screenshotService(t *testing.T, payload map[string]any) (*Service, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/screenshot" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)

	id := uuid.New().String()
	return newStreamerService(t, id, srv.Listener.Addr().String()), id
}

// TestToolScreenshotReturnsImageBlock pins the response shape: the pixels travel
// as an image block a client can render, and the text block carries metadata only.
func TestToolScreenshotReturnsImageBlock(t *testing.T) {
	t.Parallel()
	svc, id := screenshotService(t, map[string]any{
		"format": "png", "mime": "image/png", "width": 1, "height": 1,
		"data_base64": tinyPNGBase64,
	})

	res, _, err := svc.toolScreenshot(context.Background(), nil, &screenshotParams{SessionID: id})
	if err != nil {
		t.Fatal(err)
	}

	var images int
	var text strings.Builder
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.ImageContent:
			images++
			if v.MIMEType != "image/png" {
				t.Fatalf("mime = %q", v.MIMEType)
			}
			if len(v.Data) == 0 {
				t.Fatal("image block carries no data")
			}
		case *mcp.TextContent:
			text.WriteString(v.Text)
		}
	}
	if images != 1 {
		t.Fatalf("image blocks = %d, want 1", images)
	}
	meta := text.String()
	if strings.Contains(meta, "data_base64") {
		t.Fatalf("text block must not duplicate the base64 payload: %s", meta)
	}
	for _, want := range []string{`"format":"png"`, `"width":1`, `"bytes":`} {
		if !strings.Contains(meta, want) {
			t.Fatalf("metadata missing %s: %s", want, meta)
		}
	}
}

func TestToolScreenshotInfersMIMEFromFormat(t *testing.T) {
	t.Parallel()
	svc, id := screenshotService(t, map[string]any{
		"format": "jpeg", "width": 1, "height": 1, "data_base64": tinyPNGBase64,
	})

	res, _, err := svc.toolScreenshot(context.Background(), nil, &screenshotParams{SessionID: id, Format: "jpeg"})
	if err != nil {
		t.Fatal(err)
	}
	img, ok := res.Content[1].(*mcp.ImageContent)
	if !ok || img.MIMEType != "image/jpeg" {
		t.Fatalf("content = %+v", res.Content)
	}
}

func TestToolScreenshotRejectsMissingImageData(t *testing.T) {
	t.Parallel()
	svc, id := screenshotService(t, map[string]any{"format": "png"})

	if _, _, err := svc.toolScreenshot(context.Background(), nil, &screenshotParams{SessionID: id}); err == nil {
		t.Fatal("expected an error when the streamer returns no image data")
	}
}
