package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
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

func TestEmbeddingsJSONProxy(t *testing.T) {
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

	handler := newTestHandlerWithOverrides(t, map[string]string{"nomic": backend.URL})
	body := strings.NewReader(`{"model":"nomic-embed-text","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("expected backend path /v1/embeddings, got %q", gotPath)
	}
	if gotModel != "nomic-embed-text" || gotInput != "hello" {
		t.Fatalf("unexpected proxied body: model=%q input=%q", gotModel, gotInput)
	}
}

func TestMultipartProxy(t *testing.T) {
	var gotPath, gotModel, gotFilename, gotFileData string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart form: %v", err)
		}
		gotModel = r.FormValue("model")

		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("read form file: %v", err)
		}
		defer file.Close()

		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatalf("read file data: %v", err)
		}
		gotFilename = header.Filename
		gotFileData = string(data)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	handler := newTestHandlerWithOverrides(t, map[string]string{"whisper": backend.URL})

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "whisper-large-v3-turbo"); err != nil {
		t.Fatalf("write field: %v", err)
	}
	part, err := writer.CreateFormFile("file", "sample.wav")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte("fake-audio")); err != nil {
		t.Fatalf("write file payload: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if gotPath != "/v1/audio/transcriptions" {
		t.Fatalf("expected backend path /v1/audio/transcriptions, got %q", gotPath)
	}
	if gotModel != "whisper-large-v3-turbo" {
		t.Fatalf("unexpected model %q", gotModel)
	}
	if gotFilename != "sample.wav" || gotFileData != "fake-audio" {
		t.Fatalf("unexpected file passthrough filename=%q payload=%q", gotFilename, gotFileData)
	}
}

func TestDoclingTopLevelProxy(t *testing.T) {
	var gotPath string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer backend.Close()

	handler := newTestHandlerWithOverrides(t, map[string]string{"docling": backend.URL})
	req := httptest.NewRequest(http.MethodPost, "/v1/convert/source", strings.NewReader(`{"source":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	if gotPath != "/v1/convert/source" {
		t.Fatalf("expected backend path /v1/convert/source, got %q", gotPath)
	}
}

func TestBackendHealthRewrite(t *testing.T) {
	var gotPath string

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("backend-ok"))
	}))
	defer backend.Close()

	handler := newTestHandlerWithOverrides(t, map[string]string{"nomic": backend.URL})
	req := httptest.NewRequest(http.MethodGet, "/health/nomic", nil)
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
		nomicURL:   "http://127.0.0.1:8302",
		qwenURL:    "http://127.0.0.1:8505",
		whisperURL: "http://127.0.0.1:8203",
		voxtralURL: "http://127.0.0.1:8402",
		doclingURL: "http://127.0.0.1:8600",
	}

	for name, raw := range overrides {
		switch name {
		case "nomic":
			cfg.nomicURL = raw
		case "qwen":
			cfg.qwenURL = raw
		case "whisper":
			cfg.whisperURL = raw
		case "voxtral":
			cfg.voxtralURL = raw
		case "docling":
			cfg.doclingURL = raw
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
		nomicURL:   "://bad",
		qwenURL:    "http://127.0.0.1:8505",
		whisperURL: "http://127.0.0.1:8203",
		voxtralURL: "http://127.0.0.1:8402",
		doclingURL: "http://127.0.0.1:8600",
	})
	if err == nil {
		t.Fatal("expected invalid url error")
	}
}

func TestBuildRoutesRequiresSchemeAndHost(t *testing.T) {
	_, err := buildRoutes(config{
		listenAddr: ":8080",
		nomicURL:   "/relative",
		qwenURL:    "http://127.0.0.1:8505",
		whisperURL: "http://127.0.0.1:8203",
		voxtralURL: "http://127.0.0.1:8402",
		doclingURL: "http://127.0.0.1:8600",
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
