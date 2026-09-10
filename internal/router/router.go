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

	"github.com/webvictim/prism/internal/chatcompat"
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
	// ChatCompletionsShim translates /v1/chat/completions into Responses
	// API calls, for gateways that only serve OpenAI models on
	// /v1/responses. When false the path is relayed unchanged.
	ChatCompletionsShim bool
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

	// /v1/chat/completions gets its own handler: the gateway may only
	// serve OpenAI models on /v1/responses, and either way this handler
	// reacts to per-model parameter rejections instead of prism carrying
	// a list of which models reject what.
	chatHandler := chatcompat.New(cfg.OpenAIPort, cfg.Logger, cfg.Debug, cfg.ChatCompletionsShim)
	if cfg.ChatCompletionsShim {
		cfg.Logger.Printf("router: /v1/chat/completions → Responses API translation enabled")
	}

	mux := http.NewServeMux()
	if cfg.HealthHandler != nil {
		mux.Handle("/_prism/health", cfg.HealthHandler)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isChatCompletionsPath(r.URL.Path) {
			chatHandler.ServeHTTP(w, r)
		} else if isOpenAIPath(r.URL.Path) {
			openaiHandler.ServeHTTP(w, r)
		} else {
			anthropicHandler.ServeHTTP(w, r)
		}
	})

	// Wrap with request logging and usage capture (one line per request).
	var handler http.Handler = observeRequests(mux, cfg.Logger, cfg.UsageWriter, cfg.Proxy)

	// Wrap with path canonicalisation, outside logging and capture so
	// both see the canonical spelling. Clients built on the Vercel AI
	// SDK append only /messages to the base URL, where the official
	// Anthropic and OpenAI SDKs append /v1/messages — and prism hands
	// out ANTHROPIC_BASE_URL without the /v1 for exactly that reason.
	handler = canonicalizePath(handler, cfg.Logger, cfg.Debug)

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

// isInternalPath returns true for prism's own endpoints, which are
// neither proxied nor worth logging.
func isInternalPath(path string) bool {
	return strings.HasPrefix(path, "/_prism/")
}

// canonicalAPIPath maps a version-less API path onto its /v1 spelling,
// reporting whether it rewrote anything.
//
// The Vercel AI SDK (OpenCode, and anything else built on it) appends
// only /messages to the configured base URL, where the official
// Anthropic and OpenAI SDKs append /v1/messages. Both spellings are
// accepted by the gateway, so a version-less request used to work while
// silently missing every /v1-gated behaviour: path dispatch, request
// logging, usage capture and — worst — Bedrock scrubbing.
//
// Canonicalising once here keeps that knowledge in a single predicate
// instead of duplicating it across each of those gates, and forwards
// upstream the same well-trodden path the official SDKs send.
func canonicalAPIPath(path string) (string, bool) {
	if strings.HasPrefix(path, "/v1/") || isInternalPath(path) {
		return path, false
	}
	candidate := "/v1" + path
	if !isKnownAPIPath(candidate) {
		return path, false
	}
	return candidate, true
}

// isKnownAPIPath reports whether a /v1 path names an endpoint prism
// knows how to dispatch.
func isKnownAPIPath(path string) bool {
	if isOpenAIPath(path) {
		return true
	}
	// The anthropic tunnel is the catch-all, so only its real endpoints
	// count as known — otherwise "/" would canonicalise to "/v1/".
	return path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/")
}

// canonicalizePath rewrites version-less API paths before the rest of
// the chain sees them.
func canonicalizePath(next http.Handler, logger *log.Logger, debug bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical, rewrote := canonicalAPIPath(r.URL.Path)
		if rewrote {
			if debug && logger != nil {
				logger.Printf("router: canonicalised %s → %s", r.URL.Path, canonical)
			}
			r.URL.Path = canonical
		}
		next.ServeHTTP(w, r)
	})
}

// isChatCompletionsPath returns true for the chat/completions endpoint,
// which is served by the chatcompat handler rather than proxied directly.
func isChatCompletionsPath(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/chat/completions/"
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
