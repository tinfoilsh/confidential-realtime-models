package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestHealth(t *testing.T) {
	handler := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Fatalf("expected ok body, got %q", got)
	}
}

func TestRouterHealthAlias(t *testing.T) {
	handler := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/health/router", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Fatalf("expected ok body, got %q", got)
	}
}

func TestSpeechJSONProxy(t *testing.T) {
	var gotPath, gotModel, gotInput string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		defer r.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode backend request: %v", err)
		}
		gotModel, _ = body["model"].(string)
		gotInput, _ = body["input"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer backend.Close()

	handler := newTestHandlerWithOverrides(t, map[string]string{"qwen": backend.URL})
	body := strings.NewReader(`{"model":"qwen3-tts","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotPath != "/v1/audio/speech" {
		t.Fatalf("expected backend path /v1/audio/speech, got %q", gotPath)
	}
	if gotModel != "qwen3-tts" || gotInput != "hello" {
		t.Fatalf("unexpected proxied body: model=%q input=%q", gotModel, gotInput)
	}
}

func TestBackendHealthRewrite(t *testing.T) {
	var gotPath string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	handler := newTestHandlerWithOverrides(t, map[string]string{"qwen": backend.URL})
	req := httptest.NewRequest(http.MethodGet, "/health/qwen", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotPath != "/health" {
		t.Fatalf("expected backend path /health, got %q", gotPath)
	}
}

func TestBackendMetricsRewrite(t *testing.T) {
	var gotPath string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("metric 1"))
	}))
	defer backend.Close()

	handler := newTestHandlerWithOverrides(t, map[string]string{"qwen": backend.URL})
	req := httptest.NewRequest(http.MethodGet, "/metrics/qwen", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotPath != "/metrics" {
		t.Fatalf("expected backend path /metrics, got %q", gotPath)
	}
}

func TestWebsocketProxy(t *testing.T) {
	upgrader := websocket.Upgrader{}
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
			t.Fatalf("read backend websocket message: %v", err)
		}
		if err := conn.WriteMessage(mt, append([]byte("echo:"), msg...)); err != nil {
			t.Fatalf("write backend websocket message: %v", err)
		}
	}))
	defer backend.Close()

	handler := newTestHandlerWithOverrides(t, map[string]string{"voxtral": backend.URL})
	proxy := httptest.NewServer(handler)
	defer proxy.Close()

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/realtime"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial proxy websocket: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
		t.Fatalf("write websocket message: %v", err)
	}
	_, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read websocket response: %v", err)
	}
	if got := string(payload); got != "echo:ping" {
		t.Fatalf("unexpected websocket payload %q", got)
	}
}

func TestNotFound(t *testing.T) {
	handler := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/does-not-exist", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func newTestHandler(t *testing.T) http.Handler {
	t.Helper()
	return newTestHandlerWithOverrides(t, nil)
}

func newTestHandlerWithOverrides(t *testing.T, overrides map[string]string) http.Handler {
	t.Helper()

	cfg := config{
		listenAddr: ":0",
		qwenURL:    "http://127.0.0.1:8505",
		voxtralURL: "http://127.0.0.1:8402",
	}

	for name, raw := range overrides {
		switch name {
		case "qwen":
			cfg.qwenURL = raw
		case "voxtral":
			cfg.voxtralURL = raw
		default:
			t.Fatalf("unknown override name %q", name)
		}
	}

	handler, err := newHandler(cfg)
	if err != nil {
		t.Fatalf("newHandler failed: %v", err)
	}
	return handler
}

func TestBuildRoutesRejectsInvalidURL(t *testing.T) {
	_, err := buildRoutes(config{
		listenAddr: ":8080",
		qwenURL:    "http://127.0.0.1:8505",
		voxtralURL: "://bad",
	})
	if err == nil {
		t.Fatal("expected invalid url error")
	}
}

func TestBuildRoutesRejectsInvalidQwenURL(t *testing.T) {
	_, err := buildRoutes(config{
		listenAddr: ":8080",
		qwenURL:    "://bad",
		voxtralURL: "http://127.0.0.1:8402",
	})
	if err == nil {
		t.Fatal("expected invalid url error")
	}
}

func TestBuildRoutesRequiresSchemeAndHost(t *testing.T) {
	_, err := buildRoutes(config{
		listenAddr: ":8080",
		qwenURL:    "/relative",
		voxtralURL: "http://127.0.0.1:8402",
	})
	if err == nil || !strings.Contains(err.Error(), "scheme and host") {
		t.Fatalf("expected scheme and host error, got %v", err)
	}
}

func TestProxyPreservesQueryString(t *testing.T) {
	var gotQuery url.Values

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	handler := newTestHandlerWithOverrides(t, map[string]string{"qwen": backend.URL})
	req := httptest.NewRequest(http.MethodPost, "/v1/audio/speech?format=wav", strings.NewReader(`{"model":"qwen3-tts"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	if got := gotQuery.Get("format"); got != "wav" {
		t.Fatalf("expected query format=wav, got %q", got)
	}
}
