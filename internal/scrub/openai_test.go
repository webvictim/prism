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
