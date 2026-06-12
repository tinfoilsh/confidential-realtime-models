package main

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// --- fake vLLM backend ---
//
// Speaks the dialect observed against production voxtral-mini-4b-realtime:
// session.created on connect; appends accumulate silently; a non-final commit
// returns nothing; a final commit emits a few transcription.delta events then
// transcription.done with the full session text; after done the session
// ignores all further input.

type fakeVLLM struct {
	srv         *httptest.Server
	mu          sync.Mutex
	connections int32
	audioBytes  []int // per-connection audio byte counts
	text        string
}

func newFakeVLLM(t *testing.T, text string) *fakeVLLM {
	t.Helper()
	f := &fakeVLLM{text: text}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime" || r.URL.Query().Get("model") != vllmRealtimeModel {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()

		atomic.AddInt32(&f.connections, 1)
		connBytes := 0
		done := false

		_ = conn.WriteJSON(map[string]any{"type": "session.created", "id": "sess-fake"})

		for {
			var msg map[string]any
			if err := conn.ReadJSON(&msg); err != nil {
				f.recordBytes(connBytes)
				return
			}
			if done {
				continue // post-done sessions ignore everything
			}
			switch msg["type"] {
			case "session.update":
				// ignored, like vLLM
			case "input_audio_buffer.append":
				audio, _ := msg["audio"].(string)
				raw, _ := base64.StdEncoding.DecodeString(audio)
				connBytes += len(raw)
				// Stream a delta per ~16KB of audio, mimicking continuous decoding.
				if connBytes/16384 > (connBytes-len(raw))/16384 {
					_ = conn.WriteJSON(map[string]any{"type": "transcription.delta", "delta": " chunk"})
				}
			case "input_audio_buffer.commit":
				if final, _ := msg["final"].(bool); final {
					_ = conn.WriteJSON(map[string]any{"type": "transcription.delta", "delta": " tail"})
					_ = conn.WriteJSON(map[string]any{"type": "transcription.done", "text": f.text})
					done = true
				}
				// non-final commit: silence, like production
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeVLLM) recordBytes(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.audioBytes = append(f.audioBytes, n)
}

// dialCompat connects an OpenAI-style client through the router to the fake backend.
func dialCompat(t *testing.T, backendURL string, query string) *websocket.Conn {
	t.Helper()

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("X-Forwarded-Host", "voxtral-mini-4b-realtime."+testDomain)
		newTestHandlerWith(t, "voxtral_mini", backendURL).ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	wsURL := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/realtime?" + query
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var msg map[string]any
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read event: %v", err)
	}
	return msg
}

// readUntil reads events until one matches the wanted type, failing the test
// on timeout. Returns the matching event and all delta payloads seen on the way.
func readUntil(t *testing.T, conn *websocket.Conn, wantType string) (map[string]any, []string) {
	t.Helper()
	var deltas []string
	for range 50 {
		msg := readEvent(t, conn)
		mt, _ := msg["type"].(string)
		if mt == "conversation.item.input_audio_transcription.delta" {
			d, _ := msg["delta"].(string)
			deltas = append(deltas, d)
		}
		if mt == wantType {
			return msg, deltas
		}
	}
	t.Fatalf("never received %q", wantType)
	return nil, nil
}

func pcm16Sine(samples int) []byte {
	out := make([]byte, samples*2)
	for i := range samples {
		v := int16(3000 * math.Sin(2*math.Pi*440*float64(i)/24000))
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}

func sendAppend(t *testing.T, conn *websocket.Conn, pcm []byte) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// --- tests ---

func TestOpenAICompatFullTurn(t *testing.T) {
	backend := newFakeVLLM(t, " hello world.")
	conn := dialCompat(t, backend.srv.URL, "intent=transcription")

	if msg := readEvent(t, conn); msg["type"] != "session.created" {
		t.Fatalf("expected session.created first, got %v", msg["type"])
	}

	// OpenAI GA-shaped session.update declaring 24kHz.
	if err := conn.WriteJSON(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
			"audio": map[string]any{
				"input": map[string]any{
					"format":         map[string]any{"type": "audio/pcm", "rate": 24000},
					"transcription":  map[string]any{"model": "gpt-4o-mini-transcribe"},
					"turn_detection": map[string]any{"type": "server_vad"},
				},
			},
		},
	}); err != nil {
		t.Fatalf("session.update: %v", err)
	}
	// server_vad in the config is accepted without effect and never echoed.
	updated := readEvent(t, conn)
	if updated["type"] != "session.updated" {
		t.Fatalf("expected session.updated, got %v", updated["type"])
	}
	if strings.Contains(fmt.Sprintf("%v", updated["session"]), "turn_detection") {
		t.Fatal("session.updated must not echo a turn_detection we don't honor")
	}

	// One second of 24kHz audio, in chunks.
	pcm := pcm16Sine(24000)
	for i := 0; i < len(pcm); i += 4096 {
		sendAppend(t, conn, pcm[i:min(i+4096, len(pcm))])
	}

	if err := conn.WriteJSON(map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	committed, deltas := readUntil(t, conn, "input_audio_buffer.committed")
	if committed["item_id"] == "" {
		t.Fatal("committed missing item_id")
	}
	completed, _ := readUntil(t, conn, "conversation.item.input_audio_transcription.completed")
	if got := completed["transcript"]; got != "hello world." {
		t.Fatalf("expected transcript %q, got %q", "hello world.", got)
	}
	if len(deltas) == 0 {
		t.Fatal("expected delta events before committed")
	}

	// 24k -> 16k: backend must have received 2/3 of the bytes (one connection
	// so far; it records on close, so close the client first).
	conn.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		backend.mu.Lock()
		n := len(backend.audioBytes)
		backend.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.audioBytes) == 0 {
		t.Fatal("backend never recorded a connection")
	}
	got := backend.audioBytes[0]
	want := len(pcm) * 2 / 3
	if got < want-64 || got > want+64 {
		t.Fatalf("expected ~%d resampled bytes at backend, got %d", want, got)
	}
}

