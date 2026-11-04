package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/tshwrap"
)

func TestCodexAppInference(t *testing.T) {
	var apps []tshwrap.App
	if err := json.Unmarshal([]byte(`[
  {"metadata":{"name":"anthropic"},"spec":{"inference":{"format":"anthropic","models":[{"name":"other"}]}}},
  {"metadata":{"name":"openai"},"spec":{"inference":{"format":"openai","models":[{"name":"alias","provider_name":"provider.known"}]}}}
]`), &apps); err != nil {
		t.Fatal(err)
	}
	inference, err := codexAppInference(apps)
	if err != nil {
		t.Fatal(err)
	}
	want := []tshwrap.InferenceModel{{Name: "alias", ProviderName: "provider.known"}}
	if !reflect.DeepEqual(inference.Models, want) {
		t.Fatalf("inference models = %+v, want %+v", inference.Models, want)
	}
	if _, err := codexAppInference(apps[:1]); err == nil {
		t.Fatal("accepted a list without the OpenAI app")
	}
	apps[1].Spec.Inference = nil
	if _, err := codexAppInference(apps); err == nil {
		t.Fatal("accepted an app without inference metadata")
	}
}

func TestFilterCodexModels(t *testing.T) {
	catalog := []map[string]any{
		{"slug": "known", "context_window": 12345, "supported_reasoning_levels": []string{"medium", "high"}},
		{"slug": "unavailable"},
		{"slug": "other"},
	}
	for _, tt := range []struct {
		name   string
		models []tshwrap.InferenceModel
		slug   string
	}{
		{
			name: "prefer accepted catalog name over provider alias",
			models: []tshwrap.InferenceModel{
				{Name: "provider.known"},
				{Name: "known", ProviderName: "provider.known"},
				{Name: "unknown"},
			},
			slug: "known",
		},
		{
			name:   "provider-prefixed name only",
			models: []tshwrap.InferenceModel{{Name: "provider.known"}},
			slug:   "provider.known",
		},
		{
			name:   "custom alias is the request name",
			models: []tshwrap.InferenceModel{{Name: "fast", ProviderName: "provider.known"}},
			slug:   "fast",
		},
		{
			name:   "gateway names are case insensitive",
			models: []tshwrap.InferenceModel{{Name: "KNOWN"}},
			slug:   "KNOWN",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := filterCodexModels(catalog, tt.models)
			if len(got) != 1 || got[0]["slug"] != tt.slug {
				t.Fatalf("filtered catalog = %+v, want only %q", got, tt.slug)
			}
			if got[0]["context_window"] != 12345 || !reflect.DeepEqual(got[0]["supported_reasoning_levels"], catalog[0]["supported_reasoning_levels"]) {
				t.Fatalf("capability metadata changed: %+v", got[0])
			}
			if catalog[0]["slug"] != "known" {
				t.Fatal("modified source catalog")
			}
		})
	}
	if got := filterCodexModels(catalog, []tshwrap.InferenceModel{{Name: "unknown"}}); len(got) != 0 {
		t.Fatalf("unknown model produced metadata: %+v", got)
	}
}

func TestLoadCodexModelCatalogUsesFullCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	data := []byte(`{"models":[{"slug":"known","context_window":12345}],"client_version":"test"}`)
	path := filepath.Join(dir, "models_cache.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := loadCodexModelCatalog("not-a-codex-binary", nil)
	if err != nil || len(catalog.Models) != 1 || catalog.Models[0]["slug"] != "known" {
		t.Fatalf("cache load = %+v, %v", catalog, err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || string(unchanged) != string(data) {
		t.Fatalf("source cache changed: %s, %v", unchanged, err)
	}
	if err := os.WriteFile(path, []byte(`{"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCodexModelCatalog("not-a-codex-binary", nil); err == nil {
		t.Fatal("accepted an empty cache")
	}
}

func TestWriteCodexModelCatalogIsolatesLaunches(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prism")
	first, err := writeCodexModelCatalog(dir, []byte(`{"models":[{"slug":"first"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := writeCodexModelCatalog(dir, []byte(`{"models":[{"slug":"second"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("concurrent launches share a catalog path")
	}
	data, err := os.ReadFile(first)
	if err != nil || string(data) != `{"models":[{"slug":"first"}]}` {
		t.Fatalf("first catalog overwritten: %s, %v", data, err)
	}
}

func TestPrepareCodexModelCatalogRefreshesAppModels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell fixtures")
	}
	enabled := true
	cfg := &config.Config{CodexModelDiscovery: &enabled}
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	previousProxy := tshwrap.Proxy()
	tshwrap.SetProxy("selected.example.com")
	t.Cleanup(func() { tshwrap.SetProxy(previousProxy) })
	codexBin := filepath.Join(dir, "codex-fixture")
	script := `#!/bin/sh
if [ "$*" != "debug models --bundled" ]; then exit 1; fi
printf '%s' '{"models":[{"slug":"first","context_window":12345},{"slug":"second","context_window":67890}]}'
`
	if err := os.WriteFile(codexBin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "second"} {
		script := `#!/bin/sh
if [ "$*" != "--proxy=selected.example.com apps ls --format=json" ]; then exit 1; fi
printf '%s' '[{"metadata":{"name":"openai"},"spec":{"inference":{"format":"openai","models":[{"name":"` + name + `"}]}}}]'
`
		if err := os.WriteFile(filepath.Join(dir, "tsh"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		path, err := prepareCodexModelCatalog(cfg, codexBin, os.Environ())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(path) })
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var catalog codexModelCatalog
		if err := json.Unmarshal(data, &catalog); err != nil {
			t.Fatal(err)
		}
		if len(catalog.Models) != 1 || catalog.Models[0]["slug"] != name {
			t.Fatalf("launch catalog = %+v, want only %q", catalog, name)
		}
	}
	cacheDir := os.Getenv("CODEX_HOME")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "models_cache.json"), []byte(`{"models":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareCodexModelCatalog(cfg, codexBin, os.Environ()); err == nil || !strings.Contains(err.Error(), "prism config set codex_model_discovery false") {
		t.Fatalf("metadata error does not advertise the disable command: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsh"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareCodexModelCatalog(cfg, codexBin, os.Environ()); err == nil {
		t.Fatal("failed discovery silently used the full catalog")
	} else if !strings.Contains(err.Error(), "prism config set codex_model_discovery false") {
		t.Fatalf("discovery error does not advertise the disable command: %v", err)
	}
}

func TestCodexModelDiscoveryConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	cfg, err := config.Load()
	if err != nil || cfg.CodexModelDiscoveryEnabled() {
		t.Fatalf("missing config must disable discovery: %+v, %v", cfg, err)
	}
	for _, defaultConfig := range []*config.Config{nil, cfg} {
		if path, err := prepareCodexModelCatalog(defaultConfig, "missing-codex", nil); path != "" || err != nil {
			t.Fatalf("default discovery queried tsh or loaded metadata: %q, %v", path, err)
		}
	}
	for _, value := range []string{"false", "0", "off"} {
		if err := configSet("codex_model_discovery", value); err != nil {
			t.Fatal(err)
		}
		cfg, err = config.Load()
		if err != nil || cfg.CodexModelDiscoveryEnabled() {
			t.Fatalf("config value %q did not disable discovery: %+v, %v", value, cfg, err)
		}
		if path, err := prepareCodexModelCatalog(cfg, "missing-codex", nil); path != "" || err != nil {
			t.Fatalf("disabled discovery queried tsh or loaded metadata: %q, %v", path, err)
		}
	}
	for _, value := range []string{"true", "1", "on"} {
		if err := configSet("codex_model_discovery", value); err != nil {
			t.Fatal(err)
		}
		cfg, err = config.Load()
		if err != nil || !cfg.CodexModelDiscoveryEnabled() {
			t.Fatalf("config value %q did not enable discovery: %+v, %v", value, cfg, err)
		}
		if _, err := prepareCodexModelCatalog(cfg, "missing-codex", nil); err == nil || !strings.Contains(err.Error(), "prism config set codex_model_discovery false") {
			t.Fatalf("enabled discovery did not report tsh failure with disable command: %v", err)
		}
	}
	if err := configSet("codex_model_discovery", "invalid"); err == nil {
		t.Fatal("accepted an invalid boolean")
	}
	if err := configUnset("codex_model_discovery"); err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Load()
	if err != nil || cfg.CodexModelDiscovery != nil || cfg.CodexModelDiscoveryEnabled() {
		t.Fatalf("unset did not restore disabled default: %+v, %v", cfg, err)
	}
	if path, err := prepareCodexModelCatalog(cfg, "missing-codex", nil); path != "" || err != nil {
		t.Fatalf("unset discovery queried tsh or loaded metadata: %q, %v", path, err)
	}
}
