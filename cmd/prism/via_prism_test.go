package main

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/webvictim/prism/internal/jsonc"
)

// fakeVia is a source whose probe answers from a table and counts calls,
// so tests can tell which models were asked about.
func fakeVia(anthropic, openai []string, chatOnly ...string) (*viaPrismSource, *[]string) {
	var mu sync.Mutex
	var probed []string
	src := &viaPrismSource{
		Anthropic: anthropic,
		OpenAI:    openai,
		Warn:      &bytes.Buffer{},
		Probe: func(m string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			probed = append(probed, m)
			if slices.Contains(chatOnly, m) {
				return routeChat, nil
			}
			return routeResponses, nil
		},
	}
	return src, &probed
}

func TestPlaceProbesOnlyUnknownOpenAIModels(t *testing.T) {
	src, probed := fakeVia([]string{"claude-a"}, []string{"gpt-a", "kimi", "gpt-b"}, "kimi")
	got := src.place(map[string]string{"gpt-b": routeResponses})
	want := map[string]string{"claude-a": routeMessages, "gpt-a": routeResponses, "kimi": routeChat, "gpt-b": routeResponses}
	if len(got) != len(want) {
		t.Fatalf("place = %v, want %v", got, want)
	}
	for m, r := range want {
		if got[m] != r {
			t.Errorf("%s: route %q, want %q", m, got[m], r)
		}
	}
	slices.Sort(*probed)
	if !slices.Equal(*probed, []string{"gpt-a", "kimi"}) {
		t.Errorf("probed %v, want only the models the config didn't have", *probed)
	}
}

// A model whose route can't be learned is left out, not guessed: the
// guess would be remembered by the provider it was filed under.
func TestPlaceLeavesOutUnprobeableModels(t *testing.T) {
	warn := &bytes.Buffer{}
	src := viaPrismSource{OpenAI: []string{"gpt"}, Warn: warn,
		Probe: func(string) (string, error) { return "", os.ErrDeadlineExceeded }}
	if got := src.place(nil); len(got) != 0 {
		t.Errorf("place = %v, want nothing", got)
	}
	if !strings.Contains(warn.String(), "left out gpt") {
		t.Errorf("warning = %q", warn.String())
	}
}

func TestProbeOpenAIRoute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("probe went to %s", r.URL.Path)
		}
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		switch {
		case strings.Contains(b.String(), `"kimi"`):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"The model 'moonshotai.kimi-k2.5' does not support the '/openai/v1/responses' API"}}`))
		case strings.Contains(b.String(), `"down"`):
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`tunnel down`))
		case strings.Contains(b.String(), `"odd"`):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"something else"}}`))
		default:
			_, _ = w.Write([]byte(`{"object":"response"}`))
		}
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	for model, want := range map[string]string{"gpt": routeResponses, "kimi": routeChat, "odd": routeResponses} {
		got, err := probeOpenAIRoute(port, model)
		if err != nil || got != want {
			t.Errorf("%s: route %q, err %v; want %q", model, got, err, want)
		}
	}
	if _, err := probeOpenAIRoute(port, "down"); err == nil {
		t.Error("a 502 from the router should be an error, not a route")
	}
}

const opencodeFixture = `{
  "$schema": "https://opencode.ai/config.json",
  // These bill straight to the vendor.
  "disabled_providers": ["openai", "anthropic"],
  "plugins": ["opencode-brave-websearch"],
  "provider": {
    // llama-swap on the LAN
    "gaia": {
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "http://127.0.0.1:8080/v1" },
      "models": { "qwen": { "name": "Qwen" } }
    }
  }
}
`

func readOpencode(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := jsonc.Unmarshal(b, &v); err != nil {
		t.Fatalf("written config doesn't parse: %v\n%s", err, b)
	}
	return v
}

