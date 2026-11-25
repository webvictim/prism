package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/webvictim/prism/internal/config"
)

// cmdPi starts prism, refreshes Pi's model overrides, and launches Pi.
//
// `prism pi config` was prism's setup command until `prism setup pi`
// replaced it, and it shadows Pi's own `pi config`. It stays as a
// deprecated alias for now, saying where it went; once it goes, `prism pi
// config` reaches Pi's command like any other argument.
func cmdPi(args []string) error {
	if len(args) > 0 && args[0] == "config" {
		fmt.Fprintln(os.Stderr, "prism: `prism pi config` is deprecated and will pass through to Pi's own `pi config` in a future release — use `prism setup pi`")
		return setupPi(args[1:])
	}
	return runToolWithPrismSetup("pi", args, configurePiForLaunch)
}

// setupPi implements `prism setup pi`: it updates models.json without starting Pi.
func setupPi(args []string) error {
	fs := flag.NewFlagSet("setup pi", flag.ExitOnError)
	anthropicModel := fs.String("anthropic-model", "", "only route this one of Pi's own Anthropic models through prism (default: all of them)")
	openaiModel := fs.String("openai-model", "", "only route this one of Pi's own OpenAI models through prism (default: all of them)")
	overwrite := fs.Bool("overwrite", false, "replace every provider in models.json with prism's \"via prism\" models only (backs the file up first)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *overwrite && (*anthropicModel != "" || *openaiModel != "") {
		return errors.New("setup pi: --overwrite keeps only the \"via prism\" models, so --anthropic-model and --openai-model don't apply")
	}

	port := localPort()
	piDir, err := piModelsDir()
	if err != nil {
		return err
	}

	var via *viaPrismSource
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	apps, err := configModelApps(cfg)
	switch {
	case err == nil:
		if !portResponds(port) {
			fmt.Fprintf(os.Stderr, "prism: the router isn't answering on 127.0.0.1:%d, so new OpenAI models can't be checked and will be left out — `prism up` first\n", port)
		}
		src := newViaPrismSource(apps, port, os.Stderr)
		if !src.empty() {
			via = &src
		}
	case *overwrite:
		return err
	default:
		fmt.Fprintf(os.Stderr, "prism: not adding \"via prism\" models: %v\n", err)
	}
	if *overwrite && via == nil {
		return errNoViaPrismModels
	}

	res, err := writePiModelsConfig(piDir, port, *anthropicModel, *openaiModel, via, *overwrite)
	if err != nil {
		return err
	}

	if res.Backup != "" {
		fmt.Fprintf(os.Stderr, "prism: backed up %s to %s\n", res.Path, res.Backup)
	}
	fmt.Fprintf(os.Stderr, "prism: wrote %s\n", res.Path)
	for _, provider := range sortedKeys(res.Hijacked) {
		fmt.Fprintf(os.Stderr, "prism: %d of Pi's own %s model(s) now route through prism on 127.0.0.1:%d\n",
			res.Hijacked[provider], provider, port)
	}
	for _, p := range viaPrismProviders {
		if ms := res.Added[p.ID]; len(ms) > 0 {
			fmt.Fprintf(os.Stderr, "prism: added to %s: %s\n", p.ID, strings.Join(ms, ", "))
		}
	}
	if res.ViaPrism > 0 {
		fmt.Fprintf(os.Stderr, "prism: %d \"via prism\" model(s) on 127.0.0.1:%d\n", res.ViaPrism, port)
	}
	switch {
	case *overwrite:
		fmt.Fprintln(os.Stderr, "prism: launch with `prism exec pi` to keep only these; `prism pi` routes Pi's own models through prism again")
	case *anthropicModel != "" || *openaiModel != "":
		fmt.Fprintln(os.Stderr, "prism: launch with `prism exec pi` to keep this narrowed selection; `prism pi` restores all catalog models")
	default:
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
	var via *viaPrismSource
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = &config.Config{}
	}
	if apps := launchModelApps(cfg, "Pi"); apps != nil {
		if src := newViaPrismSource(apps, port, os.Stderr); !src.empty() {
			via = &src
		}
	}
	return preparePiForLaunch(piDir, port, via, func() error {
		return refreshPiModelCatalog(bin, env)
	})
}

func preparePiForLaunch(piDir string, port int, via *viaPrismSource, refresh func() error) error {
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

	_, err = writePiModelsConfig(piDir, port, "", "", via, false)
	return err
}

