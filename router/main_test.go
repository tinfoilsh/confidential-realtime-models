package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

const testDomain = "realtime.tinfoil.sh"

// --- root domain ---

func TestRootHealth(t *testing.T) {
	handler := newTestHandler(t)
	req := newReq(http.MethodGet, "/health", nil, testDomain)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Fatalf("expected ok body, got %q", got)
	}
}

func TestRootHealthRouterAlias(t *testing.T) {
	handler := newTestHandler(t)
	req := newReq(http.MethodGet, "/health/router", nil, testDomain)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRootDomainOtherPathIsNotFound(t *testing.T) {
	handler := newTestHandler(t)
	req := newReq(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{}`), testDomain)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on root domain non-health, got %d", rec.Code)
	}
}

// --- subdomain dispatch ---

func TestQwenSubdomainProxiesSpeech(t *testing.T) {
	var gotPath, gotModel string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	handler := newTestHandlerWith(t, "qwen", backend.URL)
	req := newReq(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"qwen3-tts","input":"hi"}`),
		"qwen3-tts."+testDomain)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("expected backend path /v1/audio/speech, got %q", gotPath)
	}
	if gotModel != "qwen3-tts" {
		t.Fatalf("expected model qwen3-tts forwarded, got %q", gotModel)
	}
}

func TestVoxtralTTSSubdomainProxiesSpeech(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	handler := newTestHandlerWith(t, "voxtral_tts", backend.URL)
	req := newReq(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"input":"hi"}`),
		"voxtral-tts."+testDomain)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("expected backend path /v1/audio/speech, got %q", gotPath)
	}
}

func TestVoxtralMiniRealtimeSubdomainHealth(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	handler := newTestHandlerWith(t, "voxtral_mini", backend.URL)
	req := newReq(http.MethodGet, "/health", nil, "voxtral-mini-4b-realtime."+testDomain)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotPath != "/health" {
		t.Fatalf("expected backend path /health, got %q", gotPath)
	}
}

func TestUnknownSubdomainReturns404(t *testing.T) {
	handler := newTestHandler(t)
	req := newReq(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{}`),
		"does-not-exist."+testDomain)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown subdomain, got %d", rec.Code)
	}
}

