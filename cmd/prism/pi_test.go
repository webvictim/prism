package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestMergePiProvidersPreservesOtherProviders(t *testing.T) {
	config := map[string]any{
		"custom": "preserve me",
		"providers": map[string]any{
			"llama-swap": map[string]any{
				"baseUrl": "http://127.0.0.1:9292/v1",
			},
			"openai": map[string]any{
				"baseUrl": "https://api.openai.com/v1",
			},
		},
	}
	replacements := map[string]any{
		"openai": map[string]any{
			"baseUrl": "http://127.0.0.1:7331/v1",
		},
		"anthropic": map[string]any{
			"baseUrl": "http://127.0.0.1:7331",
		},
	}

	if err := mergePiProviders(config, replacements); err != nil {
		t.Fatalf("mergePiProviders: %v", err)
	}

	if config["custom"] != "preserve me" {
		t.Fatalf("merge dropped top-level custom value: %#v", config["custom"])
	}
	providers := config["providers"].(map[string]any)
	if _, ok := providers["llama-swap"]; !ok {
		t.Fatal("merge dropped unrelated llama-swap provider")
	}
	if got := providers["openai"].(map[string]any)["baseUrl"]; got != "http://127.0.0.1:7331/v1" {
		t.Fatalf("openai baseUrl = %v, want prism URL", got)
	}
	if _, ok := providers["anthropic"]; !ok {
		t.Fatal("merge did not add replacement provider")
	}
}

func TestLoadPiConfigMissingFile(t *testing.T) {
	config, err := loadPiConfig(filepath.Join(t.TempDir(), "models.json"))
	if err != nil {
		t.Fatalf("loadPiConfig: %v", err)
	}
	providers, ok := config["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers has type %T, want object", config["providers"])
	}
	if len(providers) != 0 {
		t.Fatalf("missing config has providers: %#v", providers)
	}
}

func TestLoadPiConfigRejectsMalformedProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"providers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPiConfig(path); err == nil {
		t.Fatal("loadPiConfig accepted non-object providers")
	}
}

func TestLoadPiStoreMissingIsSentinel(t *testing.T) {
	_, err := loadPiStore(filepath.Join(t.TempDir(), "models-store.json"))
	if !errors.Is(err, errPiStoreMissing) {
		t.Fatalf("loadPiStore error = %v, want errPiStoreMissing", err)
	}
}

func TestPreparePiForLaunchBootstrapsAndPreservesCustomProviders(t *testing.T) {
	piDir := t.TempDir()
	configPath := filepath.Join(piDir, "models.json")
	if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "llama-swap": {
      "baseUrl": "http://127.0.0.1:9292/v1",
      "api": "openai-completions",
      "apiKey": "local",
      "models": [{"id": "local-model"}]
    }
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	refreshes := 0
	err := preparePiForLaunch(piDir, 7444, func() error {
		refreshes++
		writePiStoreFixture(t, piDir)
		return nil
	})
	if err != nil {
		t.Fatalf("preparePiForLaunch: %v", err)
	}
	if refreshes != 1 {
		t.Fatalf("catalog refreshes = %d, want 1", refreshes)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Errorf("models.json mode = %o, want existing mode 600 preserved", got)
	}
	if leftovers, err := filepath.Glob(filepath.Join(piDir, ".models.json-*.tmp")); err != nil {
		t.Fatal(err)
	} else if len(leftovers) != 0 {
		t.Errorf("atomic write left temporary files: %v", leftovers)
	}

	config, err := loadPiConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	providers := config["providers"].(map[string]any)
	if _, ok := providers["llama-swap"]; !ok {
		t.Fatal("automatic Pi setup dropped unrelated llama-swap provider")
	}
	for provider, wantURL := range map[string]string{
		piAnthropic: "http://127.0.0.1:7444",
		piOpenAI:    "http://127.0.0.1:7444/v1",
	} {
		entry, ok := providers[provider].(map[string]any)
		if !ok {
			t.Fatalf("provider %q missing from generated config", provider)
		}
		if got := entry["apiKey"]; got != "teleport" {
			t.Errorf("%s apiKey = %v, want teleport", provider, got)
		}
		models, ok := entry["models"].([]any)
		if !ok || len(models) != 1 {
			t.Fatalf("%s models = %#v, want one model", provider, entry["models"])
		}
		model := models[0].(map[string]any)
		if got := model["baseUrl"]; got != wantURL {
			t.Errorf("%s baseUrl = %v, want %s", provider, got, wantURL)
		}
	}
}

func TestPreparePiForLaunchUsesExistingCatalog(t *testing.T) {
	piDir := t.TempDir()
	writePiStoreFixture(t, piDir)

	refreshes := 0
	if err := preparePiForLaunch(piDir, 7331, func() error {
		refreshes++
		return errors.New("unexpected refresh")
	}); err != nil {
		t.Fatalf("preparePiForLaunch: %v", err)
	}
	if refreshes != 0 {
		t.Fatalf("catalog refreshes = %d, want 0", refreshes)
	}
}

func TestPreparePiForLaunchRejectsIncompleteRefresh(t *testing.T) {
	piDir := t.TempDir()
	err := preparePiForLaunch(piDir, 7331, func() error {
		store := map[string]piStoreProvider{
			piOpenAI: {Models: []map[string]any{{"id": "openai-model"}}},
		}
		writeJSONFixture(t, filepath.Join(piDir, "models-store.json"), store)
		return nil
	})
	if err == nil {
		t.Fatal("preparePiForLaunch accepted a catalog without Anthropic models")
	}
}

func TestPiModelsDirHonorsEnvironmentOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "custom-agent-dir")
	t.Setenv("PI_CODING_AGENT_DIR", want)
	got, err := piModelsDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("piModelsDir = %q, want %q", got, want)
	}
}

func TestPiWindowsShellPath(t *testing.T) {
	for input, want := range map[string]string{
		`/c/Users/boris/.pi/agent`:         `C:\Users\boris\.pi\agent`,
		`/mnt/d/pi-agent`:                  `D:\pi-agent`,
		`/cygdrive/E/Users/boris/pi-agent`: `E:\Users\boris\pi-agent`,
		`/home/boris/.pi/agent`:            `/home/boris/.pi/agent`,
		`//server/share/pi-agent`:          `//server/share/pi-agent`,
	} {
		if got := piWindowsShellPath(input); got != want {
			t.Errorf("piWindowsShellPath(%q) = %q, want %q", input, got, want)
		}
	}
}

func writePiStoreFixture(t *testing.T, piDir string) {
	t.Helper()
	store := map[string]piStoreProvider{
		piAnthropic: {Models: []map[string]any{{
			"id":       "anthropic-model",
			"provider": piAnthropic,
			"api":      "anthropic-messages",
			"baseUrl":  "https://api.anthropic.com",
		}}},
		piOpenAI: {Models: []map[string]any{{
			"id":       "openai-model",
			"provider": piOpenAI,
			"api":      "openai-responses",
			"baseUrl":  "https://api.openai.com/v1",
		}}},
	}
	writeJSONFixture(t, filepath.Join(piDir, "models-store.json"), store)
}

func writeJSONFixture(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
