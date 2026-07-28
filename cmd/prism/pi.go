package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gravitational/prism/internal/state"
)

func cmdPiConfig(_ []string) error {
	s, _ := state.Load()
	port := defaultLocalPort
	if s != nil && s.LocalPort != 0 {
		port = s.LocalPort
	}

	piDir, err := piModelsDir()
	if err != nil {
		return err
	}
	modelsPath := filepath.Join(piDir, "models.json")

	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	entry := map[string]any{
		"providers": map[string]any{
			"anthropic": map[string]any{
				"apiKey": "teleport",
				"models": []map[string]any{
					{
						"id":        "claude-opus-4-6",
						"name":      "Claude Opus 4.6 (via prism)",
						"api":       "anthropic-messages",
						"provider":  "anthropic",
						"baseUrl":   base,
						"reasoning": true,
						"input":     []string{"text", "image"},
						"cost": map[string]any{
							"input":      5,
							"output":     25,
							"cacheRead":  0.5,
							"cacheWrite": 6.25,
						},
						"contextWindow": 1000000,
						"maxTokens":     128000,
						"thinkingLevelMap": map[string]any{
							"max": "max",
						},
						"compat": map[string]any{
							"forceAdaptiveThinking": true,
							"supportsStrictTools":   true,
						},
					},
				},
			},
			"openai": map[string]any{
				"apiKey": "teleport",
				"models": []map[string]any{
					{
						"id":        "gpt-4o",
						"name":      "GPT-4o (via prism)",
						"api":       "openai-chat",
						"provider":  "openai",
						"baseUrl":   base + "/v1",
						"reasoning": false,
						"input":     []string{"text", "image"},
						"cost": map[string]any{
							"input":      2.5,
							"output":     10,
							"cacheRead":  1.25,
							"cacheWrite": 0,
						},
						"contextWindow": 128000,
						"maxTokens":     16384,
						"compat": map[string]any{
							"supportsStrictMode": true,
						},
					},
					{
						"id":        "gpt-5.5",
						"name":      "GPT-5.5 (via prism)",
						"api":       "openai-chat",
						"provider":  "openai",
						"baseUrl":   base + "/v1",
						"reasoning": true,
						"input":     []string{"text", "image"},
						"cost": map[string]any{
							"input":      5,
							"output":     30,
							"cacheRead":  0.5,
							"cacheWrite": 0,
						},
						"contextWindow": 272000,
						"maxTokens":     128000,
						"thinkingLevelMap": map[string]any{
							"off":    "none",
							"low":    "low",
							"medium": "medium",
							"high":   "high",
							"xhigh":  "xhigh",
						},
						"compat": map[string]any{
							"supportsStrictMode":         true,
							"supportsOpenAIGrammarTools": true,
							"supportsToolSearch":         true,
						},
					},
				},
			},
		},
	}

	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(modelsPath, append(data, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "prism: wrote %s\n", modelsPath)
	fmt.Fprintf(os.Stderr, "prism: Pi models (claude-opus-4-6, gpt-4o, gpt-5.5) will now route through prism on 127.0.0.1:%d\n", port)
	return nil
}

func piModelsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}
