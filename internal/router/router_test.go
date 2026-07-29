package router

import (
	"bytes"
	"encoding/json"
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

func TestScrubMiddleware(t *testing.T) {
	logger := log.New(io.Discard, "", 0)

	t.Run("strips auth headers", func(t *testing.T) {
		var gotHeaders http.Header
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header
		})
		handler := scrubMiddleware(next, logger, false)

		req := httptest.NewRequest("GET", "/v1/messages", nil)
		req.Header.Set("Authorization", "Bearer sk-secret")
		req.Header.Set("X-Api-Key", "sk-secret")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if gotHeaders.Get("Authorization") != "" {
			t.Error("Authorization header was not stripped")
		}
		if gotHeaders.Get("X-Api-Key") != "" {
			t.Error("X-Api-Key header was not stripped")
		}
	})

	t.Run("strips unsupported fields", func(t *testing.T) {
		var gotBody map[string]any
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &gotBody)
		})
		handler := scrubMiddleware(next, logger, false)

		body := `{"model":"claude-3","messages":[],"metadata":{"user":"x"},"context_management":{},"thinking":{"type":"enabled"},"max_tokens":100}`
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		for _, field := range []string{"metadata", "context_management", "thinking"} {
			if _, ok := gotBody[field]; ok {
				t.Errorf("field %q was not stripped", field)
			}
		}
		if gotBody["model"] != "claude-3" {
			t.Error("model field was unexpectedly modified")
		}
	})

	t.Run("caps max_tokens for non-streaming", func(t *testing.T) {
		var gotBody map[string]any
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &gotBody)
		})
		handler := scrubMiddleware(next, logger, false)

		body := `{"model":"claude-3","messages":[],"max_tokens":16384}`
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		mt, _ := gotBody["max_tokens"].(float64)
		if mt != nonStreamMaxTokensCap {
			t.Errorf("max_tokens = %v, want %d", mt, nonStreamMaxTokensCap)
		}
	})

	t.Run("does not cap max_tokens for streaming", func(t *testing.T) {
		var gotBody map[string]any
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &gotBody)
		})
		handler := scrubMiddleware(next, logger, false)

		body := `{"model":"claude-3","messages":[],"max_tokens":16384,"stream":true}`
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		mt, _ := gotBody["max_tokens"].(float64)
		if mt != 16384 {
			t.Errorf("max_tokens = %v, want 16384 (streaming should not be capped)", mt)
		}
	})

	t.Run("short-circuits on output_config.format", func(t *testing.T) {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("next handler should not be called")
		})
		handler := scrubMiddleware(next, logger, false)

		body := `{"model":"claude-3","messages":[],"output_config":{"format":"json"}}`
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("passes non-POST through unchanged", func(t *testing.T) {
		called := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		})
		handler := scrubMiddleware(next, logger, false)

		req := httptest.NewRequest("GET", "/v1/messages", nil)
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if !called {
			t.Error("next handler was not called for GET request")
		}
	})
}

func TestOpenAIScrubMiddleware(t *testing.T) {
	logger := log.New(io.Discard, "", 0)

	t.Run("strips Authorization header", func(t *testing.T) {
		var gotHeaders http.Header
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeaders = r.Header
		})
		handler := openaiScrubMiddleware(next, logger, false)

		req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer sk-secret")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if gotHeaders.Get("Authorization") != "" {
			t.Error("Authorization header was not stripped")
		}
	})

	t.Run("renames max_tokens to max_completion_tokens", func(t *testing.T) {
		var gotBody map[string]any
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &gotBody)
		})
		handler := openaiScrubMiddleware(next, logger, false)

		body := `{"model":"gpt-4o","messages":[],"max_tokens":4096}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if _, ok := gotBody["max_tokens"]; ok {
			t.Error("max_tokens was not removed")
		}
		if mct, _ := gotBody["max_completion_tokens"].(float64); mct != 4096 {
			t.Errorf("max_completion_tokens = %v, want 4096", mct)
		}
	})

	t.Run("does not rename when max_completion_tokens already present", func(t *testing.T) {
		var gotBody map[string]any
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &gotBody)
		})
		handler := openaiScrubMiddleware(next, logger, false)

		body := `{"model":"gpt-4o","messages":[],"max_tokens":1000,"max_completion_tokens":2000}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if mt, _ := gotBody["max_tokens"].(float64); mt != 1000 {
			t.Errorf("max_tokens = %v, want 1000 (should be preserved)", mt)
		}
		if mct, _ := gotBody["max_completion_tokens"].(float64); mct != 2000 {
			t.Errorf("max_completion_tokens = %v, want 2000", mct)
		}
	})

	t.Run("strips temperature for reasoning models", func(t *testing.T) {
		for _, model := range []string{"o1-preview", "o3-mini", "o4-mini", "gpt-5.5"} {
			var gotBody map[string]any
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				json.Unmarshal(b, &gotBody)
			})
			handler := openaiScrubMiddleware(next, logger, false)

			body := fmt.Sprintf(`{"model":%q,"messages":[],"temperature":0}`, model)
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(httptest.NewRecorder(), req)

			if _, ok := gotBody["temperature"]; ok {
				t.Errorf("model=%s: temperature was not stripped", model)
			}
		}
	})

	t.Run("leaves temperature=1 for reasoning models", func(t *testing.T) {
		var gotBody map[string]any
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &gotBody)
		})
		handler := openaiScrubMiddleware(next, logger, false)

		body := `{"model":"gpt-5.5","messages":[],"temperature":1}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if _, ok := gotBody["temperature"]; !ok {
			t.Error("temperature=1 should be preserved for reasoning models")
		}
	})

	t.Run("leaves temperature for non-reasoning models", func(t *testing.T) {
		var gotBody map[string]any
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			json.Unmarshal(b, &gotBody)
		})
		handler := openaiScrubMiddleware(next, logger, false)

		body := `{"model":"gpt-4o","messages":[],"temperature":0}`
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(httptest.NewRecorder(), req)

		if temp, _ := gotBody["temperature"].(float64); temp != 0 {
			t.Errorf("temperature = %v, want 0 (non-reasoning model)", temp)
		}
	})
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
