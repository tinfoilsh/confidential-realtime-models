package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// OpenAI Realtime compatibility layer for voxtral-mini-4b-realtime.
//
// vLLM's /v1/realtime speaks a minimal dialect: session.update carries only
// {model}, audio is committed by the client with a `final` flag, deltas arrive
// as flat `transcription.delta` events, and `transcription.done` (full session
// text) follows a final commit, after which the session is dead. OpenAI
// Realtime transcription clients instead expect a session.created/updated
// handshake, `conversation.item.input_audio_transcription.delta/.completed`
// events, per-turn commits on a long-lived session, and 24kHz PCM16 input.
//
// This layer terminates the client WebSocket when the connection looks like an
// OpenAI transcription client (`?intent=transcription`), dials the vLLM
// backend, and translates both directions:
//
//	client session.update            -> consumed (rate extracted), session.updated replied
//	client input_audio_buffer.append -> resampled to 16kHz, forwarded
//	client input_audio_buffer.commit -> backend commit {final:true}; await
//	                                    transcription.done; emit committed +
//	                                    ...input_audio_transcription.completed;
//	                                    backend re-dialed lazily for the next turn
//	backend transcription.delta      -> conversation.item.input_audio_transcription.delta
//	backend error                    -> OpenAI error event
//
// Turn detection (`server_vad`) is NOT emulated: session.update accepting a
// turn_detection config is a polite lie — the session behaves as one manual
// turn per commit. This serves push-to-talk dictation clients, which commit
// exactly once per utterance.
//
// vLLM-dialect clients (`?model=`) are unaffected; they keep the passthrough
// reverse proxy.

const (
	vllmRealtimeModel  = "voxtral-mini-4b-realtime"
	backendSampleRate  = 16000
	defaultClientRate  = 24000 // OpenAI Realtime default for audio/pcm
	backendDialTimeout = 10 * time.Second
	doneWaitTimeout    = 15 * time.Second
	writeTimeout       = 10 * time.Second
)

var compatUpgrader = websocket.Upgrader{
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  64 << 10,
	WriteBufferSize: 64 << 10,
}

// isOpenAICompatUpgrade reports whether the request is a WebSocket upgrade
// from an OpenAI Realtime transcription client. Stock OpenAI clients connect
// with `?intent=transcription` and no model in the query; vLLM-dialect
// clients always pass `?model=` and never `intent`.
func isOpenAICompatUpgrade(r *http.Request) bool {
	if r.URL.Path != "/v1/realtime" {
		return false
	}
	if r.URL.Query().Get("intent") != "transcription" {
		return false
	}
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

type rtEvent map[string]any

type compatSession struct {
	client     *websocket.Conn
	backendURL *url.URL
	authHeader string

	mu               sync.Mutex // guards client writes and all state below
	backend          *websocket.Conn
	backendGen       int // increments per backend connection; stale readers exit
	rs               *resampler
	clientRate       int
	itemSeq          int
	eventSeq         int
	bytesSinceCommit int
	turnText         string      // deltas accumulated for the current turn
	doneCh           chan string // non-nil while a commit awaits transcription.done
}

// serveOpenAICompat handles one OpenAI-dialect client connection end to end.
func serveOpenAICompat(w http.ResponseWriter, r *http.Request, backendURL *url.URL) {
	authHeader := r.Header.Get("Authorization")

	// Browser clients carry the API key in a subprotocol. Lift it into the
	// Authorization header for the backend dial and echo only safe protocols.
	offered := parseSubprotocols(r.Header.Values("Sec-WebSocket-Protocol"))
	for _, p := range offered {
		if strings.HasPrefix(p, "openai-insecure-api-key.") && authHeader == "" {
			authHeader = "Bearer " + strings.TrimPrefix(p, "openai-insecure-api-key.")
		}
	}
	var respHeader http.Header
	if chosen := pickRealtimeSubprotocol(offered); chosen != "" {
		respHeader = http.Header{"Sec-WebSocket-Protocol": []string{chosen}}
	}

	client, err := compatUpgrader.Upgrade(w, r, respHeader)
	if err != nil {
		log.Printf("openai-compat: client upgrade failed: %v", err)
		return
	}
	defer client.Close()

	s := &compatSession{
		client:     client,
		backendURL: backendURL,
		authHeader: authHeader,
		clientRate: defaultClientRate,
	}

	s.mu.Lock()
	if err := s.connectBackendLocked(); err != nil {
		s.sendErrorLocked("server_error", "backend_unavailable", fmt.Sprintf("could not reach model backend: %v", err))
		s.mu.Unlock()
		return
	}
	s.sendEventLocked(rtEvent{
		"type":    "session.created",
		"session": s.sessionObjectLocked(),
	})
	s.mu.Unlock()

	s.clientLoop()

	// Client is gone. Finalize the backend session so vLLM accounts usage and
	// frees the stream, then drop it.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backend != nil {
		_ = s.backend.SetWriteDeadline(time.Now().Add(writeTimeout))
		_ = s.backend.WriteJSON(rtEvent{"type": "input_audio_buffer.commit", "final": true})
		_ = s.backend.Close()
		s.backend = nil
	}
}

