package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/dm0xa/isola-browser/backend/pkg/models"
	"github.com/dm0xa/isola-browser/backend/pkg/store"
)

// graphService starts a fake streamer and a running session wired to it.
func graphService(t *testing.T, streamer http.HandlerFunc) (*Service, string, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		streamer(w, r)
	}))
	t.Cleanup(srv.Close)

	cache := store.NewMemCache()
	sessionID := uuid.New().String()
	if err := cache.Set(context.Background(), sessionAddrKey(sessionID), srv.Listener.Addr().String(), 0); err != nil {
		t.Fatal(err)
	}

	mem := store.NewMemStore()
	st := store.NewMemBridgeStore(mem)
	sess := models.Session{ID: uuid.MustParse(sessionID), Status: models.SessionStatusRunning, TargetURL: "https://example.com"}
	if err := st.Sessions.CreateSession(context.Background(), &sess); err != nil {
		t.Fatal(err)
	}

	return &Service{deps: Deps{Store: st, Cache: cache}}, sessionID, &calls
}

func TestResourceGraphRejectsInvalidScale(t *testing.T) {
	for _, scale := range []int{-1, 5, 99} {
		svc, sessionID, calls := graphService(t, func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("PNGDATA"))
		})
		p := &resourceGraphParams{SessionID: sessionID, Scale: scale}

		if _, _, err := svc.toolResourceGraph(context.Background(), nil, p); err == nil {
			t.Fatalf("scale %d: expected an error", scale)
		} else if !strings.Contains(err.Error(), "scale must be 1-4") {
			t.Fatalf("scale %d: err = %v", scale, err)
		}
		if *calls != 0 {
			t.Fatalf("scale %d: streamer was called %d times", scale, *calls)
		}
	}
}

func TestResourceGraphForwardsCanonicalQuery(t *testing.T) {
	var gotQuery string
	svc, sessionID, _ := graphService(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte("PNGDATA"))
	})

	p := &resourceGraphParams{
		SessionID: sessionID,
		Theme:     "LIGHT",
		Expand:    "all",
		Types:     "script,stylesheet",
		Scale:     3,
	}
	if _, _, err := svc.toolResourceGraph(context.Background(), nil, p); err != nil {
		t.Fatalf("toolResourceGraph: %v", err)
	}

	for _, want := range []string{"theme=light", "expand=all", "scale=3", "type=script%2Cstylesheet", "direction=horizontal"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
	}
}

func TestResourceGraphRejectsOversizePayload(t *testing.T) {
	svc, sessionID, _ := graphService(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(strings.Repeat("x", 64)))
	})
	svc.graphMaxBytes = 16

	_, _, err := svc.toolResourceGraph(context.Background(), nil, &resourceGraphParams{SessionID: sessionID})
	if err == nil {
		t.Fatal("expected an oversize error")
	}
	if !strings.Contains(err.Error(), "16 bytes") || !strings.Contains(err.Error(), "expand=collapsed") {
		t.Fatalf("err = %v", err)
	}
}

func TestResourceGraphRespectsDeadline(t *testing.T) {
	svc, sessionID, _ := graphService(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("PNGDATA"))
	})
	svc.graphTimeout = 40 * time.Millisecond

	start := time.Now()
	_, _, err := svc.toolResourceGraph(context.Background(), nil, &resourceGraphParams{SessionID: sessionID})
	if err == nil {
		t.Fatal("expected a deadline error")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("graph call ignored its deadline: %v", elapsed)
	}
}
