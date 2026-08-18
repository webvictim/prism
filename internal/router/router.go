// Package router provides a local HTTP server that dispatches requests
// to backend tunnels based on URL path, with Bedrock-compatibility
// scrubbing on the Anthropic path.
package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gravitational/prism/internal/usage"
)

// OpenAIAppName is the Teleport app name for the cluster-wide OpenAI gateway.
const OpenAIAppName = "openai"

// AnthropicAppName is the Teleport app name for the cluster-wide Anthropic gateway.
const AnthropicAppName = "anthropic"

// Config bundles the router's startup parameters.
type Config struct {
	ListenPort     int          // The externally-visible port (e.g. 7331).
	AnthropicPort  int          // Internal port where the anthropic tunnel binds.
	OpenAIPort     int          // Internal port where the openai tunnel binds.
	Logger         *log.Logger  // Required.
	Debug          bool
	HealthHandler  http.Handler // If set, mounted at /_prism/health.
	ConnectHandler http.Handler // If set, handles HTTP CONNECT (forward proxy).
	UsageWriter    *usage.Writer
	Proxy          string // Teleport proxy address for usage tracking.
}

// Service is the running local HTTP router.
type Service struct {
	cfg    Config
	server *http.Server
}

// New creates a Service. Does not start listening.
func New(cfg Config) (*Service, error) {
	if cfg.ListenPort <= 0 {
		return nil, fmt.Errorf("router.New: ListenPort required")
	}
	if cfg.AnthropicPort <= 0 {
		return nil, fmt.Errorf("router.New: AnthropicPort required")
	}
	if cfg.OpenAIPort <= 0 {
		return nil, fmt.Errorf("router.New: OpenAIPort required")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}

	anthropicProxy := newProxy(cfg.AnthropicPort, cfg.Logger, "anthropic")
	openaiProxy := newProxy(cfg.OpenAIPort, cfg.Logger, "openai")

	// Wrap the anthropic proxy with Bedrock scrubbing middleware.
	var anthropicHandler http.Handler = anthropicProxy
	anthropicHandler = scrubMiddleware(anthropicHandler, cfg.Logger, cfg.Debug)

	// Wrap the openai proxy with parameter normalization.
	var openaiHandler http.Handler = openaiProxy
	openaiHandler = openaiScrubMiddleware(openaiHandler, cfg.Logger, cfg.Debug)

	mux := http.NewServeMux()
	if cfg.HealthHandler != nil {
		mux.Handle("/_prism/health", cfg.HealthHandler)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isOpenAIPath(r.URL.Path) {
			openaiHandler.ServeHTTP(w, r)
		} else {
			anthropicHandler.ServeHTTP(w, r)
		}
	})

	// Wrap with request logging for /v1 paths.
	var handler http.Handler = logRequests(mux, cfg.Logger)

	// Wrap with usage capture (extracts token counts from responses).
	handler = captureUsage(handler, cfg.UsageWriter, cfg.Proxy, cfg.Logger)

	// Wrap with CONNECT dispatch for forward-proxy mode.
	if cfg.ConnectHandler != nil {
		next := handler
		connectHandler := cfg.ConnectHandler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				connectHandler.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	s := &Service{
		cfg: cfg,
		server: &http.Server{
			Addr:              fmt.Sprintf("127.0.0.1:%d", cfg.ListenPort),
			Handler:           handler,
			ReadHeaderTimeout: 30 * time.Second,
		},
	}
	return s, nil
}

// Serve starts the HTTP listener and blocks until ctx is cancelled.
func (s *Service) Serve(ctx context.Context) error {
	errCh := make(chan error, 1)
	go func() {
		s.cfg.Logger.Printf("router: listening on http://127.0.0.1:%d", s.cfg.ListenPort)
		err := s.server.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return s.server.Shutdown(shutdownCtx)
}

// isOpenAIPath returns true for paths that should be routed to the
// cluster's OpenAI-compatible gateway.
func isOpenAIPath(path string) bool {
	switch {
	case strings.HasPrefix(path, "/v1/chat/"):
		return true
	case path == "/v1/responses" || strings.HasPrefix(path, "/v1/responses/"):
		return true
	case path == "/v1/models" || strings.HasPrefix(path, "/v1/models/"):
		return true
	case path == "/v1/embeddings" || strings.HasPrefix(path, "/v1/embeddings/"):
		return true
	case path == "/v1/completions" || strings.HasPrefix(path, "/v1/completions/"):
		return true
	default:
		return false
	}
}

func newProxy(port int, logger *log.Logger, name string) *httputil.ReverseProxy {
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", port))
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		FlushInterval: -1,
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Printf("router: %s upstream error: %s %s: %v", name, r.Method, r.URL.Path, err)
		http.Error(w, fmt.Sprintf("prism: %s gateway unavailable: %v", name, err), http.StatusBadGateway)
	}
	return rp
}

