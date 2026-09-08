package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/webvictim/prism/internal/state"
)

// Pi resolves a model's base URL from its own registry
// (~/.pi/agent/models-store.json), with per-id overrides in models.json.
// Overrides match *by id*, so an entry whose id Pi doesn't already know is
// simply an extra model nobody selects — it intercepts nothing.
//
// So `prism pi config` mirrors Pi's registry and rewrites only baseUrl.
// The model ids come from Pi at runtime, which keeps model names out of
// prism while preserving each entry's cost/context/compat metadata.
func cmdPiConfig(args []string) error {
	fs := flag.NewFlagSet("pi config", flag.ExitOnError)
	anthropicModel := fs.String("anthropic-model", "", "only route this Anthropic model id through prism (default: all of them)")
	openaiModel := fs.String("openai-model", "", "only route this OpenAI model id through prism (default: all of them)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	s, _ := state.Load()
	port := defaultLocalPort
	if s != nil && s.LocalPort != 0 {
		port = s.LocalPort
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	piDir, err := piModelsDir()
	if err != nil {
		return err
	}

	store, err := loadPiStore(filepath.Join(piDir, "models-store.json"))
	if err != nil {
		return err
	}

	only := map[string]string{
		piAnthropic: *anthropicModel,
		piOpenAI:    *openaiModel,
	}

	providers := map[string]any{}
	counts := map[string]int{}
	for _, provider := range []string{piAnthropic, piOpenAI} {
		models := piOverrides(store[provider], only[provider], piBaseURL(provider, base))
		if len(models) == 0 {
			continue
		}
		providers[provider] = map[string]any{
			// Pi hides models when a provider has no API key. The router
			// strips this dummy token — the tunnel uses mTLS.
			"apiKey": "teleport",
			"models": models,
		}
		counts[provider] = len(models)
	}
	if len(providers) == 0 {
		return fmt.Errorf("pi config: no models to write (checked %s)", filepath.Join(piDir, "models-store.json"))
	}

	data, err := json.MarshalIndent(map[string]any{"providers": providers}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return err
	}
	modelsPath := filepath.Join(piDir, "models.json")
	if err := os.WriteFile(modelsPath, append(data, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "prism: wrote %s\n", modelsPath)
	for _, provider := range sortedKeys(counts) {
		fmt.Fprintf(os.Stderr, "prism: %d %s model(s) now route through prism on 127.0.0.1:%d\n",
			counts[provider], provider, port)
	}
	fmt.Fprintln(os.Stderr, "prism: re-run after `pi update` refreshes Pi's model catalog")
	return nil
}

// Pi's provider keys. Only these two are tunnelled; any other provider in
// Pi's registry is left pointing at its own endpoint.
const (
	piAnthropic = "anthropic"
	piOpenAI    = "openai"
)

// piStoreProvider is one provider's slice of Pi's model registry. Models
// stay as raw maps so every field survives the round trip.
type piStoreProvider struct {
	Models []map[string]any `json:"models"`
}

func loadPiStore(path string) (map[string]piStoreProvider, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("pi config: no Pi model catalog at %s — run `pi` once (or `pi update`) to populate it", path)
		}
		return nil, err
	}
	var store map[string]piStoreProvider
	if err := json.Unmarshal(b, &store); err != nil {
		return nil, fmt.Errorf("pi config: parse %s: %w", path, err)
	}
	return store, nil
}

// piBaseURL is the local router URL for a provider. Anthropic clients hit
// the router root; OpenAI clients expect the /v1 suffix.
func piBaseURL(provider, base string) string {
	if provider == piOpenAI {
		return base + "/v1"
	}
	return base
}

// piOverrides copies each registry model with baseUrl repointed at prism.
// When only is set, just that id is overridden — and if Pi's catalog
// doesn't have it, a minimal entry is synthesised so it still shows up.
func piOverrides(p piStoreProvider, only, baseURL string) []map[string]any {
	var out []map[string]any
	for _, m := range p.Models {
		id, _ := m["id"].(string)
		if only != "" && id != only {
			continue
		}
		cp := make(map[string]any, len(m))
		for k, v := range m {
			cp[k] = v
		}
		cp["baseUrl"] = baseURL
		out = append(out, cp)
	}
	if only != "" && len(out) == 0 {
		out = append(out, map[string]any{
			"id":       only,
			"name":     only + " (via prism)",
			"api":      piAPIForBase(baseURL),
			"provider": piProviderForBase(baseURL),
			"baseUrl":  baseURL,
			"input":    []string{"text"},
		})
	}
	return out
}

func piAPIForBase(baseURL string) string {
	if len(baseURL) >= 3 && baseURL[len(baseURL)-3:] == "/v1" {
		return "openai-responses"
	}
	return "anthropic-messages"
}

func piProviderForBase(baseURL string) string {
	if len(baseURL) >= 3 && baseURL[len(baseURL)-3:] == "/v1" {
		return piOpenAI
	}
	return piAnthropic
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func piModelsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}
