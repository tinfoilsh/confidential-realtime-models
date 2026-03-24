package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

type route struct {
	name    string
	pattern string
	exact   bool
	target  *url.URL
	proxy   *httputil.ReverseProxy
}

type config struct {
	listenAddr string
	qwenURL    string
	voxtralURL string
}

func main() {
	cfg := config{
		listenAddr: getenvDefault("LISTEN_ADDR", ":8080"),
		qwenURL:    getenvDefault("QWEN_URL", "http://127.0.0.1:8505"),
		voxtralURL: getenvDefault("VOXTRAL_URL", "http://127.0.0.1:8402"),
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

	log.Printf("starting realtime router on %s", cfg.listenAddr)
	log.Printf("routing /v1/audio/speech -> %s", cfg.qwenURL)
	log.Printf("routing /v1/realtime -> %s", cfg.voxtralURL)

	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server failed: %v", err)
	}
}

func newHandler(cfg config) (http.Handler, error) {
	routes, err := buildRoutes(cfg)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/health/router", healthHandler)

	for _, rt := range routes {
		route := rt
		mux.Handle(route.pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route.proxy.ServeHTTP(w, r)
		}))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, rt := range routes {
			if rt.matches(r.URL.Path) {
				mux.ServeHTTP(w, r)
				return
			}
		}
		if r.URL.Path == "/health" || r.URL.Path == "/health/router" {
			mux.ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}), nil
}

func buildRoutes(cfg config) ([]route, error) {
	specs := []struct {
		name        string
		pattern     string
		exact       bool
		rawURL      string
		targetPath  string
	}{
		{name: "qwen", pattern: "/v1/audio/speech", rawURL: cfg.qwenURL},
		{name: "voxtral", pattern: "/v1/realtime", rawURL: cfg.voxtralURL},
		{name: "qwen-health", pattern: "/health/qwen", exact: true, rawURL: cfg.qwenURL, targetPath: "/health"},
		{name: "voxtral-health", pattern: "/health/voxtral", exact: true, rawURL: cfg.voxtralURL, targetPath: "/health"},
		{name: "qwen-metrics", pattern: "/metrics/qwen", exact: true, rawURL: cfg.qwenURL, targetPath: "/metrics"},
		{name: "voxtral-metrics", pattern: "/metrics/voxtral", exact: true, rawURL: cfg.voxtralURL, targetPath: "/metrics"},
	}

	routes := make([]route, 0, len(specs))
	for _, spec := range specs {
		target, err := url.Parse(spec.rawURL)
		if err != nil {
			return nil, fmt.Errorf("parse %s url: %w", spec.name, err)
		}
		if target.Scheme == "" || target.Host == "" {
			return nil, fmt.Errorf("%s url must include scheme and host: %q", spec.name, spec.rawURL)
		}

		proxy := newSingleHostReverseProxy(target, spec.pattern, spec.exact, spec.targetPath)
		proxy.ErrorLog = log.New(os.Stderr, fmt.Sprintf("%s proxy: ", spec.name), log.LstdFlags)
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy error for %s %s via %s: %v", r.Method, r.URL.Path, spec.name, err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}

		routes = append(routes, route{
			name:    spec.name,
			pattern: spec.pattern,
			exact:   spec.exact,
			target:  target,
			proxy:   proxy,
		})
	}

	return routes, nil
}

func (r route) matches(requestPath string) bool {
	if r.exact {
		return requestPath == r.pattern
	}
	return strings.HasPrefix(requestPath, r.pattern)
}

func newSingleHostReverseProxy(target *url.URL, pattern string, exact bool, targetPath string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director

	proxy.Director = func(req *http.Request) {
		director(req)

		switch {
		case targetPath != "":
			req.URL.Path = targetPath
			req.URL.RawPath = targetPath
		case exact:
			req.URL.Path = pattern
			req.URL.RawPath = pattern
		default:
			req.URL.Path = singleJoiningSlash(target.Path, req.URL.Path)
			req.URL.RawPath = req.URL.Path
		}

		req.Host = target.Host
	}

	return proxy
}

func singleJoiningSlash(basePath, requestPath string) string {
	if basePath == "" || basePath == "/" {
		return requestPath
	}
	return path.Join(basePath, requestPath)
}

func getenvDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
