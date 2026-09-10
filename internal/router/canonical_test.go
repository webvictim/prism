package router

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/usage"
)

func TestCanonicalAPIPath(t *testing.T) {
	for _, tt := range []struct {
		path       string
		want       string
		wantRewrit bool
	}{
		// Version-less spellings, as sent by the Vercel AI SDK.
		{"/messages", "/v1/messages", true},
		{"/messages/batches", "/v1/messages/batches", true},
		{"/responses", "/v1/responses", true},
		{"/responses/resp_123", "/v1/responses/resp_123", true},
		{"/chat/completions", "/v1/chat/completions", true},
		{"/models", "/v1/models", true},
		{"/models/gpt-4", "/v1/models/gpt-4", true},
		{"/embeddings", "/v1/embeddings", true},
		{"/completions", "/v1/completions", true},

		// Already canonical — left alone.
		{"/v1/messages", "/v1/messages", false},
		{"/v1/responses", "/v1/responses", false},
		{"/v1/chat/completions", "/v1/chat/completions", false},

		// prism's own endpoints are never rewritten.
		{"/_prism/health", "/_prism/health", false},

		// Unknown paths are forwarded as-is rather than guessed at. In
		// particular "/" must not become "/v1/".
		{"/", "/", false},
		{"/complete", "/complete", false},
		{"/v1/complete", "/v1/complete", false},
		{"/environments/bridge", "/environments/bridge", false},
	} {
		got, rewrote := canonicalAPIPath(tt.path)
		if got != tt.want || rewrote != tt.wantRewrit {
			t.Errorf("canonicalAPIPath(%q) = (%q, %v), want (%q, %v)",
				tt.path, got, rewrote, tt.want, tt.wantRewrit)
		}
	}
}

// A version-less request must land on the same tunnel, arrive scrubbed,
// be logged and be metered — all four used to be skipped because every
// gate was anchored on a /v1/ prefix.
func TestVersionlessAnthropicRequestIsFullyHandled(t *testing.T) {
	var gotPath string
	var gotBody []byte
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-opus-5","usage":{"input_tokens":11,"output_tokens":5}}`))
	}))
	defer anthropic.Close()

	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("openai tunnel received %s %s, want anthropic", r.Method, r.URL.Path)
	}))
	defer openai.Close()

	dir := t.TempDir()
	uw, err := usage.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	svc, err := New(Config{
		ListenPort:    9999,
		AnthropicPort: portFromURL(t, anthropic.URL),
		OpenAIPort:    portFromURL(t, openai.URL),
		Logger:        log.New(&logBuf, "", 0),
		UsageWriter:   uw,
		Proxy:         "teleport.example.com:443",
	})
	if err != nil {
		t.Fatal(err)
	}

	// "thinking" is one of the fields Bedrock rejects, so it doubles as
	// the marker for whether scrubbing ran at all.
	body := `{"max_tokens":16,"thinking":{"type":"enabled","budget_tokens":1024},` +
		`"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	svc.server.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// Dispatch: reached the anthropic tunnel on the canonical path.
	if gotPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", gotPath)
	}

	// Scrubbing: the Bedrock-incompatible field is gone.
	var forwarded map[string]any
	if err := json.Unmarshal(gotBody, &forwarded); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	if _, ok := forwarded["thinking"]; ok {
		t.Errorf("thinking survived scrubbing: %s", gotBody)
	}

	// Logging: one line, naming the canonical path, carrying the usage
	// figures inline rather than on a second line.
	logged := strings.TrimSpace(logBuf.String())
	if strings.Count(logged, "\n") != 0 {
		t.Errorf("expected a single log line, got:\n%s", logged)
	}
	for _, want := range []string{
		"POST /v1/messages 200",
		"model=claude-opus-5 in=11 out=5 cache_read=0 cache_write=0",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log line missing %q; line was:\n%s", want, logged)
		}
	}

	// Metering: a record with the model the gateway reported.
	_ = uw.Close()
	records, err := usage.Load(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d usage records, want 1", len(records))
	}
	if records[0].Model != "claude-opus-5" {
		t.Errorf("model = %q, want claude-opus-5", records[0].Model)
	}
	if records[0].Backend != "anthropic" {
		t.Errorf("backend = %q, want anthropic", records[0].Backend)
	}
	if records[0].InputTokens != 11 || records[0].OutputTokens != 5 {
		t.Errorf("tokens = in %d/out %d, want 11/5", records[0].InputTokens, records[0].OutputTokens)
	}
}

// A version-less chat/completions request must reach the openai tunnel
// through the shim, not fall through to the anthropic catch-all.
func TestVersionlessChatCompletionsReachesOpenAI(t *testing.T) {
	var gotPath string
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","model":"gpt-5","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer openai.Close()

	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("anthropic tunnel received %s %s, want openai", r.Method, r.URL.Path)
	}))
	defer anthropic.Close()

	svc, err := New(Config{
		ListenPort:          9999,
		AnthropicPort:       portFromURL(t, anthropic.URL),
		OpenAIPort:          portFromURL(t, openai.URL),
		Logger:              log.New(io.Discard, "", 0),
		ChatCompletionsShim: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	svc.server.Handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotPath != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", gotPath)
	}
}

// Health checks stay out of the request log; everything else that gets
// proxied stays in it, even paths prism does not recognise.
func TestLoggingCoversUnrecognisedProxiedPaths(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	var logBuf bytes.Buffer
	svc, err := New(Config{
		ListenPort:    9999,
		AnthropicPort: portFromURL(t, backend.URL),
		OpenAIPort:    portFromURL(t, backend.URL),
		Logger:        log.New(&logBuf, "", 0),
		HealthHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.server.Handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/_prism/health", nil))
	if logBuf.Len() != 0 {
		t.Errorf("health check was logged: %s", logBuf.String())
	}

	svc.server.Handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/some/new/endpoint", nil))
	if !strings.Contains(logBuf.String(), "POST /some/new/endpoint 200") {
		t.Errorf("proxied path not logged; log was:\n%s", logBuf.String())
	}
}

// A request that carries no token usage — a GET, or anything the gateway
// answers without a usage object — still gets its request line, without
// the usage fields dangling off it.
func TestRequestsWithoutUsageLogNoFields(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer backend.Close()

	dir := t.TempDir()
	uw, err := usage.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer uw.Close()

	var logBuf bytes.Buffer
	svc, err := New(Config{
		ListenPort:    9999,
		AnthropicPort: portFromURL(t, backend.URL),
		OpenAIPort:    portFromURL(t, backend.URL),
		Logger:        log.New(&logBuf, "", 0),
		UsageWriter:   uw,
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.server.Handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("GET", "/v1/models", nil))

	logged := strings.TrimSpace(logBuf.String())
	if !strings.Contains(logged, "GET /v1/models 200") {
		t.Errorf("request not logged; line was:\n%s", logged)
	}
	if strings.Contains(logged, "model=") || strings.Contains(logged, "in=") {
		t.Errorf("usage fields present on a request with no usage:\n%s", logged)
	}
}