// --- OpenAI scrubbing middleware ---

// openaiScrubMiddleware renames max_tokens to max_completion_tokens for
// OpenAI chat completion requests. Newer models (gpt-5.5+) reject the
// legacy parameter name; older models accept both.
func openaiScrubMiddleware(next http.Handler, logger *log.Logger, debug bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip client-supplied auth headers — the tunnel provides auth via mTLS.
		r.Header.Del("Authorization")

		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/chat/completions") {
			next.ServeHTTP(w, r)
			return
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "prism: read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
			return
		}

		changed := false

		if mt, ok := obj["max_tokens"]; ok {
			if _, hasNew := obj["max_completion_tokens"]; !hasNew {
				obj["max_completion_tokens"] = mt
				delete(obj, "max_tokens")
				changed = true
				if debug {
					logger.Printf("openai-scrub: renamed max_tokens → max_completion_tokens")
				}
			}
		}

		if openaiModelRequiresDefaultTemp(obj) {
			if temp, ok := obj["temperature"]; ok {
				if f, fok := numberAsFloat(temp); fok && f != 1.0 {
					delete(obj, "temperature")
					changed = true
					if debug {
						logger.Printf("openai-scrub: stripped temperature=%v for model %v", temp, obj["model"])
					}
				}
			}
		}

		if changed {
			rewritten, err := json.Marshal(obj)
			if err == nil {
				body = rewritten
			}
		}

		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", fmt.Sprint(len(body)))
		next.ServeHTTP(w, r)
	})
}

// openaiModelRequiresDefaultTemp returns true for models that reject
// non-default temperature values (reasoning models like o1, o3, gpt-5.5).
func openaiModelRequiresDefaultTemp(obj map[string]any) bool {
	model, _ := obj["model"].(string)
	if model == "" {
		return false
	}
	for _, prefix := range openaiFixedTempPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}

var openaiFixedTempPrefixes = []string{
	"o1",
	"o3",
	"o4",
	"gpt-5.5",
}

// --- Bedrock scrubbing middleware ---

const nonStreamMaxTokensCap = 8192

var stripFields = []string{
	"metadata",
	"context_management",
	"thinking",
}

func scrubMiddleware(next http.Handler, logger *log.Logger, debug bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Strip client-supplied auth headers — the tunnel provides auth
		// via mTLS. Without this, the gateway rejects dummy tokens.
		r.Header.Del("Authorization")
		r.Header.Del("X-Api-Key")

		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") {
			next.ServeHTTP(w, r)
			return
		}
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "application/json") {
			next.ServeHTTP(w, r)
			return
		}

		body, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil {
			http.Error(w, "prism: read body: "+err.Error(), http.StatusBadRequest)
			return
		}

		if shouldShortCircuit(body) {
			if debug {
				logger.Printf("scrub: short-circuit 400 (output_config.format) %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prism: output_config.format is not supported by the cluster gateway"}}`+"\n")
			return
		}

		body = scrubBody(body, logger, debug)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", fmt.Sprint(len(body)))
		next.ServeHTTP(w, r)
	})
}

func scrubBody(body []byte, logger *log.Logger, debug bool) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	for _, k := range stripFields {
		if _, ok := obj[k]; ok {
			delete(obj, k)
			changed = true
		}
	}
	streaming, _ := obj["stream"].(bool)
	if !streaming {
		if mt, ok := numberAsFloat(obj["max_tokens"]); ok && mt > nonStreamMaxTokensCap {
			if debug {
				logger.Printf("scrub: capping max_tokens %.0f → %d (non-streaming)", mt, nonStreamMaxTokensCap)
			}
			obj["max_tokens"] = nonStreamMaxTokensCap
			changed = true
		}
	}
	if !changed {
		return body
	}
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}

func shouldShortCircuit(body []byte) bool {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	oc, _ := obj["output_config"].(map[string]any)
	if oc == nil {
		return false
	}
	_, hasFormat := oc["format"]
	return hasFormat
}

func numberAsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

// --- request logging ---

func logRequests(next http.Handler, logger *log.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		reqSize := r.Header.Get("Content-Length")
		if reqSize == "" {
			reqSize = "?"
		}
		sr := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(sr, r)
		logger.Printf("%s %s %d req=%sB resp=%dB %s",
			r.Method, r.URL.Path, sr.status, reqSize, sr.bytes, time.Since(start).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