func dig(v any, path ...string) any {
	for _, k := range path {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

func TestWriteOpencodeConfigAddsViaPrismProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	if err := os.WriteFile(path, []byte(opencodeFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	src, probed := fakeVia([]string{"claude-a"}, []string{"gpt-a", "kimi"}, "kimi")

	res, err := writeOpencodeConfig(path, 7331, *src, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Total != 3 || res.Backup != "" {
		t.Errorf("result = %+v", res)
	}
	raw, _ := os.ReadFile(path)
	for _, keep := range []string{"// These bill straight to the vendor.", "// llama-swap on the LAN", `"plugins": ["opencode-brave-websearch"]`} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("lost %q", keep)
		}
	}
	doc := readOpencode(t, path)
	if dig(doc, "provider", "gaia", "models", "qwen", "name") != "Qwen" {
		t.Error("an unrelated provider changed")
	}
	for id, want := range map[string]struct{ npm, model string }{
		"prism-anthropic":   {"@ai-sdk/anthropic", "claude-a"},
		"prism-openai":      {"@ai-sdk/openai", "gpt-a"},
		"prism-openai-chat": {"@ai-sdk/openai-compatible", "kimi"},
	} {
		if got := dig(doc, "provider", id, "npm"); got != want.npm {
			t.Errorf("%s npm = %v, want %s", id, got, want.npm)
		}
		if got := dig(doc, "provider", id, "options", "baseURL"); got != "http://127.0.0.1:7331/v1" {
			t.Errorf("%s baseURL = %v", id, got)
		}
		if got := dig(doc, "provider", id, "options", "apiKey"); got != "teleport" {
			t.Errorf("%s apiKey = %v", id, got)
		}
		if got := dig(doc, "provider", id, "models", want.model, "name"); got != want.model+" via prism" {
			t.Errorf("%s model name = %v", id, got)
		}
	}

	// Again, on a new port and with a user's edit to an entry: nothing is
	// probed again, the edit stays, and only the URLs move.
	edited, _ := jsonc.Set(raw, []string{"provider", "prism-openai-chat", "models", "kimi", "limit"}, map[string]any{"context": 262144})
	_ = os.WriteFile(path, edited, 0o600)
	*probed = nil
	res, err = writeOpencodeConfig(path, 7400, *src, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(*probed) != 0 {
		t.Errorf("re-probed %v", *probed)
	}
	doc = readOpencode(t, path)
	if dig(doc, "provider", "prism-openai-chat", "models", "kimi", "limit", "context") != float64(262144) {
		t.Error("user's edit to an entry was lost")
	}
	if got := dig(doc, "provider", "prism-anthropic", "options", "baseURL"); got != "http://127.0.0.1:7400/v1" {
		t.Errorf("baseURL after port change = %v", got)
	}

	// And with nothing to change, nothing is written.
	if res, err = writeOpencodeConfig(path, 7400, *src, false); err != nil || res.Changed {
		t.Errorf("idempotent run: changed=%v err=%v", res.Changed, err)
	}
}

func TestWriteOpencodeConfigOverwriteKeepsOtherSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.jsonc")
	if err := os.WriteFile(path, []byte(opencodeFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	src, _ := fakeVia([]string{"claude-a"}, nil)
	res, err := writeOpencodeConfig(path, 7331, *src, true)
	if err != nil {
		t.Fatal(err)
	}
	if bak, _ := os.ReadFile(res.Backup); string(bak) != opencodeFixture {
		t.Errorf("backup %q doesn't hold the original", res.Backup)
	}
	doc := readOpencode(t, path)
	providers := dig(doc, "provider").(map[string]any)
	if len(providers) != 1 || providers["prism-anthropic"] == nil {
		t.Errorf("providers = %v, want prism-anthropic only", providers)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "// These bill straight to the vendor.") || dig(doc, "plugins") == nil {
		t.Error("--overwrite touched settings outside the provider block")
	}
}

func TestWriteOpencodeConfigCreatesFileAndWarns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode", "opencode.json")
	src, _ := fakeVia([]string{"claude-a"}, nil)
	res, err := writeOpencodeConfig(path, 7331, *src, false)
	if err != nil || !res.Changed {
		t.Fatalf("changed=%v err=%v", res.Changed, err)
	}
	if dig(readOpencode(t, path), "$schema") == nil {
		t.Error("a new file should carry OpenCode's $schema")
	}

	_ = os.WriteFile(path, []byte(`{"disabled_providers": ["prism-anthropic"]}`), 0o600)
	res, _ = writeOpencodeConfig(path, 7331, *src, false)
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "disabled_providers") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestOpencodeConfigPathPrefersJSONC(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if p, _ := opencodeConfigPath(); p != filepath.Join(dir, "opencode", "opencode.json") {
		t.Errorf("with no file, path = %s", p)
	}
	_ = os.MkdirAll(filepath.Join(dir, "opencode"), 0o755)
	for _, name := range []string{"config.json", "opencode.json", "opencode.jsonc"} {
		_ = os.WriteFile(filepath.Join(dir, "opencode", name), []byte("{}"), 0o600)
		if p, _ := opencodeConfigPath(); p != filepath.Join(dir, "opencode", name) {
			t.Errorf("after creating %s, path = %s", name, p)
		}
	}
}

func TestWritePiModelsConfigAddsViaPrismProviders(t *testing.T) {
	piDir := t.TempDir()
	writePiStoreFixture(t, piDir)
	writeJSONFixture(t, filepath.Join(piDir, "models.json"), map[string]any{
		"providers": map[string]any{"llama-swap": map[string]any{"baseUrl": "http://x/v1", "models": []any{}}},
	})
	src, probed := fakeVia([]string{"anthropic-model", "claude-new"}, []string{"kimi"}, "kimi")

	res, err := writePiModelsConfig(piDir, 7331, "", "", src, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.ViaPrism != 3 || res.Hijacked[piAnthropic] != 1 {
		t.Errorf("result = %+v", res)
	}
	cfg, _ := loadPiConfig(res.Path)
	providers := cfg["providers"].(map[string]any)
	for _, keep := range []string{"llama-swap", piAnthropic, piOpenAI} {
		if providers[keep] == nil {
			t.Errorf("lost provider %s", keep)
		}
	}
	anth := providers["prism-anthropic"].(map[string]any)
	if anth["baseUrl"] != "http://127.0.0.1:7331" || anth["api"] != "anthropic-messages" || anth["apiKey"] != "teleport" {
		t.Errorf("prism-anthropic = %v", anth)
	}
	models := anth["models"].([]any)
	first := models[0].(map[string]any)
	if first["id"] != "anthropic-model" || first["name"] != "anthropic-model via prism" {
		t.Errorf("first model = %v", first)
	}
	if _, ok := first["baseUrl"]; ok {
		t.Error("a via-prism entry kept the registry's baseUrl, which would bypass prism")
	}
	chat := providers["prism-openai-chat"].(map[string]any)
	if chat["api"] != "openai-completions" || chat["baseUrl"] != "http://127.0.0.1:7331/v1" {
		t.Errorf("prism-openai-chat = %v", chat)
	}

	*probed = nil
	if _, err := writePiModelsConfig(piDir, 7331, "", "", src, false); err != nil {
		t.Fatal(err)
	}
	if len(*probed) != 0 {
		t.Errorf("second run re-probed %v", *probed)
	}

	res, err = writePiModelsConfig(piDir, 7331, "", "", src, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Backup == "" {
		t.Error("--overwrite made no backup")
	}
	cfg, _ = loadPiConfig(res.Path)
	providers = cfg["providers"].(map[string]any)
	if len(providers) != 2 || providers["prism-anthropic"] == nil || providers["prism-openai-chat"] == nil {
		t.Errorf("after --overwrite, providers = %v", providers)
	}
}

// Pi's registry marks models supportsMidConvoEffort, which makes it send
// configuration_update items the gateway rejects; prism's entries turn it
// off without touching the registry's own copy.
func TestPiForGatewayDropsMidConvoEffort(t *testing.T) {
	compat := map[string]any{"supportsMidConvoEffort": true, "supportsStrictMode": true}
	store := map[string]piStoreProvider{piOpenAI: {Models: []map[string]any{{"id": "gpt", "compat": compat}}}}
	for name, entry := range map[string]map[string]any{
		"via prism": piViaPrismModel(store, routeResponses, "gpt"),
		"override":  piOverrides(store[piOpenAI], "", "http://127.0.0.1:7331/v1")[0],
	} {
		got := entry["compat"].(map[string]any)
		if got["supportsMidConvoEffort"] != false || got["supportsStrictMode"] != true {
			t.Errorf("%s: compat = %v", name, got)
		}
	}
	if compat["supportsMidConvoEffort"] != true {
		t.Error("the registry's own compat map was modified")
	}
}
