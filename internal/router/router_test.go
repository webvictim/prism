package router

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
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
