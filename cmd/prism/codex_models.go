package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/tshwrap"
)

type codexModelCatalog struct {
	Models []map[string]any `json:"models"`
}

// prepareCodexModelCatalog writes the catalog `prism codex` hands Codex,
// returning its path, or "" to leave Codex its own.
//
// On by default, so it fails open: whatever goes wrong — no tbot identity
// or login to list with, a tsh too old to show models, Codex not
// answering, nothing matching — costs the user one warning line and a
// picker that offers models the gateway may refuse, never a Codex that
// won't start. That picker is what every launch got before discovery
// existed.
func prepareCodexModelCatalog(cfg *config.Config, bin string, port int, env []string, warn io.Writer) string {
	if !cfg.ModelDiscoveryEnabled() {
		return ""
	}
	path, err := buildCodexModelCatalog(cfg, bin, port, env)
	if err != nil {
		fmt.Fprintf(warn, "prism: %v\n"+
			"       Codex keeps its own model list; `prism config set model_discovery false` stops trying\n", err)
		return ""
	}
	return path
}

// buildCodexModelCatalog does the work, reporting exactly what failed.
func buildCodexModelCatalog(cfg *config.Config, bin string, port int, env []string) (string, error) {
	// The daemon's cached list, not a live query: listing apps takes about
	// a second, and up to its timeout when the cluster is unreachable.
	apps, err := cachedApps(cfg)
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
	catalog, err := loadCodexModelCatalog(bin, port, env)
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
	// The OS temp dir rather than prism's config dir: the file is removed
	// when Codex exits, but a killed prism never gets to, and the OS cleans
	// its temp dir where nothing would ever clean ~/.config/prism.
	return writeCodexModelCatalog(os.TempDir(), append(data, '\n'))
}

func codexAppInference(apps []tshwrap.App) (*tshwrap.Inference, error) {
	for _, app := range apps {
		if app.Metadata.Name != router.OpenAIAppName {
			continue
		}
		if app.Spec.Inference == nil {
			// tsh drops fields it doesn't know, so an older tsh shows an LLM
			// app with no inference block at all. The field reached v18 in
			// 18.8.3 (gravitational/teleport#66909).
			return nil, fmt.Errorf("codex model discovery: `tsh apps ls` shows no inference configuration for app %q — "+
				"tsh before 18.8.3 doesn't report it, so check `tsh version`", router.OpenAIAppName)
		}
		if app.Spec.Inference.Format != "openai" {
			return nil, fmt.Errorf("codex model discovery: app %q serves format %q, not openai", router.OpenAIAppName, app.Spec.Inference.Format)
		}
		return app.Spec.Inference, nil
	}
	return nil, fmt.Errorf("codex model discovery: app %q is not visible in `tsh apps ls`", router.OpenAIAppName)
}

// loadCodexModelCatalog asks Codex for the catalog it would use itself.
//
// `codex debug models` applies Codex's own rule: its models_cache.json
// when this Codex version wrote it, the catalog bundled with the binary
// otherwise. Reading the cache file directly skips that check, and a cache
// left by an older Codex lists models the gateway no longer serves —
// enough to match nothing and refuse every launch. prism's provider is
// passed so that a refresh, if Codex attempts one, goes through the router
// rather than to api.openai.com with prism's dummy key.
func loadCodexModelCatalog(bin string, port int, env []string) (codexModelCatalog, error) {
	// It answers in well under a second; on by default, a hung Codex must
	// not hold up the launch for long before it falls back.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "debug", "models",
		"-c", codexProviderOverride(port), "-c", "model_provider="+codexProvider)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return codexModelCatalog{}, fmt.Errorf("read Codex model catalog: %w (%s)", err, bytes.TrimSpace(stderr.Bytes()))
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
		entry := make(map[string]any, len(model))
		for key, value := range model {
			entry[key] = value
		}
		entry["slug"] = name
		out = append(out, entry)
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
