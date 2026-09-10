package router

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/usage"
)

// captureRoundTrip sends one request through observeRequests backed by a
// fake handler, then returns the usage records that were written.
func captureRoundTrip(t *testing.T, method, path, reqBody string, backend http.HandlerFunc) []usage.Record {
	t.Helper()
	dir := t.TempDir()
	w, err := usage.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	handler := observeRequests(backend, log.New(io.Discard, "", 0), w, "teleport.example.com:443")

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
	// The response's model wins over the request's: the gateway aliases
	// unknown names, so what it served is the useful fact.
	if rec.Model != "gpt-4o-2024" {
		t.Errorf("model = %q, want the response's model", rec.Model)
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

// The Responses API spells its token counts input_/output_tokens rather
// than prompt_/completion_. Codex and anything else hitting /v1/responses
// directly returns this shape, so it has to be recognised or that traffic
// records no usage at all.
func TestCaptureOpenAIResponsesNonStreaming(t *testing.T) {
	body := `{"object":"response","id":"resp_1","model":"openai.gpt-5.6-sol",
	          "usage":{"input_tokens":41,"output_tokens":9,"total_tokens":50,
	                   "input_tokens_details":{"cached_tokens":8}}}`

	records := captureRoundTrip(t, "POST", "/v1/responses", `{"model":"openai.gpt-5.6-sol"}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, body)
		})

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.Backend != "openai" {
		t.Errorf("backend = %q, want openai", rec.Backend)
	}
	if rec.InputTokens != 41 || rec.OutputTokens != 9 {
		t.Errorf("tokens = %+v, want in=41 out=9", rec)
	}
	if rec.CacheRead != 8 {
		t.Errorf("cache read = %d, want 8", rec.CacheRead)
	}
}

func TestCaptureOpenAIResponsesStreaming(t *testing.T) {
	sse := `data: {"type":"response.created","response":{"id":"r","model":"openai.gpt-5.6-sol"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r","model":"openai.gpt-5.6-luna","usage":{"input_tokens":11,"output_tokens":52,"input_tokens_details":{"cached_tokens":3}}}}` + "\n\n"

	records := captureRoundTrip(t, "POST", "/v1/responses", `{"model":"gpt-5.5"}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, sse)
		})

	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	rec := records[0]
	if rec.InputTokens != 11 || rec.OutputTokens != 52 {
		t.Errorf("tokens = %+v, want in=11 out=52", rec)
	}
	if rec.CacheRead != 3 {
		t.Errorf("cache read = %d, want 3", rec.CacheRead)
	}
	// The gateway aliases unknown names, so the streamed model — what was
	// actually served — beats the alias the client asked for.
	if rec.Model != "openai.gpt-5.6-luna" {
		t.Errorf("model = %q, want the model the gateway served", rec.Model)
	}
}

// Clients may omit `model` entirely and let the gateway choose; the usage
// record must still name the model rather than falling back to "".
func TestCaptureRecordsModelWhenRequestOmitsIt(t *testing.T) {
	records := captureRoundTrip(t, "POST", "/v1/chat/completions", `{"messages":[]}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"model":"openai.gpt-5.6-luna","usage":{"prompt_tokens":5,"completion_tokens":2}}`)
		})
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	if records[0].Model != "openai.gpt-5.6-luna" {
		t.Errorf("model = %q, want the model the gateway served", records[0].Model)
	}
}