// Foreign hosts (anything not ending in `.<domain>` and not equal to <domain>
// itself) are treated like the root domain: only /health is served, everything
// else 404s. This intentionally lets internal /health probes work regardless
// of how they spell the Host header.
func TestForeignDomainServesOnlyHealth(t *testing.T) {
	handler := newTestHandler(t)

	healthReq := newReq(http.MethodGet, "/health", nil, "qwen3-tts.example.com")
	healthRec := httptest.NewRecorder()
	handler.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Fatalf("expected 200 on foreign domain /health, got %d", healthRec.Code)
	}

	speechReq := newReq(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{}`), "qwen3-tts.example.com")
	speechRec := httptest.NewRecorder()
	handler.ServeHTTP(speechRec, speechReq)
	if speechRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 on foreign domain /v1/audio/speech, got %d", speechRec.Code)
	}
}

// --- X-Forwarded-Host takes precedence over Host ---

func TestXForwardedHostWins(t *testing.T) {
	var gotPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	handler := newTestHandlerWith(t, "qwen", backend.URL)
	// Direct Host points at root domain (would be a 404), but the shim
	// forwarded the original `qwen3-tts.realtime.tinfoil.sh` upstream.
	req := newReq(http.MethodPost, "/v1/audio/speech", strings.NewReader(`{"model":"qwen3-tts"}`), testDomain)
	req.Header.Set("X-Forwarded-Host", "qwen3-tts."+testDomain)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via X-Forwarded-Host, got %d", rec.Code)
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("unexpected backend path %q", gotPath)
	}
}

// --- query string preserved across the proxy ---

func TestProxyPreservesQueryString(t *testing.T) {
	var gotQuery url.Values
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	handler := newTestHandlerWith(t, "qwen", backend.URL)
	req := newReq(http.MethodPost, "/v1/audio/speech?format=wav", strings.NewReader(`{}`),
		"qwen3-tts."+testDomain)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if gotQuery.Get("format") != "wav" {
		t.Fatalf("expected query format=wav, got %q", gotQuery.Get("format"))
	}
}

// --- WebSocket realtime upgrade ---

func TestWebSocketRealtimeProxy(t *testing.T) {
	upgrader := websocket.Upgrader{
		Subprotocols:    []string{"realtime"},
		CheckOrigin:     func(r *http.Request) bool { return true },
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade backend websocket: %v", err)
		}
		defer conn.Close()

		mt, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read backend websocket: %v", err)
		}
		_ = conn.WriteMessage(mt, append([]byte("echo:"), msg...))
	}))
	defer backend.Close()

	handler := newTestHandlerWith(t, "voxtral_mini", backend.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// gorilla's dialer sets r.Host to the proxy's address, so inject
		// X-Forwarded-Host to simulate the shim handing us the original Host.
		r.Header.Set("X-Forwarded-Host", "voxtral-mini-4b-realtime."+testDomain)
		handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/realtime"
	dialer := websocket.DefaultDialer
	dialer.Subprotocols = []string{"realtime"}
	conn, _, err := dialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial proxy websocket: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write websocket: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket: %v", err)
	}
	if got := string(payload); got != "echo:ping" {
		t.Fatalf("unexpected websocket payload %q", got)
	}
}

// --- buildBackends validation ---

func TestBuildBackendsRejectsInvalidURL(t *testing.T) {
	_, err := newHandler(config{
		listenAddr:             ":0",
		domain:                 testDomain,
		qwenTTSURL:             "http://127.0.0.1:8505",
		voxtralTTSURL:          "://bad",
		voxtralMiniRealtimeURL: "http://127.0.0.1:8402",
	})
	if err == nil {
		t.Fatal("expected invalid url error")
	}
}

func TestBuildBackendsRequiresSchemeAndHost(t *testing.T) {
	_, err := newHandler(config{
		listenAddr:             ":0",
		domain:                 testDomain,
		qwenTTSURL:             "/relative",
		voxtralTTSURL:          "http://127.0.0.1:8605",
		voxtralMiniRealtimeURL: "http://127.0.0.1:8402",
	})
	if err == nil || !strings.Contains(err.Error(), "scheme and host") {
		t.Fatalf("expected scheme and host error, got %v", err)
	}
}

func TestNewHandlerRequiresDomain(t *testing.T) {
	_, err := newHandler(config{
		listenAddr:             ":0",
		domain:                 "",
		qwenTTSURL:             "http://127.0.0.1:8505",
		voxtralTTSURL:          "http://127.0.0.1:8605",
		voxtralMiniRealtimeURL: "http://127.0.0.1:8402",
	})
	if err == nil {
		t.Fatal("expected DOMAIN required error")
	}
}

// --- helpers ---

// newReq builds an httptest request whose Host header is the given fqdn.
func newReq(method, path string, body io.Reader, host string) *http.Request {
	req := httptest.NewRequest(method, path, body)
	req.Host = host
	return req
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandlerWith(t, "", "")
}

// newTestHandlerWith returns a handler whose specific backend is replaced
// with the given httptest server URL. backend names: "qwen", "voxtral_tts",
// "voxtral_mini".
func newTestHandlerWith(t *testing.T, name, backendURL string) http.Handler {
	t.Helper()

	cfg := config{
		listenAddr:             ":0",
		domain:                 testDomain,
		qwenTTSURL:             "http://127.0.0.1:8505",
		voxtralTTSURL:          "http://127.0.0.1:8605",
		voxtralMiniRealtimeURL: "http://127.0.0.1:8402",
	}

	switch name {
	case "":
		// no override
	case "qwen":
		cfg.qwenTTSURL = backendURL
	case "voxtral_tts":
		cfg.voxtralTTSURL = backendURL
	case "voxtral_mini":
		cfg.voxtralMiniRealtimeURL = backendURL
	default:
		t.Fatalf("unknown backend override %q", name)
	}

	handler, err := newHandler(cfg)
	if err != nil {
		t.Fatalf("newHandler failed: %v", err)
	}
	return handler
}
