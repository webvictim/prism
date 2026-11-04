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
	} else if !strings.Contains(err.Error(), "18.8.3") {
		// An older tsh drops the field; the error must point there, not at
		// the cluster's configuration.
		t.Errorf("missing inference should name the tsh version that reports it: %v", err)
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

// fakeCodex writes a stand-in codex that prints catalog for
// `debug models` with prism's provider overrides, and fails otherwise.
func fakeCodex(t *testing.T, dir, catalog string) string {
	t.Helper()
	bin := filepath.Join(dir, "codex-fixture")
	script := `#!/bin/sh
[ "$1 $2" = "debug models" ] || exit 1
case "$*" in *"model_providers.prism="*"model_provider=prism"*) ;; *) exit 2 ;; esac
printf '%s' '` + catalog + `'
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin
}

// The catalog is the one Codex itself reports, never models_cache.json
// read directly. A cache left by an older Codex lists models the gateway
// no longer serves; Codex ignores it, and prism used to match against it
// and refuse every launch.
func TestLoadCodexModelCatalogAsksCodex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell fixtures")
	}
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	stale := `{"client_version":"0.117.0","models":[{"slug":"gpt-5.4"}]}`
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := fakeCodex(t, dir, `{"models":[{"slug":"known","context_window":12345}]}`)
	catalog, err := loadCodexModelCatalog(bin, 7331, os.Environ())
	if err != nil || len(catalog.Models) != 1 || catalog.Models[0]["slug"] != "known" {
		t.Fatalf("catalog = %+v, %v; want Codex's own, not the stale cache", catalog, err)
	}

	empty := fakeCodex(t, t.TempDir(), `{"models":[]}`)
	if _, err := loadCodexModelCatalog(empty, 7331, os.Environ()); err == nil {
		t.Fatal("accepted an empty catalog")
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
	codexBin := fakeCodex(t, dir, `{"models":[{"slug":"first","context_window":12345},{"slug":"second","context_window":67890}]}`)
	for _, name := range []string{"first", "second"} {
		// A live login: prism only lists apps as the login when tsh status
		// shows one, since otherwise tsh would start an interactive login.
		script := `#!/bin/sh
case "$*" in
"--proxy=selected.example.com status --format=json")
  printf '%s' '{"active":{"cluster":"c","valid_until":"2999-01-01T00:00:00Z"}}' ;;
"--proxy=selected.example.com apps ls --format=json")
  printf '%s' '[{"metadata":{"name":"openai"},"spec":{"inference":{"format":"openai","models":[{"name":"` + name + `"}]}}}]' ;;
*) exit 1 ;;
esac
`
		if err := os.WriteFile(filepath.Join(dir, "tsh"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		path, err := prepareCodexModelCatalog(cfg, codexBin, 7331, os.Environ())
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
	// Codex knows none of the models the app serves.
	unmatched := fakeCodex(t, t.TempDir(), `{"models":[{"slug":"elsewhere"}]}`)
	if _, err := prepareCodexModelCatalog(cfg, unmatched, 7331, os.Environ()); err == nil || !strings.Contains(err.Error(), "prism config set codex_model_discovery false") {
		t.Fatalf("metadata error does not advertise the disable command: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tsh"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareCodexModelCatalog(cfg, codexBin, 7331, os.Environ()); err == nil {
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
		if path, err := prepareCodexModelCatalog(defaultConfig, "missing-codex", 7331, nil); path != "" || err != nil {
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
		if path, err := prepareCodexModelCatalog(cfg, "missing-codex", 7331, nil); path != "" || err != nil {
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
		if _, err := prepareCodexModelCatalog(cfg, "missing-codex", 7331, nil); err == nil || !strings.Contains(err.Error(), "prism config set codex_model_discovery false") {
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
	if path, err := prepareCodexModelCatalog(cfg, "missing-codex", 7331, nil); path != "" || err != nil {
		t.Fatalf("unset discovery queried tsh or loaded metadata: %q, %v", path, err)
	}
}
