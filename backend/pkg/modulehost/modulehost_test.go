package modulehost

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/dm0xa/isola-browser/backend/pkg/modulesdk"
	"github.com/dm0xa/isola-browser/backend/pkg/plugin"
	"github.com/dm0xa/isola-browser/backend/pkg/registry"
)

// startFakeModule serves the contract v1 surface with test logic on a real
// UDS — the same path a separate module process would take.
func startFakeModule(t *testing.T) (socket string, done func()) {
	t.Helper()
	// A short socket dir: AF_UNIX paths are length-capped, and t.TempDir()
	// embeds the (often long) test name, which can exceed the limit on macOS.
	dir, err := createSocketDir()
	if err != nil {
		t.Fatalf("socket dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket = filepath.Join(dir, "fake.sock")

	srv := modulesdk.New(modulesdk.Info{
		Name:    "fake",
		Version: "0.0.1",
		API:     modulesdk.APIVersion,
	})

	srv.SetSessionGuard(guardFunc(func(_ context.Context, r *http.Request, userID uuid.UUID, fingerprint string) error {
		_ = r
		_ = userID
		if fingerprint == "bad" {
			return errRejected
		}
		return nil
	}))
	srv.SetURLPolicy(urlPolicyFunc(func(_ context.Context, _ *plugin.SessionContext, u string) (bool, string) {
		if strings.Contains(u, "evil.example") {
			return false, "blocked by policy"
		}
		return true, ""
	}))
	srv.SetAuditLogger(auditFunc(func(_ context.Context, ev plugin.AuditEvent) {
		lastAudit = ev
	}))
	srv.SetDLPProvider(dlpFunc(func(data []byte, direction string) ([]byte, error) {
		_ = direction
		return []byte("[dlp] " + string(data)), nil
	}))
	srv.SetWebSocket("net", func(w http.ResponseWriter, r *http.Request, sessionID string) {
		up := websocket.Upgrader{}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Echo back with the session id prefix to prove the pipe works.
		mt, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		conn.WriteMessage(mt, append([]byte(sessionID+":"), msg...))
	})

	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(l)
	return socket, func() { l.Close() }
}

// The Go SDK Server consumes the backend plugin interfaces; the small func
// shims below keep the test terse.

const errRejected = guardError("rejected by fake guard")

type guardError string

func (e guardError) Error() string { return string(e) }

type guardFunc func(ctx context.Context, r *http.Request, userID uuid.UUID, fingerprint string) error

func (f guardFunc) Info() plugin.Info { return plugin.Info{Name: "fake"} }
func (f guardFunc) BeforeCreate(ctx context.Context, r *http.Request, userID uuid.UUID, fingerprint string) error {
	return f(ctx, r, userID, fingerprint)
}

type urlPolicyFunc func(ctx context.Context, s *plugin.SessionContext, u string) (bool, string)

func (f urlPolicyFunc) Info() plugin.Info { return plugin.Info{Name: "fake"} }
func (f urlPolicyFunc) AllowURL(ctx context.Context, s *plugin.SessionContext, u string) (bool, string) {
	return f(ctx, s, u)
}

type auditFunc func(ctx context.Context, ev plugin.AuditEvent)

func (f auditFunc) Info() plugin.Info                             { return plugin.Info{Name: "fake"} }
func (f auditFunc) Log(ctx context.Context, ev plugin.AuditEvent) { f(ctx, ev) }

type dlpFunc func(data []byte, direction string) ([]byte, error)

func (f dlpFunc) Info() plugin.Info { return plugin.Info{Name: "fake"} }
func (f dlpFunc) FilterClipboard(data []byte, direction string) ([]byte, error) {
	return f(data, direction)
}

var lastAudit plugin.AuditEvent

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestRoundTripAdaptersOverUDS(t *testing.T) {
	socket, stop := startFakeModule(t)
	defer stop()

	m := &Module{man: Manifest{Module: "fake", Version: "0.0.1"}, socket: socket, client: newRPCClient(socket)}

	info := modulesdk.Info{
		Name: "fake", Version: "0.0.1", API: modulesdk.APIVersion,
		Implements: []string{
			modulesdk.KindSessionGuard, modulesdk.KindURLPolicy,
			modulesdk.KindAudit, modulesdk.KindDLP, modulesdk.KindWebSocket,
		},
		SubPath: "net",
	}
	adapters{m}.registerInRegistry(info)

	// Guard: allow path.
	guards := registry.SessionGuards()
	if len(guards) == 0 {
		t.Fatal("guard adapter not registered")
	}
	req := httptest.NewRequest("POST", "/api/sessions", nil)
	if err := guards[len(guards)-1].BeforeCreate(context.Background(), req, uuid.Nil, "ok-fp"); err != nil {
		t.Fatalf("guard should allow: %v", err)
	}

	// Guard: reject path (contract 403 → reason error).
	if err := guards[len(guards)-1].BeforeCreate(context.Background(), req, uuid.Nil, "bad"); err == nil {
		t.Fatal("guard should reject bad fingerprint")
	}

	// URL policy.
	policies := registry.URLPolicies()
	if len(policies) == 0 {
		t.Fatal("url policy adapter not registered")
	}
	if ok, _ := policies[len(policies)-1].AllowURL(context.Background(), nil, "https://evil.example/x"); ok {
		t.Fatal("url policy should block evil.example")
	}
	if ok, _ := policies[len(policies)-1].AllowURL(context.Background(), nil, "https://good.example/"); !ok {
		t.Fatal("url policy should allow good.example")
	}

	// DLP round-trip.
	providers := registry.DLPProviders()
	if len(providers) == 0 {
		t.Fatal("dlp adapter not registered")
	}
	out, err := providers[len(providers)-1].FilterClipboard([]byte("secret"), "out")
	if err != nil || string(out) != "[dlp] secret" {
		t.Fatalf("dlp round-trip failed: %q %v", out, err)
	}

	// Audit (async — poll briefly).
	loggers := registry.AuditLoggers()
	if len(loggers) == 0 {
		t.Fatal("audit adapter not registered")
	}
	loggers[len(loggers)-1].Log(context.Background(), plugin.AuditEvent{Type: "test.event"})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && lastAudit.Type != "test.event" {
		time.Sleep(10 * time.Millisecond)
	}
	if lastAudit.Type != "test.event" {
		t.Fatal("audit event did not reach the module")
	}
}

func TestWebSocketPipe(t *testing.T) {
	socket, stop := startFakeModule(t)
	defer stop()

	m := &Module{man: Manifest{Module: "fake", SubPath: "net"}, socket: socket, client: newRPCClient(socket)}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wsAdapter{m}.ServeWS(w, r, r.URL.Query().Get("session_id"))
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/sess123/net?session_id=sess123"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	conn.WriteMessage(websocket.TextMessage, []byte("ping"))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg) != "sess123:ping" {
		t.Fatalf("echo mismatch: %q", msg)
	}
}

func TestDiscoverSkipsDisabledAndMalformed(t *testing.T) {
	dir := t.TempDir()

	writePkg := func(name, manifest string) {
		t.Helper()
		pkg := filepath.Join(dir, name)
		if err := os.MkdirAll(pkg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, "plugin.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pkg, "bin"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writePkg("good", `{"module":"good","version":"1.0.0","api":"v1","backend":{"bin":"bin"}}`)
	writePkg("off", `{"module":"off","version":"1.0.0","api":"v1","backend":{"bin":"bin"}}`)
	if err := os.WriteFile(filepath.Join(dir, "off", ".disabled"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePkg("nobin", `{"module":"nobin","version":"1.0.0","api":"v1","backend":{"bin":"missing"}}`)
	// Malformed manifest.
	writePkg("broken", `{not json`)

	found, err := Discover(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Discovered{}
	for _, d := range found {
		got[d.Manifest.Module] = *d
	}
	if g, ok := got["good"]; !ok || !g.Enabled || !g.BinFound {
		t.Fatalf("good package wrong: %+v", g)
	}
	if g, ok := got["off"]; !ok || g.Enabled {
		t.Fatalf("disabled package wrong: %+v", g)
	}
	if g, ok := got["nobin"]; !ok || g.BinFound {
		t.Fatalf("missing-bin package wrong: %+v", g)
	}
	if _, ok := got["broken"]; ok {
		t.Fatal("malformed manifest should be skipped")
	}
}

// TestHostStartLaunchesRealProcess covers the full host path: manifest
// discovery, binary exec, readiness wait, and registry bridging — with a
// real child process instead of an in-process fake.
func TestHostStartLaunchesRealProcess(t *testing.T) {
	// Build the fake module binary from testdata (same module, no deps).
	binDir := t.TempDir()
	pkgDir := filepath.Join(binDir, "fakemod")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(pkgDir, "fakemod")
	build := exec.Command("go", "build", "-o", bin, "./testdata/fakemod")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build fakemod: %v", err)
	}
	manifest := `{"module":"fakemod","version":"0.0.1","api":"v1","backend":{"bin":"fakemod"},"requires":{"redis":true}}`
	if err := os.WriteFile(filepath.Join(pkgDir, "plugin.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, err := Start(ctx, Options{Dir: binDir})
	if err != nil {
		t.Fatalf("host start: %v", err)
	}
	defer h.Stop()

	statuses := h.Statuses(ctx)
	if len(statuses) != 1 || statuses[0].Name != "fakemod" || !statuses[0].Healthy {
		t.Fatalf("unexpected statuses: %+v", statuses)
	}

	// Registry got the guard bridged from the child process.
	guards := registry.SessionGuards()
	found := false
	for _, g := range guards {
		if g.Info().Name == "fakemod" {
			found = true
			if err := g.BeforeCreate(context.Background(), httptest.NewRequest("POST", "/api/sessions", nil), uuid.Nil, ""); err != nil {
				t.Fatalf("fakemod guard should allow: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("fakemod guard not registered")
	}
}
