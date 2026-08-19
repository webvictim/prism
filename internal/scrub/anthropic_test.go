package scrub

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var testLogger = log.New(io.Discard, "", 0)

// anthropicPOST runs a JSON POST body through AnthropicMiddleware and
// returns the body the downstream handler observed.
func anthropicPOST(t *testing.T, body string) (map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	var gotBody map[string]any
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if len(b) > 0 {
			if err := json.Unmarshal(b, &gotBody); err != nil {
				t.Fatalf("downstream body is not JSON: %v", err)
			}
		}
		if r.ContentLength != int64(len(b)) {
			t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(b))
		}
	})
	handler := AnthropicMiddleware(next, testLogger, false)

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return gotBody, rec
}

func TestAnthropicStripsAuthHeaders(t *testing.T) {
	var gotHeaders http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
	})
	handler := AnthropicMiddleware(next, testLogger, false)

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
}

func TestAnthropicPreservesAPIHeaders(t *testing.T) {
	// The gateway accepts Anthropic-Version/Anthropic-Beta and the SDK's
	// X-Stainless-* headers — the scrub must not touch them. (Regression:
	// forward-proxy mode used to strip these while chasing Bedrock 400s.)
	var gotHeaders http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
	})
	handler := AnthropicMiddleware(next, testLogger, false)

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(`{"model":"claude-3"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("X-Stainless-Lang", "js")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	for _, h := range []string{"Anthropic-Version", "Anthropic-Beta", "X-Stainless-Lang"} {
		if gotHeaders.Get(h) == "" {
			t.Errorf("header %s was stripped, want preserved", h)
		}
	}
}

func TestAnthropicStripsUnsupportedFields(t *testing.T) {
	body := `{"model":"claude-3","messages":[],"metadata":{"user":"x"},"context_management":{},"thinking":{"type":"enabled"},"diagnostics":{},"output_config":{"effort":"high"},"max_tokens":100}`
	gotBody, _ := anthropicPOST(t, body)

	for _, field := range anthropicStripFields {
		if _, ok := gotBody[field]; ok {
			t.Errorf("field %q was not stripped", field)
		}
	}
	if gotBody["model"] != "claude-3" {
		t.Error("model field was unexpectedly modified")
	}
}

func TestAnthropicPreservesToolFields(t *testing.T) {
	// Claude Code adds eager_input_streaming/defer_loading to tool
	// definitions when talking to (what it believes is) the real API.
	// The gateway ignores unknown tool keys, so prism leaves them
	// alone — tamper with requests as little as possible. (The strip
	// field in this body forces a rewrite, exercising the tools
	// round-trip rather than the untouched-body fast path.)
	body := `{"model":"claude-3","messages":[],"metadata":{},"tools":[
		{"name":"Bash","description":"run","input_schema":{"type":"object"},"eager_input_streaming":true},
		{"name":"DeferredToolPlaceholder","description":"d","input_schema":{"type":"object"},"defer_loading":true}
	]}`
	gotBody, _ := anthropicPOST(t, body)

	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("tools = %v, want 2 entries", gotBody["tools"])
	}
	if eis, _ := tools[0].(map[string]any)["eager_input_streaming"].(bool); !eis {
		t.Error("eager_input_streaming was stripped, want preserved")
	}
	if dl, _ := tools[1].(map[string]any)["defer_loading"].(bool); !dl {
		t.Error("defer_loading was stripped, want preserved")
	}
}

func TestAnthropicScrubsCacheControlScope(t *testing.T) {
	// Claude Code in forward-proxy (OAuth) mode adds "scope" to
	// cache_control (prompt-caching-scope beta); Bedrock rejects the
	// unknown key with a generic invalid-request 400. It must be
	// removed everywhere: system blocks, message content blocks,
	// nested tool_result content, and tool definitions.
	body := `{"model":"claude-3","max_tokens":100,
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","scope":"session"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out","cache_control":{"type":"ephemeral","scope":"global"}}]}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Edit","input":{"cache_control":{"scope":"keep-me"}}}]}
		],
		"tools":[{"name":"Bash","description":"d","input_schema":{"type":"object","properties":{"cache_control":{"type":"string","description":"keep-me"}}},"cache_control":{"type":"ephemeral","scope":"global"}}]}`
	gotBody, _ := anthropicPOST(t, body)

	raw, _ := json.Marshal(gotBody)
	if n := strings.Count(string(raw), "keep-me"); n != 2 {
		t.Errorf("tool_use input / input_schema were modified (found %d of 2 keep-me markers): %s", n, raw)
	}
	if strings.Contains(string(raw), `"scope":"global"`) || strings.Contains(string(raw), `"scope":"session"`) {
		t.Errorf("cache_control scope not fully scrubbed: %s", raw)
	}
	// The allowed keys survive.
	sys := gotBody["system"].([]any)[0].(map[string]any)
	cc := sys["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" || cc["ttl"] != "1h" {
		t.Errorf("system cache_control lost allowed keys: %v", cc)
	}
}

func TestAnthropicCapsMaxTokensNonStreaming(t *testing.T) {
	gotBody, _ := anthropicPOST(t, `{"model":"claude-3","messages":[],"max_tokens":16384}`)
	if mt, _ := gotBody["max_tokens"].(float64); mt != NonStreamMaxTokensCap {
		t.Errorf("max_tokens = %v, want %d", mt, NonStreamMaxTokensCap)
	}
}

func TestAnthropicDoesNotCapMaxTokensStreaming(t *testing.T) {
	gotBody, _ := anthropicPOST(t, `{"model":"claude-3","messages":[],"max_tokens":64000,"stream":true}`)
	if mt, _ := gotBody["max_tokens"].(float64); mt != 64000 {
		t.Errorf("max_tokens = %v, want 64000 (streaming should not be capped)", mt)
	}
}

func TestAnthropicShortCircuitsOutputConfigFormat(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not be called")
	})
	handler := AnthropicMiddleware(next, testLogger, false)

	body := `{"model":"claude-3","messages":[],"output_config":{"format":"json"}}`
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("invalid_request_error")) {
		t.Errorf("body = %s, want anthropic-style error JSON", rec.Body.String())
	}
}

func TestAnthropicLeavesCleanBodyUntouched(t *testing.T) {
	// A body with nothing to scrub must pass through byte-identical
	// (no JSON re-marshalling, which would reorder keys).
	body := `{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`
	var gotRaw []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw, _ = io.ReadAll(r.Body)
	})
	handler := AnthropicMiddleware(next, testLogger, false)

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if string(gotRaw) != body {
		t.Errorf("clean body was rewritten:\n got: %s\nwant: %s", gotRaw, body)
	}
}

func TestAnthropicPassesNonPOSTThrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler := AnthropicMiddleware(next, testLogger, false)

	req := httptest.NewRequest("GET", "/v1/messages", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !called {
		t.Error("next handler was not called for GET request")
	}
}

func TestAnthropicPassesNonMessagesPathThrough(t *testing.T) {
	body := `{"metadata":{"user":"x"}}`
	var gotRaw []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw, _ = io.ReadAll(r.Body)
	})
	handler := AnthropicMiddleware(next, testLogger, false)

	req := httptest.NewRequest("POST", "/v1/complete", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if string(gotRaw) != body {
		t.Errorf("non-/v1/messages body was modified: %s", gotRaw)
	}
}

func TestAnthropicPassesInvalidJSONThrough(t *testing.T) {
	body := `not json at all`
	var gotRaw []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw, _ = io.ReadAll(r.Body)
	})
	handler := AnthropicMiddleware(next, testLogger, false)

	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if string(gotRaw) != body {
		t.Errorf("invalid JSON body was modified: %s", gotRaw)
	}
}