func TestOpenAICompatEmptyCommit(t *testing.T) {
	backend := newFakeVLLM(t, "")
	conn := dialCompat(t, backend.srv.URL, "intent=transcription")
	readEvent(t, conn) // session.created

	if err := conn.WriteJSON(map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	msg := readEvent(t, conn)
	if msg["type"] != "error" {
		t.Fatalf("expected error event, got %v", msg["type"])
	}
	errObj, _ := msg["error"].(map[string]any)
	if errObj["code"] != "input_audio_buffer_commit_empty" {
		t.Fatalf("expected input_audio_buffer_commit_empty, got %v", errObj["code"])
	}
}

func TestOpenAICompatMultiTurnRedialsBackend(t *testing.T) {
	backend := newFakeVLLM(t, " turn text.")
	conn := dialCompat(t, backend.srv.URL, "intent=transcription")
	readEvent(t, conn) // session.created

	for turn := range 2 {
		sendAppend(t, conn, pcm16Sine(24000))
		if err := conn.WriteJSON(map[string]any{"type": "input_audio_buffer.commit"}); err != nil {
			t.Fatalf("turn %d commit: %v", turn, err)
		}
		completed, _ := readUntil(t, conn, "conversation.item.input_audio_transcription.completed")
		if completed["transcript"] != "turn text." {
			t.Fatalf("turn %d: unexpected transcript %v", turn, completed["transcript"])
		}
	}

	if n := atomic.LoadInt32(&backend.connections); n != 2 {
		t.Fatalf("expected 2 backend connections (one per turn), got %d", n)
	}
}

func TestOpenAICompat16kDeclaredPassesThrough(t *testing.T) {
	backend := newFakeVLLM(t, " ok.")
	conn := dialCompat(t, backend.srv.URL, "intent=transcription")
	readEvent(t, conn) // session.created

	if err := conn.WriteJSON(map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"audio": map[string]any{
				"input": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 16000},
				},
			},
		},
	}); err != nil {
		t.Fatalf("session.update: %v", err)
	}
	readEvent(t, conn) // session.updated

	pcm := pcm16Sine(16000)
	sendAppend(t, conn, pcm)
	_ = conn.WriteJSON(map[string]any{"type": "input_audio_buffer.commit"})
	readUntil(t, conn, "conversation.item.input_audio_transcription.completed")
	conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for {
		backend.mu.Lock()
		n := len(backend.audioBytes)
		backend.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.audioBytes) == 0 || backend.audioBytes[0] != len(pcm) {
		t.Fatalf("expected passthrough of %d bytes, got %v", len(pcm), backend.audioBytes)
	}
}

