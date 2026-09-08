package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/webvictim/prism/internal/state"
)

// Placeholder model ids used when the caller doesn't name a model. The
// gateway doesn't recognise them and resolves unknown names to whatever
// it currently serves, so Pi gets a working entry without prism naming a
// model.
const (
	piDefaultAnthropicID = "prism-anthropic"
	piDefaultOpenAIID    = "prism-openai"
)

func cmdPiConfig(args []string) error {
	fs := flag.NewFlagSet("pi config", flag.ExitOnError)
	anthropicModel := fs.String("anthropic-model", "", "Anthropic model id to register (default: let the gateway choose)")
	openaiModel := fs.String("openai-model", "", "OpenAI model id to register (default: let the gateway choose)")
	if err := fs.Parse(args); err != nil {
		return err
	}

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

	anthropicID := *anthropicModel
	if anthropicID == "" {
		anthropicID = piDefaultAnthropicID
	}
	openaiID := *openaiModel
	if openaiID == "" {
		openaiID = piDefaultOpenAIID
	}

	entry := map[string]any{
		"providers": map[string]any{
			"anthropic": map[string]any{
				"apiKey": "teleport",
				"models": []map[string]any{
					{
						"id":        anthropicID,
						"name":      piModelName(*anthropicModel, "Anthropic"),
						"api":       "anthropic-messages",
						"provider":  "anthropic",
						"baseUrl":   base,
						"reasoning": true,
						"input":     []string{"text", "image"},
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
						"id":        openaiID,
						"name":      piModelName(*openaiModel, "OpenAI"),
						"api":       "openai-responses",
						"provider":  "openai",
						"baseUrl":   base + "/v1",
						"reasoning": true,
						"input":     []string{"text", "image"},
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
	fmt.Fprintf(os.Stderr, "prism: Pi models (%s, %s) will now route through prism on 127.0.0.1:%d\n",
		anthropicID, openaiID, port)
	if *anthropicModel == "" || *openaiModel == "" {
		fmt.Fprintln(os.Stderr, "prism: unnamed models resolve to whatever the gateway currently serves;")
		fmt.Fprintln(os.Stderr, "prism: pass --anthropic-model / --openai-model to pin specific ids")
	}
	return nil
}

// piModelName builds the label Pi shows in its model picker.
func piModelName(model, provider string) string {
	if model == "" {
		return provider + " via prism (gateway default)"
	}
	return model + " (via prism)"
}

func piModelsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}
