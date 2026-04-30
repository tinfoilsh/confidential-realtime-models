// Package main implements the Tinfoil realtime-models reverse proxy.
//
// Three audio backends share the same enclave: qwen3-tts, voxtral-tts, and
// voxtral-mini-4b-realtime. They all expose the OpenAI-standard
// /v1/audio/speech and/or /v1/realtime paths, so we can't dispatch by path.
// We route on the leftmost subdomain of the request's Host (taken from
// X-Forwarded-Host when the shim terminates TLS):
//
//   qwen3-tts.realtime.tinfoil.sh                 -> qwen3-tts container
//   voxtral-tts.realtime.tinfoil.sh               -> voxtral-tts container
//   voxtral-mini-4b-realtime.realtime.tinfoil.sh  -> voxtral-mini-4b-realtime
//   realtime.tinfoil.sh                           -> /health on the router itself
//
// The shim's listen-port still terminates TLS for *.realtime.tinfoil.sh
// (wildcard cert required). All requests reach this router on plain HTTP via
// the loopback `upstream-port`.
package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

type backend struct {
	name     string
	upstream *url.URL
	proxy    *httputil.ReverseProxy
}

type config struct {
	listenAddr             string
	domain                 string
	qwenTTSURL             string
	voxtralTTSURL          string
	voxtralMiniRealtimeURL string
}

func main() {
	cfg := config{
		listenAddr:             getenvDefault("LISTEN_ADDR", ":8080"),
		domain:                 getenvDefault("DOMAIN", "localhost"),
		qwenTTSURL:             getenvDefault("QWEN_TTS_URL", "http://127.0.0.1:8505"),
		voxtralTTSURL:          getenvDefault("VOXTRAL_TTS_URL", "http://127.0.0.1:8605"),
		voxtralMiniRealtimeURL: getenvDefault("VOXTRAL_MINI_REALTIME_URL", "http://127.0.0.1:8402"),
	}

	handler, err := newHandler(cfg)
	if err != nil {
		log.Fatalf("failed to build router: %v", err)
	}

	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 15 * time.Second,
	}

	log.Printf("starting realtime router on %s (domain=%s)", cfg.listenAddr, cfg.domain)
	log.Printf("backend qwen3-tts                -> %s", cfg.qwenTTSURL)
	log.Printf("backend voxtral-tts              -> %s", cfg.voxtralTTSURL)
	log.Printf("backend voxtral-mini-4b-realtime -> %s", cfg.voxtralMiniRealtimeURL)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}

func newHandler(cfg config) (http.Handler, error) {
	if cfg.domain == "" {
		return nil, errors.New("DOMAIN must not be empty")
	}

	backends, err := buildBackends(cfg)
	if err != nil {
		return nil, err
	}

	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelName := parseModelFromSubdomain(r, cfg.domain)

		if modelName == "" {
			// Root domain or other host: only /health is exposed.
			if r.URL.Path == "/health" {
				healthHandler(w, r)
				return
			}
			http.NotFound(w, r)
			return
		}

		b, ok := backends[modelName]
		if !ok {
			log.Printf("unknown model subdomain %q (host=%q)", modelName, r.Header.Get("X-Forwarded-Host"))
			http.Error(w, fmt.Sprintf("unknown model: %s", modelName), http.StatusNotFound)
			return
		}

		b.proxy.ServeHTTP(w, r)
	}), nil
}

func buildBackends(cfg config) (map[string]*backend, error) {
	specs := []struct {
		name string
		raw  string
	}{
		{name: "qwen3-tts", raw: cfg.qwenTTSURL},
		{name: "voxtral-tts", raw: cfg.voxtralTTSURL},
		{name: "voxtral-mini-4b-realtime", raw: cfg.voxtralMiniRealtimeURL},
	}

	backends := make(map[string]*backend, len(specs))
	for _, spec := range specs {
		target, err := url.Parse(spec.raw)
		if err != nil {
			return nil, fmt.Errorf("parse %s url: %w", spec.name, err)
		}
		if target.Scheme == "" || target.Host == "" {
			return nil, fmt.Errorf("%s url must include scheme and host: %q", spec.name, spec.raw)
		}

		proxy := httputil.NewSingleHostReverseProxy(target)
		director := proxy.Director
		proxy.Director = func(req *http.Request) {
			director(req)
			req.Host = target.Host
		}
		proxy.ModifyResponse = fixRealtimeSubprotocolEcho
		proxy.ErrorLog = log.New(os.Stderr, fmt.Sprintf("%s proxy: ", spec.name), log.LstdFlags)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy error for %s %s via %s: %v", r.Method, r.URL.Path, spec.name, err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}

		backends[spec.name] = &backend{
			name:     spec.name,
			upstream: target,
			proxy:    proxy,
		}
	}

	return backends, nil
}

// parseModelFromSubdomain returns the leftmost label of the X-Forwarded-Host
// header when the host is a strict subdomain of `domain`. Returns "" when:
//   - the header is missing (we trust only what the shim hands us; there's
//     no fallback to r.Host since this router is meant to sit behind the shim)
//   - the host equals `domain` itself (apex)
//   - the host doesn't end in `.domain`
//   - the leftmost label is empty
func parseModelFromSubdomain(r *http.Request, domain string) string {
	host := r.Header.Get("X-Forwarded-Host")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSpace(host))

	if host == "" || host == domain {
		return ""
	}
	if !strings.HasSuffix(host, "."+domain) {
		return ""
	}

	sub := strings.TrimSuffix(host, "."+domain)
	if sub == "" {
		return ""
	}
	parts := strings.Split(sub, ".")
	return parts[0]
}

func getenvDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// Subprotocols that carry credentials and must never be echoed back to the
// client. RFC 6455 echoes go in plaintext response headers, so reflecting
// `openai-insecure-api-key.<KEY>` would leak the key to any TLS-terminating
// intermediary.
var sensitiveSubprotocolPrefixes = []string{
	"openai-insecure-api-key.",
	"openai-organization.",
	"openai-project.",
}

// fixRealtimeSubprotocolEcho rewrites the Sec-WebSocket-Protocol header on
// 101 Switching Protocols responses so vLLM's native /v1/realtime handler
// becomes Chrome- and Firefox-compatible.
func fixRealtimeSubprotocolEcho(resp *http.Response) error {
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Request == nil {
		return nil
	}
	offered := parseSubprotocols(resp.Request.Header.Values("Sec-WebSocket-Protocol"))
	if chosen := pickRealtimeSubprotocol(offered); chosen != "" {
		resp.Header.Set("Sec-WebSocket-Protocol", chosen)
	} else {
		resp.Header.Del("Sec-WebSocket-Protocol")
	}
	return nil
}

func parseSubprotocols(headerValues []string) []string {
	var out []string
	for _, v := range headerValues {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// pickRealtimeSubprotocol picks a single subprotocol to echo back, preferring
// "realtime" and never reflecting credential-bearing tokens. Returns "" when
// nothing is safe to echo (in which case Sec-WebSocket-Protocol is dropped
// from the response, matching the offer-nothing case in RFC 6455).
func pickRealtimeSubprotocol(offered []string) string {
	var safe []string
	for _, p := range offered {
		sensitive := false
		for _, prefix := range sensitiveSubprotocolPrefixes {
			if strings.HasPrefix(p, prefix) {
				sensitive = true
				break
			}
		}
		if !sensitive {
			safe = append(safe, p)
		}
	}
	for _, p := range safe {
		if p == "realtime" {
			return "realtime"
		}
	}
	if len(safe) > 0 {
		return safe[0]
	}
	return ""
}
