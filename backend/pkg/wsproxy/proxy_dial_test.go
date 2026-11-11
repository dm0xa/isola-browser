package wsproxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// TestHandleWebSocketRefusesDeadContainerBeforeUpgrade pins the handshake order
// the viewer's reconnect backoff depends on.
//
// The relay used to upgrade the client first and dial the container second, so a
// dead sandbox still produced a 101 and `onopen` in the browser. The viewer
// resets its retry delay on open, so that made the backoff cap unreachable: a
// dead session was retried twice a second until its row expired. With the dial
// first, the handshake fails with 502 and the cap engages.
func TestHandleWebSocketRefusesDeadContainerBeforeUpgrade(t *testing.T) {
	// Reserve a port, then release it: the relay resolves the session to an
	// address that refuses connections, exactly like a container that OOMed.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	sid := uuid.New()
	cache := store.NewMemCache()
	ctx := context.Background()
	if err := cache.Set(ctx, "session:"+sid.String(), addr, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := cache.Set(ctx, "session:"+sid.String()+":control_token", "test-token", time.Minute); err != nil {
		t.Fatal(err)
	}

	// st/orch stay nil: the reap is skipped, which keeps this a pure relay test.
	p := New(cache, nil, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("sessionId", sid.String())
		p.HandleWebSocket(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx)))
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/" + sid.String() + "?channel=isola&session_token=test-token"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Origin": {srv.URL}})
	if err == nil {
		conn.Close()
		t.Fatal("dial succeeded: the relay upgraded the client even though the container is gone")
	}
	if resp == nil {
		t.Fatalf("dial failed without an HTTP response: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("handshake status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}
