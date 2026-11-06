package chatcompat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/scrub"
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

// Translation is text only, so a tool-using request is relayed natively
// — which serves a chat/completions-only model (Kimi, GLM) in an agent —
// with its tools intact, and without first being tried on /v1/responses.
func TestToolsAreRelayedNatively(t *testing.T) {
	g := &routeGateway{chatOnly: map[string]bool{"kimi": true}}
	upstream := httptest.NewServer(g)
	defer upstream.Close()
	h := testHandler(t, upstream, true)

	rec := post(t, h, `{"model":"kimi","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`)
	if rec.Code != http.StatusOK || rec.Body.String() != chatReply {
		t.Fatalf("status = %d, body = %s; want the native reply", rec.Code, rec.Body.String())
	}
	if want := []string{"/v1/chat/completions"}; !slices.Equal(g.paths, want) {
		t.Fatalf("upstream paths = %v, want %v", g.paths, want)
	}
	if _, ok := g.bodies[0]["tools"]; !ok {
		t.Error("native relay dropped tools")
	}
}

// A Responses-only model can't take tools through the shim. The gateway's
// refusal turns into guidance pointing at /v1/responses, the route is
// remembered, and the next tool request is refused without a round trip.
func TestToolsForResponsesOnlyModelAreRefusedWithGuidance(t *testing.T) {
	g := &routeGateway{chatOnly: map[string]bool{}}
	upstream := httptest.NewServer(g)
	defer upstream.Close()
	h := testHandler(t, upstream, true)

	body := `{"model":"gpt","messages":[],"tools":[{"type":"function","function":{"name":"f"}}]}`
	for i := 0; i < 2; i++ {
		rec := post(t, h, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("request %d: status = %d, want 400", i, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "/v1/responses") {
			t.Errorf("request %d: error should point at /v1/responses, got %s", i, rec.Body.String())
		}
	}
	if want := []string{"/v1/chat/completions"}; !slices.Equal(g.paths, want) {
		t.Errorf("upstream paths = %v, want %v — the second refusal should need no request", g.paths, want)
	}

	// Learning it doesn't stop text requests being translated as before.
	if rec := post(t, h, `{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`); rec.Code != http.StatusOK {
		t.Errorf("text request after the refusal: status = %d", rec.Code)
	}
	if got := g.paths[len(g.paths)-1]; got != "/v1/responses" {
		t.Errorf("text request went to %s, want /v1/responses", got)
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

// openai_strip_fields applies to whatever this handler sends upstream,
// in both translate and relay modes.
func TestConfiguredFieldsAreStripped(t *testing.T) {
	scrub.SetExtra(scrub.Extra{OpenAIFields: []string{"speed"}})
	t.Cleanup(func() { scrub.SetExtra(scrub.Extra{}) })

	for _, translate := range []bool{true, false} {
		var obj map[string]any
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &obj)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(responsesReply))
		}))
		h := testHandler(t, upstream, translate)
		post(t, h, `{"model":"m","speed":"fast","messages":[{"role":"user","content":"hi"}]}`)
		upstream.Close()

		if obj == nil {
			t.Fatalf("translate=%v: upstream saw no request", translate)
		}
		if _, ok := obj["speed"]; ok {
			t.Errorf("translate=%v: speed reached upstream", translate)
		}
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

// The gateway's real route refusals, verified live on a tenant serving
// both kinds of model.
const (
	gatewayResponsesUnsupported = `{"type":"error","error":{"type":"invalid_request_error",` +
		`"message":"the inference provider rejected the request as invalid. Check the request ` +
		`body for unsupported or invalid fields: The model 'moonshotai.kimi-k2.5' does not ` +
		`support the '/openai/v1/responses' API"}}`
	gatewayChatRouteUnsupported = `{"type":"error","error":{"type":"invalid_request_error",` +
		`"message":"the inference provider rejected the request as invalid. Check the request ` +
		"body for unsupported or invalid fields: model `openai.gpt-6-luna` isn't supported on " +
		`this route"}}`
	chatReply = `{"object":"chat.completion","model":"moonshotai.kimi-k2.5"}`
)

// routeGateway serves each model on one route only, refusing it on the
// other with the gateway's own wording, and records every call.
type routeGateway struct {
	chatOnly map[string]bool // models served only on /v1/chat/completions
	paths    []string
	bodies   []map[string]any
}

func (g *routeGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var obj map[string]any
	_ = json.Unmarshal(b, &obj)
	g.paths = append(g.paths, r.URL.Path)
	g.bodies = append(g.bodies, obj)
	model, _ := obj["model"].(string)
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/responses" && g.chatOnly[model]:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(gatewayResponsesUnsupported))
	case r.URL.Path == "/v1/chat/completions" && !g.chatOnly[model]:
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(gatewayChatRouteUnsupported))
	case r.URL.Path == "/v1/responses":
		_, _ = w.Write([]byte(responsesReply))
	default:
		_, _ = w.Write([]byte(chatReply))
	}
}