func TestOpenAICompatUnsupportedFormatRejected(t *testing.T) {
	backend := newFakeVLLM(t, "")
	conn := dialCompat(t, backend.srv.URL, "intent=transcription")
	readEvent(t, conn) // session.created

	if err := conn.WriteJSON(map[string]any{
		"type":    "session.update",
		"session": map[string]any{"input_audio_format": "g711_ulaw"},
	}); err != nil {
		t.Fatalf("session.update: %v", err)
	}
	msg := readEvent(t, conn)
	errObj, _ := msg["error"].(map[string]any)
	if msg["type"] != "error" || errObj["code"] != "unsupported_audio_format" {
		t.Fatalf("expected unsupported_audio_format error, got %v", msg)
	}
}

func TestVLLMDialectStillPassesThrough(t *testing.T) {
	// ?model= (no intent): must hit the reverse proxy untouched, not the shim.
	var sawBackend atomic.Bool
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("model") != vllmRealtimeModel {
			t.Errorf("backend expected raw query passthrough, got %q", r.URL.RawQuery)
		}
		sawBackend.Store(true)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "session.created"})
		_, _, _ = conn.ReadMessage()
	}))
	defer backend.Close()

	conn := dialCompat(t, backend.URL, "model="+vllmRealtimeModel)
	msg := readEvent(t, conn)
	if msg["type"] != "session.created" {
		t.Fatalf("expected vLLM session.created passthrough, got %v", msg)
	}
	if !sawBackend.Load() {
		t.Fatal("backend never saw the passthrough connection")
	}
}

// --- resampler unit tests ---

func TestResamplerRatioAndChunking(t *testing.T) {
	// 24k -> 16k over a ramp; chunked and unchunked must agree exactly.
	src := make([]byte, 24000*2)
	for i := range 24000 {
		binary.LittleEndian.PutUint16(src[2*i:], uint16(int16(i%2000)))
	}

	whole := newResampler(24000, 16000).process(src)

	chunked := newResampler(24000, 16000)
	var out []byte
	for i := 0; i < len(src); i += 1234 { // odd size: exercises byte-split carry
		out = append(out, chunked.process(src[i:min(i+1234, len(src))])...)
	}

	if len(whole) != len(out) {
		t.Fatalf("chunked length %d != whole length %d", len(out), len(whole))
	}
	for i := range whole {
		if whole[i] != out[i] {
			t.Fatalf("chunked output diverges at byte %d", i)
		}
	}

	want := len(src) * 2 / 3
	if len(whole) < want-8 || len(whole) > want+8 {
		t.Fatalf("expected ~%d bytes out, got %d", want, len(whole))
	}
}

func TestResamplerPreservesDC(t *testing.T) {
	// A constant signal must stay constant through interpolation.
	src := make([]byte, 4800*2)
	for i := range 4800 {
		binary.LittleEndian.PutUint16(src[2*i:], uint16(int16(1000)))
	}
	out := newResampler(24000, 16000).process(src)
	if len(out) == 0 {
		t.Fatal("no output")
	}
	for i := 0; i+1 < len(out); i += 2 {
		if v := int16(binary.LittleEndian.Uint16(out[i:])); v != 1000 {
			t.Fatalf("DC level not preserved at sample %d: %d", i/2, v)
		}
	}
}
