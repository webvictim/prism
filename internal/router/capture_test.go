package router

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gravitational/prism/internal/usage"
)

// captureRoundTrip sends one request through captureUsage backed by a
// fake handler, then returns the usage records that were written.
func captureRoundTrip(t *testing.T, method, path, reqBody string, backend http.HandlerFunc) []usage.Record {
	t.Helper()
	dir := t.TempDir()
	w, err := usage.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	handler := captureUsage(backend, w, "teleport.example.com:443", log.New(io.Discard, "", 0))

	req := httptest.NewRequest(method, path, strings.NewReader(reqBody))
	if reqBody != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	handler.ServeHTTP(httptest.NewRecorder(), req)

	w.Close()
	records, err := usage.Load(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestCaptureAnthropicNonStreaming(t *testing.T) {
	records := captureRoundTrip(t, "POST", "/v1/messages", `{"model":"claude-opus-4-6"}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"model":"claude-opus-4-6","usage":{"input_tokens":100,"output_tokens":25,"cache_read_input_tokens":50,"cache_creation_input_tokens":10}}`)
		})

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.Backend != "anthropic" || rec.Model != "claude-opus-4-6" {
		t.Errorf("backend=%s model=%s, want anthropic/claude-opus-4-6", rec.Backend, rec.Model)
	}
	if rec.InputTokens != 100 || rec.OutputTokens != 25 || rec.CacheRead != 50 || rec.CacheCreate != 10 {
		t.Errorf("tokens = %+v, want in=100 out=25 cacheRead=50 cacheCreate=10", rec)
	}
	if rec.Proxy != "teleport.example.com:443" {
		t.Errorf("proxy = %q", rec.Proxy)
	}
}

func TestCaptureAnthropicStreaming(t *testing.T) {
	sse := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-opus-4-6","usage":{"input_tokens":200,"output_tokens":1,"cache_read_input_tokens":80}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	records := captureRoundTrip(t, "POST", "/v1/messages", `{"model":"claude-opus-4-6"}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			// Write in two chunks to exercise partial-line buffering.
			half := len(sse) / 2
			io.WriteString(w, sse[:half])
			w.(http.Flusher).Flush()
			io.WriteString(w, sse[half:])
		})

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.InputTokens != 200 || rec.OutputTokens != 42 || rec.CacheRead != 80 {
		t.Errorf("tokens = %+v, want in=200 out=42 cacheRead=80", rec)
	}
	if rec.Model != "claude-opus-4-6" {
		t.Errorf("model = %q", rec.Model)
	}
}

func TestCaptureOpenAINonStreaming(t *testing.T) {
	records := captureRoundTrip(t, "POST", "/v1/chat/completions", `{"model":"gpt-4o"}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"model":"gpt-4o-2024","usage":{"prompt_tokens":30,"completion_tokens":7}}`)
		})

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.Backend != "openai" {
		t.Errorf("backend = %q, want openai", rec.Backend)
	}
	// For non-streaming responses the request's model is recorded
	// (finalize overwrites with the request-derived model).
	if rec.Model != "gpt-4o" {
		t.Errorf("model = %q, want gpt-4o", rec.Model)
	}
	if rec.InputTokens != 30 || rec.OutputTokens != 7 {
		t.Errorf("tokens = %+v, want in=30 out=7", rec)
	}
}

func TestCaptureOpenAIStreaming(t *testing.T) {
	sse := `data: {"model":"gpt-4o","choices":[{"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"model":"gpt-4o","usage":{"prompt_tokens":12,"completion_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"

	records := captureRoundTrip(t, "POST", "/v1/chat/completions", `{"model":"gpt-4o"}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sse)
		})

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.InputTokens != 12 || rec.OutputTokens != 3 {
		t.Errorf("tokens = %+v, want in=12 out=3", rec)
	}
}

func TestCaptureSkipsErrorResponses(t *testing.T) {
	records := captureRoundTrip(t, "POST", "/v1/messages", `{"model":"claude-3"}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"usage":{"input_tokens":100,"output_tokens":25}}`)
		})

	if len(records) != 0 {
		t.Fatalf("got %d records for a 400 response, want 0", len(records))
	}
}

func TestCaptureSkipsNonAPIRequests(t *testing.T) {
	records := captureRoundTrip(t, "GET", "/_prism/health", "",
		func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
		})

	if len(records) != 0 {
		t.Fatalf("got %d records for a health check, want 0", len(records))
	}
}

func TestCaptureSkipsResponsesWithoutUsage(t *testing.T) {
	records := captureRoundTrip(t, "POST", "/v1/messages", `{"model":"claude-3"}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"msg_1","content":[]}`)
		})

	if len(records) != 0 {
		t.Fatalf("got %d records for a usage-less response, want 0", len(records))
	}
}
