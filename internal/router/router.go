// Package router provides a local HTTP server that dispatches requests
// to backend tunnels based on URL path, with Bedrock-compatibility
// scrubbing on the Anthropic path.
package router

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/scrub"
	"github.com/webvictim/prism/internal/usage"
)

// OpenAIAppName is the Teleport app name for the cluster-wide OpenAI gateway.
const OpenAIAppName = "openai"

// AnthropicAppName is the Teleport app name for the cluster-wide Anthropic gateway.
const AnthropicAppName = "anthropic"

// Config bundles the router's startup parameters.
type Config struct {
	ListenPort    int         // The externally-visible port (e.g. 7331).
	AnthropicPort int         // Internal port where the anthropic tunnel binds.
	OpenAIPort    int         // Internal port where the openai tunnel binds.
	Logger        *log.Logger // Required.
	Debug         bool
	HealthHandler http.Handler // If set, mounted at /_prism/health.
	// ProxyHandler, if set, handles forward-proxy requests: HTTP
	// CONNECT and absolute-form plain-HTTP requests (clients that use
	// HTTPS_PROXY without CONNECT, like the Remote Control bridge).
	ProxyHandler http.Handler
	UsageWriter  *usage.Writer
	Proxy        string // Teleport proxy address for usage tracking.
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
	anthropicHandler = scrub.AnthropicMiddleware(anthropicHandler, cfg.Logger, cfg.Debug)

	// Wrap the openai proxy with parameter normalization.
	var openaiHandler http.Handler = openaiProxy
	openaiHandler = scrub.OpenAIMiddleware(openaiHandler, cfg.Logger, cfg.Debug)

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

	// Wrap with forward-proxy dispatch: CONNECT requests and
	// absolute-form proxy requests go to the proxy handler; ordinary
	// origin-form requests fall through to path dispatch.
	if cfg.ProxyHandler != nil {
		next := handler
		proxyHandler := cfg.ProxyHandler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect || r.URL.IsAbs() {
				proxyHandler.ServeHTTP(w, r)
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
