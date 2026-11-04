package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/tshwrap"
)

type codexModelCatalog struct {
	Models []map[string]any `json:"models"`
}

func prepareCodexModelCatalog(cfg *config.Config, bin string, env []string) (path string, err error) {
	if !cfg.CodexModelDiscoveryEnabled() {
		return "", nil
	}
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w; disable model discovery with `prism config set codex_model_discovery false`", err)
		}
	}()
	apps, err := tshwrap.AppsList()
	if err != nil {
		return "", fmt.Errorf("codex model discovery: %w", err)
	}
	inference, err := codexAppInference(apps)
	if err != nil {
		return "", err
	}
	// An empty model list lets the gateway pass every requested name through.
	if len(inference.Models) == 0 {
		return "", nil
	}
	catalog, err := loadCodexModelCatalog(bin, env)
	if err != nil {
		return "", err
	}
	catalog.Models = filterCodexModels(catalog.Models, inference.Models)
	if len(catalog.Models) == 0 {
		return "", fmt.Errorf("codex model discovery: app %q has no models with Codex metadata", router.OpenAIAppName)
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return "", err
	}
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return writeCodexModelCatalog(dir, append(data, '\n'))
}

func codexAppInference(apps []tshwrap.App) (*tshwrap.Inference, error) {
	for _, app := range apps {
		if app.Metadata.Name != router.OpenAIAppName {
			continue
		}
		if app.Spec.Inference == nil || app.Spec.Inference.Format != "openai" {
			return nil, fmt.Errorf("codex model discovery: app %q has no OpenAI inference configuration", router.OpenAIAppName)
		}
		return app.Spec.Inference, nil
	}
	return nil, fmt.Errorf("codex model discovery: app %q is not visible in `tsh apps ls`", router.OpenAIAppName)
}

// loadCodexModelCatalog reads models_cache.json, falling back to Codex's
// bundled catalog when the cache file is missing.
func loadCodexModelCatalog(bin string, env []string) (codexModelCatalog, error) {
	dir := os.Getenv("CODEX_HOME")
	var err error
	if dir == "" {
		dir, err = os.UserHomeDir()
		if err != nil {
			return codexModelCatalog{}, err
		}
		dir = filepath.Join(dir, ".codex")
	} else {
		dir, err = expandHome(dir)
		if err != nil {
			return codexModelCatalog{}, err
		}
	}
	path := filepath.Join(dir, "models_cache.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "debug", "models", "--bundled")
		cmd.Env = env
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		data, err = cmd.Output()
		if err != nil {
			return codexModelCatalog{}, fmt.Errorf("read Codex bundled model catalog: %w (%s)", err, bytes.TrimSpace(stderr.Bytes()))
		}
	} else if err != nil {
		return codexModelCatalog{}, fmt.Errorf("read Codex model cache: %w", err)
	}
	var catalog codexModelCatalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return catalog, fmt.Errorf("parse Codex model catalog: %w", err)
	}
	if len(catalog.Models) == 0 {
		return catalog, errors.New("Codex model catalog contains no models")
	}
	return catalog, nil
}

// filterCodexModels copies catalog entries matching the app's advertised
// models, preserving capability metadata and using accepted app names as slugs.
func filterCodexModels(catalog []map[string]any, models []tshwrap.InferenceModel) []map[string]any {
	var out []map[string]any
	for _, model := range catalog {
		slug, _ := model["slug"].(string)
		if slug == "" {
			continue
		}
		name := ""
		for _, appModel := range models {
			if strings.EqualFold(appModel.Name, slug) {
				name = appModel.Name
				break
			}
		}
		if name == "" {
			for _, appModel := range models {
				if appModel.Name != "" && (codexModelNameMatches(appModel.ProviderName, slug) || codexModelNameMatches(appModel.Name, slug)) {
					name = appModel.Name
					break
				}
			}
		}
		if name == "" {
			continue
		}
		copy := make(map[string]any, len(model))
		for key, value := range model {
			copy[key] = value
		}
		copy["slug"] = name
		out = append(out, copy)
	}
	return out
}

func codexModelNameMatches(name, slug string) bool {
	return strings.EqualFold(name, slug) || strings.HasSuffix(strings.ToLower(name), "."+strings.ToLower(slug))
}

// writeCodexModelCatalog writes a uniquely named catalog file and returns
// its path.
func writeCodexModelCatalog(dir string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, "codex-models-*.json")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