// connectBackendLocked dials vLLM and performs its handshake. Caller holds mu.
func (s *compatSession) connectBackendLocked() error {
	wsURL := *s.backendURL
	switch wsURL.Scheme {
	case "http":
		wsURL.Scheme = "ws"
	case "https":
		wsURL.Scheme = "wss"
	}
	wsURL.Path = "/v1/realtime"
	wsURL.RawQuery = url.Values{"model": []string{vllmRealtimeModel}}.Encode()

	header := http.Header{}
	if s.authHeader != "" {
		header.Set("Authorization", s.authHeader)
	}
	dialer := websocket.Dialer{HandshakeTimeout: backendDialTimeout}
	conn, _, err := dialer.Dial(wsURL.String(), header)
	if err != nil {
		return err
	}

	// vLLM handshake: session.created, then our session.update + readiness commit.
	type created struct {
		Type string `json:"type"`
	}
	var first created
	_ = conn.SetReadDeadline(time.Now().Add(backendDialTimeout))
	if err := conn.ReadJSON(&first); err != nil || first.Type != "session.created" {
		conn.Close()
		return fmt.Errorf("unexpected backend handshake (type=%q err=%v)", first.Type, err)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if err := conn.WriteJSON(rtEvent{"type": "session.update", "model": vllmRealtimeModel}); err != nil {
		conn.Close()
		return err
	}
	if err := conn.WriteJSON(rtEvent{"type": "input_audio_buffer.commit"}); err != nil {
		conn.Close()
		return err
	}

	s.backend = conn
	s.backendGen++
	s.rs = newResampler(s.clientRate, backendSampleRate)
	go s.backendLoop(conn, s.backendGen)
	return nil
}

// backendLoop translates backend events to the client until the backend
// connection dies or is superseded.
func (s *compatSession) backendLoop(conn *websocket.Conn, gen int) {
	for {
		var msg rtEvent
		if err := conn.ReadJSON(&msg); err != nil {
			s.mu.Lock()
			// Only report unexpected deaths of the live connection.
			if s.backendGen == gen && s.backend != nil {
				s.backend = nil
				if s.doneCh != nil {
					close(s.doneCh)
					s.doneCh = nil
				} else {
					s.sendErrorLocked("server_error", "backend_closed", "model backend connection closed unexpectedly")
				}
			}
			s.mu.Unlock()
			return
		}

		t, _ := msg["type"].(string)
		switch t {
		case "transcription.delta":
			delta, _ := msg["delta"].(string)
			if delta == "" {
				continue
			}
			s.mu.Lock()
			if s.backendGen == gen {
				s.turnText += delta
				s.sendEventLocked(rtEvent{
					"type":    "conversation.item.input_audio_transcription.delta",
					"item_id": s.currentItemIDLocked(),
					"delta":   delta,
				})
			}
			s.mu.Unlock()

		case "transcription.done":
			text, _ := msg["text"].(string)
			s.mu.Lock()
			if s.backendGen == gen && s.doneCh != nil {
				s.doneCh <- text
				s.doneCh = nil
			}
			s.mu.Unlock()

		case "error":
			s.mu.Lock()
			if s.backendGen == gen {
				message := "model backend error"
				if e, ok := msg["error"].(map[string]any); ok {
					if m, ok := e["message"].(string); ok {
						message = m
					}
				} else if m, ok := msg["error"].(string); ok {
					message = m
				}
				s.sendErrorLocked("server_error", "backend_error", message)
			}
			s.mu.Unlock()
		}
	}
}

// clientLoop processes OpenAI-dialect client events until the client hangs up.
func (s *compatSession) clientLoop() {
	for {
		var msg rtEvent
		if err := s.client.ReadJSON(&msg); err != nil {
			return
		}

		t, _ := msg["type"].(string)
		switch t {
		case "session.update":
			s.handleSessionUpdate(msg)

		case "input_audio_buffer.append":
			s.handleAppend(msg)

		case "input_audio_buffer.commit":
			s.handleCommit()

		case "input_audio_buffer.clear":
			s.mu.Lock()
			// vLLM has no clear; drop the turn by recycling the backend.
			if s.backend != nil {
				_ = s.backend.Close()
				s.backend = nil
			}
			s.turnText = ""
			s.bytesSinceCommit = 0
			s.sendEventLocked(rtEvent{"type": "input_audio_buffer.cleared"})
			s.mu.Unlock()

		default:
			// Ignore unknown client events (response.create etc. have no
			// meaning for transcription-only sessions).
		}
	}
}

func (s *compatSession) handleSessionUpdate(msg rtEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rate, format, found := parseDeclaredInputFormat(msg)
	if found {
		if format != "" && format != "audio/pcm" && format != "pcm16" {
			s.sendErrorLocked("invalid_request_error", "unsupported_audio_format",
				fmt.Sprintf("unsupported input audio format %q: only PCM16 is supported", format))
			return
		}
		if rate != s.clientRate {
			s.clientRate = rate
			s.rs = newResampler(s.clientRate, backendSampleRate)
		}
	}

	s.sendEventLocked(rtEvent{
		"type":    "session.updated",
		"session": s.sessionObjectLocked(),
	})
}

func (s *compatSession) handleAppend(msg rtEvent) {
	audioB64, _ := msg["audio"].(string)
	if audioB64 == "" {
		return
	}
	pcm, err := base64.StdEncoding.DecodeString(audioB64)
	if err != nil {
		s.mu.Lock()
		s.sendErrorLocked("invalid_request_error", "invalid_audio", "audio is not valid base64")
		s.mu.Unlock()
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.backend == nil {
		if err := s.connectBackendLocked(); err != nil {
			s.sendErrorLocked("server_error", "backend_unavailable", fmt.Sprintf("could not reach model backend: %v", err))
			return
		}
	}

	if s.clientRate != backendSampleRate {
		pcm = s.rs.process(pcm)
		if len(pcm) == 0 {
			s.bytesSinceCommit++ // count the attempt; tiny chunk carried over
			return
		}
	}
	s.bytesSinceCommit += len(pcm)

	_ = s.backend.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.backend.WriteJSON(rtEvent{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm),
	}); err != nil {
		s.sendErrorLocked("server_error", "backend_write_failed", "failed to forward audio to model backend")
	}
}

func (s *compatSession) handleCommit() {
	s.mu.Lock()

	if s.bytesSinceCommit == 0 || s.backend == nil {
		s.sendErrorLocked("invalid_request_error", "input_audio_buffer_commit_empty",
			"buffer too small: no audio to commit")
		s.mu.Unlock()
		return
	}

	doneCh := make(chan string, 1)
	s.doneCh = doneCh
	_ = s.backend.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.backend.WriteJSON(rtEvent{"type": "input_audio_buffer.commit", "final": true}); err != nil {
		s.doneCh = nil
		s.sendErrorLocked("server_error", "backend_write_failed", "failed to commit audio to model backend")
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	// Await the authoritative final text; fall back to accumulated deltas.
	var text string
	var ok bool
	select {
	case text, ok = <-doneCh:
	case <-time.After(doneWaitTimeout):
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.doneCh = nil
	if !ok || text == "" {
		text = s.turnText
	}
	itemID := s.currentItemIDLocked()

	s.sendEventLocked(rtEvent{
		"type":    "input_audio_buffer.committed",
		"item_id": itemID,
	})
	s.sendEventLocked(rtEvent{
		"type":       "conversation.item.input_audio_transcription.completed",
		"item_id":    itemID,
		"transcript": strings.TrimSpace(text),
	})

	// The vLLM session is dead after a final commit; recycle for the next turn.
	if s.backend != nil {
		_ = s.backend.Close()
		s.backend = nil
	}
	s.itemSeq++
	s.turnText = ""
	s.bytesSinceCommit = 0
}

// --- helpers (callers hold mu) ---

func (s *compatSession) currentItemIDLocked() string {
	return fmt.Sprintf("item_%03d", s.itemSeq)
}

func (s *compatSession) sessionObjectLocked() rtEvent {
	return rtEvent{
		"type": "transcription",
		"audio": rtEvent{
			"input": rtEvent{
				"format": rtEvent{"type": "audio/pcm", "rate": s.clientRate},
				"transcription": rtEvent{
					"model": vllmRealtimeModel,
				},
			},
		},
	}
}

func (s *compatSession) sendEventLocked(ev rtEvent) {
	s.eventSeq++
	ev["event_id"] = fmt.Sprintf("event_%04d", s.eventSeq)
	_ = s.client.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.client.WriteJSON(ev); err != nil {
		log.Printf("openai-compat: client write failed: %v", err)
	}
}

func (s *compatSession) sendErrorLocked(errType, code, message string) {
	s.sendEventLocked(rtEvent{
		"type": "error",
		"error": rtEvent{
			"type":    errType,
			"code":    code,
			"message": message,
		},
	})
}

// parseDeclaredInputFormat extracts the input audio format from the two
// session.update shapes in the wild. Returns (rate, formatName, found).
//
//	GA:     session.audio.input.format: {type: "audio/pcm", rate: 24000}
//	Legacy: session.input_audio_format: "pcm16" (implies 24kHz)
func parseDeclaredInputFormat(msg rtEvent) (int, string, bool) {
	session, ok := msg["session"].(map[string]any)
	if !ok {
		return 0, "", false
	}

	if audio, ok := session["audio"].(map[string]any); ok {
		if input, ok := audio["input"].(map[string]any); ok {
			if format, ok := input["format"].(map[string]any); ok {
				name, _ := format["type"].(string)
				rate := defaultClientRate
				if r, ok := format["rate"].(float64); ok && r > 0 {
					rate = int(r)
				}
				return rate, name, true
			}
		}
	}

	if name, ok := session["input_audio_format"].(string); ok {
		return defaultClientRate, name, true
	}

	return 0, "", false
}
