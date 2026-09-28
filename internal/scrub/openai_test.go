package scrub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func openaiPOST(t *testing.T, body string) map[string]any {
	t.Helper()
	var gotBody map[string]any
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
	})
	handler := OpenAIMiddleware(next, testLogger, false)

	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return gotBody
}

func TestOpenAIStripsAuthorizationHeader(t *testing.T) {
	var gotHeaders http.Header
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header
	})
	handler := OpenAIMiddleware(next, testLogger, false)

	req := httptest.NewRequest("GET", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-secret")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if gotHeaders.Get("Authorization") != "" {
		t.Error("Authorization header was not stripped")
	}
}

func TestOpenAIRenamesMaxTokens(t *testing.T) {
	gotBody := openaiPOST(t, `{"model":"gpt-4o","messages":[],"max_tokens":4096}`)

	if _, ok := gotBody["max_tokens"]; ok {
		t.Error("max_tokens was not removed")
	}
	if mct, _ := gotBody["max_completion_tokens"].(float64); mct != 4096 {
		t.Errorf("max_completion_tokens = %v, want 4096", mct)
	}
}

func TestOpenAIKeepsExistingMaxCompletionTokens(t *testing.T) {
	gotBody := openaiPOST(t, `{"model":"gpt-4o","messages":[],"max_tokens":1000,"max_completion_tokens":2000}`)

	if mt, _ := gotBody["max_tokens"].(float64); mt != 1000 {
		t.Errorf("max_tokens = %v, want 1000 (should be preserved)", mt)
	}
	if mct, _ := gotBody["max_completion_tokens"].(float64); mct != 2000 {
		t.Errorf("max_completion_tokens = %v, want 2000", mct)
	}
}

// Temperature and other per-model parameter quirks are no longer handled
// here: internal/chatcompat discovers them from the gateway's error
// message, so no model names live in prism. A non-default temperature
// must therefore pass through untouched.
func TestOpenAIPassesTemperatureThrough(t *testing.T) {
	gotBody := openaiPOST(t, `{"model":"some-model","messages":[],"temperature":0}`)
	temp, ok := gotBody["temperature"].(float64)
	if !ok || temp != 0 {
		t.Errorf("temperature = %v (ok=%v), want 0 passed through", gotBody["temperature"], ok)
	}
}

// responsesPOST sends body to /v1/responses and returns what reached
// downstream, raw, so byte-for-byte passthrough can be checked.
func responsesPOST(t *testing.T, body string) []byte {
	t.Helper()
	var got []byte
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		if r.ContentLength != int64(len(got)) {
			t.Errorf("ContentLength = %d, want %d", r.ContentLength, len(got))
		}
	})
	req := httptest.NewRequest("POST", "/v1/responses", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	OpenAIMiddleware(next, testLogger, false).ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestOpenAIResponsesUntouchedByDefault(t *testing.T) {
	// No openai_strip_* config: Responses bodies pass byte-for-byte,
	// including max_tokens (the rename is chat/completions only).
	body := `{"model":"m",  "input":"hi","max_tokens":5,"tools":[{"type":"web_search"}]}`
	if got := responsesPOST(t, body); string(got) != body {
		t.Errorf("body modified:\n got: %s\nwant: %s", got, body)
	}
}

func TestOpenAIResponsesStripsConfigured(t *testing.T) {
	SetExtra(Extra{OpenAIFields: []string{"speed"}, OpenAIToolTypes: []string{"web_search"}})
	t.Cleanup(func() { SetExtra(Extra{}) })

	var gotBody map[string]any
	raw := responsesPOST(t, `{"model":"m","input":"hi","speed":"fast","max_tokens":5,
		"tools":[{"type":"function","name":"Bash"},{"type":"web_search_preview"}],
		"tool_choice":{"type":"web_search_preview"}}`)
	if err := json.Unmarshal(raw, &gotBody); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBody["speed"]; ok {
		t.Error("speed was not stripped")
	}
	if _, ok := gotBody["max_tokens"]; !ok {
		t.Error("max_tokens was renamed on /v1/responses")
	}
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "Bash" {
		t.Errorf("tools = %v, want only Bash", gotBody["tools"])
	}
	if _, ok := gotBody["tool_choice"]; ok {
		t.Error("tool_choice selecting the removed hosted tool was kept")
	}
}

func TestOpenAIChatStripsConfiguredFields(t *testing.T) {
	SetExtra(Extra{OpenAIFields: []string{"speed"}})
	t.Cleanup(func() { SetExtra(Extra{}) })

	gotBody := openaiPOST(t, `{"model":"m","messages":[],"speed":"fast"}`)
	if _, ok := gotBody["speed"]; ok {
		t.Error("speed was not stripped")
	}
}