// With the shim on, a chat/completions-only model is refused on
// /v1/responses. It must end up relayed natively, carrying the client's
// own body rather than a translated one, and only pay for the detour once.
func TestChatOnlyModelIsRelayedNatively(t *testing.T) {
	g := &routeGateway{chatOnly: map[string]bool{"kimi": true}}
	upstream := httptest.NewServer(g)
	defer upstream.Close()
	h := testHandler(t, upstream, true)

	body := `{"model":"kimi","messages":[{"role":"user","content":"hi"}],"max_tokens":40}`
	rec := post(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != chatReply {
		t.Errorf("reply = %s, want the native reply relayed verbatim", rec.Body.String())
	}
	if want := []string{"/v1/responses", "/v1/chat/completions"}; !slices.Equal(g.paths, want) {
		t.Fatalf("upstream paths = %v, want %v", g.paths, want)
	}
	native := g.bodies[1]
	if _, ok := native["messages"]; !ok {
		t.Error("native relay lost messages — it was sent the translated body")
	}
	if _, ok := native["input"]; ok {
		t.Error("native relay carried the Responses field input")
	}
	if _, ok := native["max_tokens"]; !ok {
		t.Error("native relay lost max_tokens")
	}

	post(t, h, body)
	if want := []string{"/v1/responses", "/v1/chat/completions", "/v1/chat/completions"}; !slices.Equal(g.paths, want) {
		t.Errorf("after a second request, paths = %v, want %v — the route should be remembered", g.paths, want)
	}

	// Another model hasn't been learned, so it still takes the default.
	post(t, h, `{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`)
	if got := g.paths[len(g.paths)-1]; got != "/v1/responses" || len(g.paths) != 4 {
		t.Errorf("unlearned model: paths = %v, want one more /v1/responses", g.paths)
	}
}

// The mirror image: with the shim off, a Responses-only model is refused
// on /v1/chat/completions and must be translated instead.
func TestResponsesOnlyModelIsTranslatedWithShimOff(t *testing.T) {
	g := &routeGateway{chatOnly: map[string]bool{}}
	upstream := httptest.NewServer(g)
	defer upstream.Close()
	h := testHandler(t, upstream, false)

	body := `{"model":"gpt","messages":[{"role":"user","content":"hi"}]}`
	rec := post(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Hi there, friend!") {
		t.Errorf("reply = %s, want the Responses reply translated back", rec.Body.String())
	}
	if want := []string{"/v1/chat/completions", "/v1/responses"}; !slices.Equal(g.paths, want) {
		t.Fatalf("upstream paths = %v, want %v", g.paths, want)
	}
	post(t, h, body)
	if got := g.paths[len(g.paths)-1]; got != "/v1/responses" || len(g.paths) != 3 {
		t.Errorf("after a second request, paths = %v, want one more /v1/responses", g.paths)
	}
}

// A model refused on both routes isn't served at all. One switch, then
// the gateway's answer goes to the client — no ping-pong.
func TestRouteSwitchesOnlyOnce(t *testing.T) {
	var paths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if r.URL.Path == "/v1/responses" {
			_, _ = w.Write([]byte(gatewayResponsesUnsupported))
		} else {
			_, _ = w.Write([]byte(gatewayChatRouteUnsupported))
		}
	}))
	defer upstream.Close()
	h := testHandler(t, upstream, true)

	rec := post(t, h, `{"model":"nowhere","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the gateway's 400", rec.Code)
	}
	if len(paths) != 2 {
		t.Errorf("upstream saw %d requests (%v), want 2", len(paths), paths)
	}
}

func TestWrongRoute(t *testing.T) {
	cases := []struct {
		name       string
		translated bool
		status     int
		body       string
		want       bool
	}{
		{"chat-only model on responses", true, 400, gatewayResponsesUnsupported, true},
		{"responses-only model on chat", false, 400, gatewayChatRouteUnsupported, true},
		// Each refusal only means something on the route that says it.
		{"responses refusal while relaying natively", false, 400, gatewayResponsesUnsupported, false},
		{"chat refusal while translating", true, 400, gatewayChatRouteUnsupported, false},
		{"a parameter refusal", true, 400, gatewayUnsupported, false},
		{"not a 400", true, 500, gatewayResponsesUnsupported, false},
	}
	for _, c := range cases {
		if got := wrongRoute(c.translated, c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: wrongRoute = %v, want %v", c.name, got, c.want)
		}
	}
}
