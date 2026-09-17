package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/webvictim/prism/internal/state"
)

// cmdPi preserves the existing `prism pi config` setup command. Every other
// invocation starts prism, refreshes Pi's model overrides, and launches Pi.
func cmdPi(args []string) error {
	if len(args) > 0 && args[0] == "config" {
		return cmdPiConfig(args[1:])
	}
	return runToolWithPrismSetup("pi", args, configurePiForLaunch)
}

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
	piDir, err := piModelsDir()
	if err != nil {
		return err
	}

	modelsPath, counts, err := writePiModelsConfig(piDir, port, *anthropicModel, *openaiModel)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "prism: wrote %s\n", modelsPath)
	for _, provider := range sortedKeys(counts) {
		fmt.Fprintf(os.Stderr, "prism: %d %s model(s) now route through prism on 127.0.0.1:%d\n",
			counts[provider], provider, port)
	}
	if *anthropicModel != "" || *openaiModel != "" {
		fmt.Fprintln(os.Stderr, "prism: launch with `prism exec pi` to keep this narrowed selection; `prism pi` restores all catalog models")
	} else {
		fmt.Fprintln(os.Stderr, "prism: `prism pi` refreshes this configuration automatically before launch")
	}
	return nil
}

// configurePiForLaunch prepares the same environment and model configuration
// that Pi will read, bootstrapping its catalog on a fresh installation.
func configurePiForLaunch(bin string, port int, env []string) error {
	piDir, err := piModelsDir()
	if err != nil {
		return err
	}
	return preparePiForLaunch(piDir, port, func() error {
		return refreshPiModelCatalog(bin, env)
	})
}

func preparePiForLaunch(piDir string, port int, refresh func() error) error {
	storePath := filepath.Join(piDir, "models-store.json")
	store, err := loadPiStore(storePath)
	if err != nil && !errors.Is(err, errPiStoreMissing) {
		return err
	}

	missing := missingPiStoreProviders(store)
	if errors.Is(err, errPiStoreMissing) || len(missing) > 0 {
		fmt.Fprintln(os.Stderr, "prism: Pi model catalog is missing or incomplete; running `pi update --models`…")
		if err := refresh(); err != nil {
			return err
		}
		store, err = loadPiStore(storePath)
		if err != nil {
			return err
		}
		missing = missingPiStoreProviders(store)
		if len(missing) > 0 {
			return fmt.Errorf("pi setup: `pi update --models` did not populate %s in %s", strings.Join(missing, " and "), storePath)
		}
	}

	_, _, err = writePiModelsConfig(piDir, port, "", "")
	return err
}

func refreshPiModelCatalog(bin string, env []string) error {
	cmd := exec.Command(bin, "update", "--models")
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run `pi update --models`: %w", err)
	}
	return nil
}

func missingPiStoreProviders(store map[string]piStoreProvider) []string {
	var missing []string
	for _, provider := range []string{piAnthropic, piOpenAI} {
		if len(store[provider].Models) == 0 {
			missing = append(missing, provider)
		}
	}
	return missing
}

func writePiModelsConfig(piDir string, port int, anthropicModel, openaiModel string) (string, map[string]int, error) {
	storePath := filepath.Join(piDir, "models-store.json")
	store, err := loadPiStore(storePath)
	if err != nil {
		return "", nil, err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	only := map[string]string{
		piAnthropic: anthropicModel,
		piOpenAI:    openaiModel,
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
		return "", nil, fmt.Errorf("pi config: no models to write (checked %s)", storePath)
	}

	configPath := filepath.Join(piDir, "models.json")
	configFile, err := loadPiConfig(configPath)
	if err != nil {
		return "", nil, err
	}
	if err := mergePiProviders(configFile, providers); err != nil {
		return "", nil, err
	}
	data, err := json.MarshalIndent(configFile, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return "", nil, err
	}
	if err := writePiConfigFile(configPath, append(data, '\n')); err != nil {
		return "", nil, err
	}
	return configPath, counts, nil
}

// writePiConfigFile replaces models.json atomically so an interrupted
// `prism pi` launch cannot leave Pi's complete model configuration truncated.
func writePiConfigFile(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".models.json-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
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

var errPiStoreMissing = errors.New("no Pi model catalog")

func loadPiConfig(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{
				"providers": map[string]any{},
			}, nil
		}
		return nil, err
	}

	var config map[string]any
	if err := json.Unmarshal(b, &config); err != nil {
		return nil, fmt.Errorf("pi config: parse %s: %w", path, err)
	}
	if config == nil {
		config = map[string]any{}
	}
	if providers, ok := config["providers"]; ok && providers != nil {
		if _, ok := providers.(map[string]any); !ok {
			return nil, fmt.Errorf("pi config: %s: providers must be an object", path)
		}
	} else {
		config["providers"] = map[string]any{}
	}
	return config, nil
}

func mergePiProviders(config map[string]any, replacements map[string]any) error {
	providers, ok := config["providers"].(map[string]any)
	if !ok {
		return errors.New("pi config: providers must be an object")
	}
	for provider, value := range replacements {
		providers[provider] = value
	}
	return nil
}

func loadPiStore(path string) (map[string]piStoreProvider, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("pi config: %w at %s — run `pi update --models` to populate it", errPiStoreMissing, path)
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
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		if runtime.GOOS == "windows" {
			dir = piWindowsShellPath(dir)
		}
		if dir != "~" && !strings.HasPrefix(dir, "~/") && !strings.HasPrefix(dir, `~\`) {
			return dir, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if dir == "~" {
			return home, nil
		}
		return filepath.Join(home, dir[2:]), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}

// piWindowsShellPath matches Pi's conversion of Git Bash, MSYS, Cygwin and
// WSL drive paths before it resolves PI_CODING_AGENT_DIR on Windows.
func piWindowsShellPath(path string) string {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") || strings.Contains(path, `\`) {
		return path
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	driveIndex := 0
	if len(parts) > 0 && (strings.EqualFold(parts[0], "mnt") || strings.EqualFold(parts[0], "cygdrive")) {
		driveIndex = 1
	}
	if len(parts) <= driveIndex || len(parts[driveIndex]) != 1 {
		return path
	}
	drive := parts[driveIndex][0]
	if (drive < 'a' || drive > 'z') && (drive < 'A' || drive > 'Z') {
		return path
	}
	return strings.ToUpper(parts[driveIndex]) + `:\` + strings.Join(parts[driveIndex+1:], `\`)
}
