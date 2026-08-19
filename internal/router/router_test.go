package router

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestIsOpenAIPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/v1/chat/completions", true},
		{"/v1/chat/completions/", true},
		{"/v1/responses", true},
		{"/v1/responses/resp_123", true},
		{"/v1/models", true},
		{"/v1/models/gpt-4", true},
		{"/v1/embeddings", true},
		{"/v1/completions", true},
		{"/v1/messages", false},
		{"/v1/messages/batch", false},
		{"/_prism/health", false},
		{"/", false},
		{"/v1/complete", false},
	}
	for _, tt := range tests {
		if got := isOpenAIPath(tt.path); got != tt.want {
			t.Errorf("isOpenAIPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestRouting(t *testing.T) {
	anthropicBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "anthropic")
		w.WriteHeader(200)
	}))
	defer anthropicBackend.Close()

	openaiBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "openai")
		w.WriteHeader(200)
	}))
	defer openaiBackend.Close()

	// Extract ports from test servers.
	anthropicPort := portFromURL(t, anthropicBackend.URL)
	openaiPort := portFromURL(t, openaiBackend.URL)

	svc, err := New(Config{
		ListenPort:    9999,
		AnthropicPort: anthropicPort,
		OpenAIPort:    openaiPort,
		Logger:        log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		path        string
		wantBackend string
	}{
		{"/v1/messages", "anthropic"},
		{"/v1/chat/completions", "openai"},
		{"/v1/responses", "openai"},
		{"/v1/models", "openai"},
		{"/v1/embeddings", "openai"},
		{"/", "anthropic"},
	}

	for _, tt := range tests {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", tt.path, nil)
		svc.server.Handler.ServeHTTP(rec, req)

		got := rec.Header().Get("X-Backend")
		if got != tt.wantBackend {
			t.Errorf("path=%s: routed to %q, want %q", tt.path, got, tt.wantBackend)
		}
	}
}

func TestProxyHandlerDispatch(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "path-dispatch")
	}))
	defer backend.Close()
	port := portFromURL(t, backend.URL)

	var proxied []string
	svc, err := New(Config{
		ListenPort:    9999,
		AnthropicPort: port,
		OpenAIPort:    port,
		Logger:        log.New(io.Discard, "", 0),
		ProxyHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxied = append(proxied, r.Method+" "+r.URL.String())
			w.Header().Set("X-Backend", "proxy")
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Absolute-form request (plain-HTTP forward proxy usage) goes to
	// the proxy handler, not path dispatch.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "https://api.anthropic.com/v1/environments/bridge", nil)
	svc.server.Handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Backend"); got != "proxy" {
		t.Errorf("absolute-form request routed to %q, want proxy handler", got)
	}

	// CONNECT goes to the proxy handler.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodConnect, "//example.com:443", nil)
	req.URL = &url.URL{Host: "example.com:443"}
	req.Host = "example.com:443"
	svc.server.Handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Backend"); got != "proxy" {
		t.Errorf("CONNECT routed to %q, want proxy handler", got)
	}

	// Ordinary origin-form request still path-dispatches.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/v1/messages", nil)
	svc.server.Handler.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Backend"); got != "path-dispatch" {
		t.Errorf("origin-form request routed to %q, want path-dispatch", got)
	}

	if len(proxied) != 2 {
		t.Errorf("proxy handler saw %d requests, want 2: %v", len(proxied), proxied)
	}
}

func portFromURL(t *testing.T, rawURL string) int {
	t.Helper()
	var port int
	// URL is http://127.0.0.1:<port>
	_, err := fmt.Sscanf(rawURL, "http://127.0.0.1:%d", &port)
	if err != nil {
		t.Fatalf("parse port from %q: %v", rawURL, err)
	}
	return port
}