func refreshPiModelCatalog(bin string, env []string) error {
	cmd := exec.Command(bin, "update", "--models")
	cmd.Env = env
	cmd.Stdin, cmd.Stdout = childStdio()
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

type piResult struct {
	Path     string
	Backup   string
	Hijacked map[string]int      // Pi's own provider → models repointed
	Added    map[string][]string // prism provider → models added
	ViaPrism int                 // models under prism's providers afterwards
}

// writePiModelsConfig reads Pi's cached catalog (models-store.json) and
// writes Pi's config (models.json).
//
// Normal setup writes both kinds of entries when the cluster list is available:
//
//   - Built-in provider overrides (anthropic, openai): copy Pi's catalog
//     entries by ID and set baseUrl per model. anthropicModel and openaiModel
//     are the IDs from --anthropic-model and --openai-model: a nonempty ID
//     reroutes just that model; an empty string reroutes all catalog models
//     for that provider. Neither argument sets Pi's default model or limits
//     "via prism" entries. The provider sets apiKey; each model has its own route.
//   - "Via prism" providers (prism-anthropic, prism-openai, prism-openai-chat):
//     take IDs from the cluster list (via) and borrow metadata from Pi's
//     catalog when available. Set api, baseUrl, and apiKey once on the
//     provider. Models inherit that route unless they specify their own.
//
// Existing "via prism" models are kept on normal refreshes. If via is nil,
// no new "via prism" entries are added. Overwrite backs up models.json and
// replaces its providers with only the "via prism" providers; other
// top-level keys stay.
func writePiModelsConfig(piDir string, port int, anthropicModel, openaiModel string, via *viaPrismSource, overwrite bool) (piResult, error) {
	res := piResult{Hijacked: map[string]int{}, Added: map[string][]string{}}
	storePath := filepath.Join(piDir, "models-store.json")
	store, err := loadPiStore(storePath)
	if err != nil {
		return res, err
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	res.Path = filepath.Join(piDir, "models.json")
	orig, err := os.ReadFile(res.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	configFile, err := loadPiConfig(res.Path)
	if err != nil {
		return res, err
	}
	existing := configFile["providers"].(map[string]any)

	providers := map[string]any{}
	if !overwrite {
		only := map[string]string{
			piAnthropic: anthropicModel,
			piOpenAI:    openaiModel,
		}
		for _, provider := range []string{piAnthropic, piOpenAI} {
			modelBaseURL := piBaseURL(provider, base)
			models := piOverrides(store[provider], only[provider], modelBaseURL)
			if len(models) == 0 {
				continue
			}
			if provider == piAnthropic {
				previous, _ := existing[provider].(map[string]any)
				defaults := piRouteDefaults{
					API:     piAnthropicMessagesAPI, // Assume the built-in API if the catalog omits it.
					BaseURL: modelBaseURL,           // Same URL piOverrides puts on each model.
				}
				piConfigureMessagesAffinity(models, defaults, base, piAffinityOptOuts(previous))
			}
			providers[provider] = map[string]any{
				// Pi hides models when a provider has no API key. The router
				// strips this dummy token — the tunnel uses mTLS.
				"apiKey": "teleport",
				"models": models,
			}
			res.Hijacked[provider] = len(models)
		}
		if len(providers) == 0 && via == nil {
			return res, fmt.Errorf("pi setup: no models to write (checked %s)", storePath)
		}
	}

	if via != nil {
		for id, p := range piViaPrismProviders(existing, store, base, *via, overwrite, res.Added) {
			providers[id] = p
			res.ViaPrism += len(p["models"].([]map[string]any))
		}
	} else if !overwrite {
		// Discovery may be unavailable on launch. Update existing Messages
		// entries in place without rebuilding their provider or probing.
		p, _ := existing[viaPrismProviderFor(routeMessages).ID].(map[string]any)
		api, _ := p["api"].(string)
		url, _ := p["baseUrl"].(string)
		piConfigureMessagesAffinity(piConfiguredModels(p), piRouteDefaults{API: api, BaseURL: url}, base, piAffinityOptOuts(p))
	}

	if overwrite {
		configFile["providers"] = providers
	} else if err := mergePiProviders(configFile, providers); err != nil {
		return res, err
	}
	data, err := json.MarshalIndent(configFile, "", "  ")
	if err != nil {
		return res, err
	}
	data = append(data, '\n')
	if string(data) == string(orig) {
		return res, nil
	}
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return res, err
	}
	if overwrite && len(orig) > 0 {
		res.Backup = res.Path + ".bak"
		if err := os.WriteFile(res.Backup, orig, 0o600); err != nil {
			return res, fmt.Errorf("back up %s: %w", res.Path, err)
		}
	}
	if err := writeFileAtomic(res.Path, data); err != nil {
		return res, err
	}
	return res, nil
}

// piViaPrismProviders builds prism's providers for models.json. Existing
// entries retain their settings (except prism's gateway compatibility
// changes) and say which route a model was given, so it isn't probed again.
func piViaPrismProviders(existing map[string]any, store map[string]piStoreProvider, base string, via viaPrismSource, overwrite bool, added map[string][]string) map[string]map[string]any {
	kept := map[string][]map[string]any{}
	known := map[string]string{}
	for _, p := range viaPrismProviders {
		prov, _ := existing[p.ID].(map[string]any)
		for _, entry := range piConfiguredModels(prov) {
			id, _ := entry["id"].(string)
			if id == "" {
				continue
			}
			known[id] = p.Route
			// The user's edits stay; only what the gateway refuses goes.
			piForGateway(entry)
			kept[p.ID] = append(kept[p.ID], entry)
		}
	}
	placed := via.place(known)

	out := map[string]map[string]any{}
	for _, p := range viaPrismProviders {
		var models []map[string]any
		if !overwrite {
			models = kept[p.ID]
		}
		for _, m := range byRoute(placed)[p.Route] {
			if _, had := known[m]; had && !overwrite {
				continue
			}
			if _, had := known[m]; !had {
				added[p.ID] = append(added[p.ID], m)
			}
			models = append(models, piViaPrismModel(store, p.Route, m))
		}
		if len(models) == 0 {
			continue
		}
		url, api := base+"/v1", "openai-responses"
		switch p.Route {
		case routeMessages:
			url, api = base, piAnthropicMessagesAPI
			var optOuts map[string]bool
			if !overwrite {
				previous, _ := existing[p.ID].(map[string]any)
				optOuts = piAffinityOptOuts(previous)
			}
			piConfigureMessagesAffinity(models, piRouteDefaults{API: api, BaseURL: url}, base, optOuts)
		case routeChat:
			api = "openai-completions"
		}
		out[p.ID] = map[string]any{
			"baseUrl": url,
			"api":     api,
			"apiKey":  "teleport",
			"models":  models,
		}
	}
	return out
}

// piViaPrismModel is one "via prism" entry: Pi's registry metadata for the
// id when it has it — cost, context window, capabilities — else the
// minimum Pi needs. Where the request goes is the provider's business, so
// the registry's own routing fields are dropped.
func piViaPrismModel(store map[string]piStoreProvider, route, id string) map[string]any {
	catalog := piOpenAI
	if route == routeMessages {
		catalog = piAnthropic
	}
	entry := map[string]any{"input": []string{"text"}}
	for _, m := range store[catalog].Models {
		if mid, _ := m["id"].(string); mid == id {
			entry = make(map[string]any, len(m))
			for k, v := range m {
				entry[k] = v
			}
			break
		}
	}
	for _, k := range []string{"baseUrl", "api", "provider"} {
		delete(entry, k)
	}
	entry["id"] = id
	entry["name"] = viaPrismModelName(id)
	piForGateway(entry)
	return entry
}

// piForGateway turns off what Pi's registry says a model supports on the
// vendor's own API but the gateway refuses — a capability of the gateway,
// not of any one model.
//
// supportsMidConvoEffort makes Pi carry the thinking level in
// `configuration_update` input items, which the Bedrock-backed gateway
// rejects outright ("The 'configuration_update' item type is not
// supported with this model"). Without it Pi sends reasoning.effort,
// which the gateway takes, so the chosen thinking level still applies.
func piForGateway(entry map[string]any) {
	compat, ok := entry["compat"].(map[string]any)
	if !ok {
		return
	}
	if on, _ := compat["supportsMidConvoEffort"].(bool); !on {
		return
	}
	piSetCompatBool(entry, "supportsMidConvoEffort", false)
}

const piSessionAffinity = "sendSessionAffinityHeaders"

// piConfiguredModels reads the model entries from a decoded models.json
// provider. Writers build their replacement lists as []map[string]any.
func piConfiguredModels(provider map[string]any) []map[string]any {
	raw, _ := provider["models"].([]any)
	var models []map[string]any
	for _, m := range raw {
		if entry, ok := m.(map[string]any); ok {
			models = append(models, entry)
		}
	}
	return models
}

// piAffinityOptOuts reads one existing provider object from models.json.
// It accepts per-model settings in two shapes:
//
//   - models: [{"id":"example","compat":{"sendSessionAffinityHeaders":false}}]
//   - modelOverrides: {"example":{"compat":{"sendSessionAffinityHeaders":true}}}
//
// In models, the ID is a field in each entry; in modelOverrides, it's the
// object key. Only explicit boolean values for this flag count. If both
// locations set it for the same ID, modelOverrides wins, as it does in Pi.
//
// The returned map is keyed by model ID: true means affinity was explicitly
// turned off; false means it was explicitly turned on. An absent ID means
// there was no explicit choice. The caller disables affinity only for IDs
// mapped to true. This function does not modify the provider.
func piAffinityOptOuts(provider map[string]any) map[string]bool {
	optOuts := map[string]bool{}
	read := func(id string, entry map[string]any) {
		compat, _ := entry["compat"].(map[string]any)
		if enabled, ok := compat[piSessionAffinity].(bool); ok && id != "" {
			optOuts[id] = !enabled
		}
	}
	for _, entry := range piConfiguredModels(provider) {
		id, _ := entry["id"].(string)
		read(id, entry)
	}
	overrides, _ := provider["modelOverrides"].(map[string]any)
	for id, raw := range overrides {
		entry, _ := raw.(map[string]any)
		read(id, entry)
	}
	return optOuts
}

// piRouteDefaults supplies routing values for a model that omits api or
// baseUrl. These are inputs to the affinity check, not fields to write.
// For "via prism" entries they come from the enclosing provider.
type piRouteDefaults struct {
	API     string
	BaseURL string
}

// piConfigureMessagesAffinity edits only compat.sendSessionAffinityHeaders
// on the model entries in models. Each call handles one provider. The two
// usual shapes, with unrelated fields omitted, are:
//
//   - Built-in anthropic override: the model carries its route directly:
//     {"id":"example","api":"anthropic-messages","baseUrl":"http://127.0.0.1:7331"}
//   - prism-anthropic model: {"id":"example"} inherits api and baseUrl
//     from the enclosing provider. A model can still specify either field.
//
// defaults supplies api and baseUrl only when the model omits them. For
// prism-anthropic these are the provider's settings; for built-in anthropic
// overrides, the URL is the same one piOverrides already puts on every model.
// prismRouterURL is the current local router URL to check against, not a
// fallback. Only Anthropic Messages models targeting that URL (ignoring
// trailing slashes) get the flag. optOuts maps model IDs to true for an
// explicit opt-out: those get false, and other matching models get true.
// No routing fields change. The caller writes models.json.
func piConfigureMessagesAffinity(models []map[string]any, defaults piRouteDefaults, prismRouterURL string, optOuts map[string]bool) {
	for _, entry := range models {
		api, _ := entry["api"].(string)
		if api == "" {
			api = defaults.API
		}
		url, _ := entry["baseUrl"].(string)
		if url == "" {
			url = defaults.BaseURL
		}
		if api == piAnthropicMessagesAPI && strings.TrimRight(url, "/") == prismRouterURL {
			id, _ := entry["id"].(string)
			piSetCompatBool(entry, piSessionAffinity, !optOuts[id])
		}
	}
}

// A model entry generated for models.json can still share its compat map
// with the entry read from models-store.json. Copy the map so the change
// affects only the generated override, not the in-memory source catalog.
func piSetCompatBool(entry map[string]any, key string, value bool) {
	compat, _ := entry["compat"].(map[string]any)
	if got, ok := compat[key].(bool); ok && got == value {
		return
	}
	cp := make(map[string]any, len(compat)+1)
	maps.Copy(cp, compat)
	cp[key] = value
	entry["compat"] = cp
}

// writeFileAtomic replaces a tool's config file atomically, keeping its
// permissions, so an interrupted launch can't leave the tool's whole
// configuration truncated.
func writeFileAtomic(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
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

// piAnthropicMessagesAPI is Pi's models.json API identifier for Anthropic
// Messages. It selects the request format, not a provider or a model.
const piAnthropicMessagesAPI = "anthropic-messages"

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
		return nil, fmt.Errorf("pi setup: parse %s: %w", path, err)
	}
	if config == nil {
		config = map[string]any{}
	}
	if providers, ok := config["providers"]; ok && providers != nil {
		if _, ok := providers.(map[string]any); !ok {
			return nil, fmt.Errorf("pi setup: %s: providers must be an object", path)
		}
	} else {
		config["providers"] = map[string]any{}
	}
	return config, nil
}

func mergePiProviders(config map[string]any, replacements map[string]any) error {
	providers, ok := config["providers"].(map[string]any)
	if !ok {
		return errors.New("pi setup: providers must be an object")
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
			return nil, fmt.Errorf("pi setup: %w at %s — run `pi update --models` to populate it", errPiStoreMissing, path)
		}
		return nil, err
	}
	var store map[string]piStoreProvider
	if err := json.Unmarshal(b, &store); err != nil {
		return nil, fmt.Errorf("pi setup: parse %s: %w", path, err)
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
		piForGateway(cp)
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
	return piAnthropicMessagesAPI
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
		return expandHome(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pi", "agent"), nil
}
