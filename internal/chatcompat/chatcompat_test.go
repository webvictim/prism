package chatcompat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// gatewayUnsupported is the gateway's real wording, verified live.
const gatewayUnsupported = `{"type":"error","error":{"type":"invalid_request_error",` +
	`"message":"the inference provider rejected the request as invalid. Check the request ` +
	`body for unsupported or invalid fields: Unsupported parameter: 'temperature' is not ` +
	`supported with this model."}}`

// testHandler wires a Handler to a stub upstream.
func testHandler(t *testing.T, upstream *httptest.Server, translate bool) *Handler {
	t.Helper()
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	h := New(1, discardLogger(), false, translate)
	h.Upstream = u
	return h
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRetriesAfterUnsupportedParameter(t *testing.T) {
	var seen []map[string]any
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		seen = append(seen, obj)
		paths = append(paths, r.URL.Path)
		if _, ok := obj["temperature"]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(gatewayUnsupported))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesReply))
	}))
	defer upstream.Close()

	h := testHandler(t, upstream, true)
	rec := post(t, h, `{"model":"m","temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (reject then retry)", len(seen))
	}
	if _, ok := seen[1]["temperature"]; ok {
		t.Error("retry still carried temperature")
	}
	for _, p := range paths {
		if p != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", p)
		}
	}
	if !strings.Contains(rec.Body.String(), "Hi there, friend!") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestRejectedParameterIsRememberedPerModel(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls++
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		if _, ok := obj["temperature"]; ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(gatewayUnsupported))
			return
		}
		_, _ = w.Write([]byte(responsesReply))
	}))
	defer upstream.Close()

	h := testHandler(t, upstream, true)
	body := `{"model":"m","temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`
	post(t, h, body)
	if calls != 2 {
		t.Fatalf("first call made %d upstream requests, want 2", calls)
	}
	post(t, h, body)
	if calls != 3 {
		t.Errorf("second call made %d upstream requests total, want 3 — the rejection should be remembered", calls)
	}

	// A different model hasn't been learned yet, so it pays the retry.
	post(t, h, `{"model":"other","temperature":0.3,"messages":[{"role":"user","content":"hi"}]}`)
	if calls != 5 {
		t.Errorf("total upstream requests = %d, want 5 (per-model memory)", calls)
	}
}

func TestGivesUpRatherThanLoopingForever(t *testing.T) {
	calls := 0
	// Always complains about a parameter that isn't in the body, so the
	// retry can't make progress.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'nonexistent' is not supported with this model."}}`))
	}))
	defer upstream.Close()

	h := testHandler(t, upstream, true)
	rec := post(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the gateway's 400 relayed", rec.Code)
	}
	if calls != 1 {
		t.Errorf("upstream called %d times, want 1 — nothing to drop means no retry", calls)
	}
}

func TestClientAuthNeverReachesUpstream(t *testing.T) {
	var gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		_, _ = w.Write([]byte(responsesReply))
	}))
	defer upstream.Close()

	h := testHandler(t, upstream, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer teleport")
	req.Header.Set("X-Api-Key", "teleport")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotAuth != "" || gotAPIKey != "" {
		t.Errorf("upstream saw auth headers: Authorization=%q X-Api-Key=%q", gotAuth, gotAPIKey)
	}
}

func TestToolsAreRejectedWithGuidance(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called for a tool request")
	}))
	defer upstream.Close()

	h := testHandler(t, upstream, true)
	rec := post(t, h, `{"model":"m","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/v1/responses") {
		t.Errorf("error should point at /v1/responses, got %s", rec.Body.String())
	}
}

func TestEmptyToolsArrayIsNotToolUse(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(responsesReply))
	}))
	defer upstream.Close()

	h := testHandler(t, upstream, true)
	rec := post(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[]}`)
	if !called || rec.Code != http.StatusOK {
		t.Errorf("empty tools array should be forwarded: called=%v status=%d", called, rec.Code)
	}
}

// With the shim off, the body must reach /v1/chat/completions unaltered —
// this is the legacy-gateway path.
func TestShimOffRelaysVerbatim(t *testing.T) {
	var gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"chat.completion","model":"legacy"}`))
	}))
	defer upstream.Close()

	h := testHandler(t, upstream, false)
	rec := post(t, h, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":40}`)

	if gotPath != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", gotPath)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(gotBody), &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["messages"]; !ok {
		t.Error("messages should not be translated when the shim is off")
	}
	if _, ok := obj["max_tokens"]; !ok {
		t.Error("max_tokens should not be renamed when the shim is off")
	}
	if rec.Body.String() != `{"object":"chat.completion","model":"legacy"}` {
		t.Errorf("reply = %s, want verbatim relay", rec.Body.String())
	}
}

func TestUpstreamErrorIsRelayedVerbatim(t *testing.T) {
	const gwErr = `{"error":{"message":"model ` + "`x`" + ` isn't supported on this route"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(gwErr))
	}))
	defer upstream.Close()

	h := testHandler(t, upstream, false)
	rec := post(t, h, `{"model":"x","messages":[]}`)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != gwErr {
		t.Errorf("status=%d body=%s — the gateway's own message must reach the user", rec.Code, rec.Body.String())
	}
}

func TestBadRequestsAreRejectedLocally(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()
	h := testHandler(t, upstream, true)

	if rec := post(t, h, `not json`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON: status = %d, want 400", rec.Code)
	}
	if rec := post(t, h, `{"model":"m"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing messages: status = %d, want 400", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}
}

func TestUnsupportedParamExtraction(t *testing.T) {
	h := New(1, discardLogger(), false, true)
	if f, ok := h.unsupported([]byte(gatewayUnsupported)); !ok || f != "temperature" {
		t.Errorf("got (%q, %v), want temperature", f, ok)
	}
	if _, ok := h.unsupported([]byte(`{"error":{"message":"something else entirely"}}`)); ok {
		t.Error("should not match an unrelated error")
	}
}
