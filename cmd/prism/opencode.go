package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/jsonc"
)

// OpenCode v2 ignores ANTHROPIC_BASE_URL and OPENAI_BASE_URL — verified on
// 2.2.5, with the variables present in its background service's own
// environment — and talks to the vendors directly with prism's dummy key
// ("invalid x-api-key"). The only lever is its config file, so `prism
// setup opencode` writes providers of prism's own there (see
// via_prism.go), and every `prism opencode` launch refreshes them.

// cmdOpencode starts prism, refreshes the "via prism" providers and
// launches OpenCode. Every argument is OpenCode's: prism's own setup
// command is `prism setup opencode`, so nothing here can shadow a
// subcommand OpenCode has or adds later.
func cmdOpencode(args []string) error {
	return runToolWithPrismSetup("opencode", args, configureOpencodeForLaunch)
}

func setupOpencode(args []string) error {
	fs := flag.NewFlagSet("setup opencode", flag.ExitOnError)
	overwrite := fs.Bool("overwrite", false, "replace OpenCode's whole provider block with prism's providers only (backs the file up first)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	apps, err := configModelApps(cfg)
	if err != nil {
		return err
	}
	port := localPort()
	if !portResponds(port) {
		fmt.Fprintf(os.Stderr, "prism: the router isn't answering on 127.0.0.1:%d, so new OpenAI models can't be checked and will be left out — `prism up` first\n", port)
	}
	src := newViaPrismSource(apps, port, os.Stderr)
	if src.empty() {
		return errNoViaPrismModels
	}
	path, err := opencodeConfigPath()
	if err != nil {
		return err
	}
	res, err := writeOpencodeConfig(path, port, src, *overwrite)
	if err != nil {
		return err
	}
	res.report(os.Stderr, path, port)
	if res.Changed {
		if bin, err := resolveToolBin("opencode"); err == nil {
			reloadOpencode(bin, os.Environ())
		}
	}
	return nil
}

// configureOpencodeForLaunch is the launch-time refresh: the daemon's
// cached model list, additive only, and never a reason not to start.
func configureOpencodeForLaunch(bin string, port int, env []string) error {
	cfg, _ := config.Load()
	if cfg == nil {
		cfg = &config.Config{}
	}
	apps := launchModelApps(cfg, "OpenCode")
	if apps == nil {
		return nil
	}
	src := newViaPrismSource(apps, port, os.Stderr)
	if src.empty() {
		return nil
	}
	path, err := opencodeConfigPath()
	if err == nil {
		var res opencodeResult
		if res, err = writeOpencodeConfig(path, port, src, false); err == nil {
			if res.Changed {
				if !quiet() {
					res.report(os.Stderr, path, port)
				}
				reloadOpencode(bin, env)
			}
			return nil
		}
	}
	fmt.Fprintf(os.Stderr, "prism: not refreshing OpenCode's \"via prism\" models: %v\n", err)
	return nil
}

// opencodeConfigPath is OpenCode's global config file. It reads
// config.json, opencode.json and opencode.jsonc from its config dir and
// merges them in that order, so the last one present is where a value
// wins; with none, prism creates opencode.json.
func opencodeConfigPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	dir = filepath.Join(dir, "opencode")
	for _, name := range []string{"opencode.jsonc", "opencode.json", "config.json"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return filepath.Join(dir, "opencode.json"), nil
}

type opencodeResult struct {
	Changed  bool
	Added    map[string][]string // provider id → models added
	Total    int                 // models under prism's providers afterwards
	Backup   string
	Warnings []string
}

func (r opencodeResult) report(w io.Writer, path string, port int) {
	if r.Backup != "" {
		fmt.Fprintf(w, "prism: backed up %s to %s\n", path, r.Backup)
	}
	if r.Changed {
		fmt.Fprintf(w, "prism: wrote %s\n", path)
	} else {
		fmt.Fprintf(w, "prism: %s already up to date\n", path)
	}
	for _, p := range viaPrismProviders {
		if ms := r.Added[p.ID]; len(ms) > 0 {
			fmt.Fprintf(w, "prism: added to %s: %s\n", p.ID, strings.Join(ms, ", "))
		}
	}
	fmt.Fprintf(w, "prism: %d model(s) route through prism on 127.0.0.1:%d — pick one named \"… via prism\"\n", r.Total, port)
	for _, warn := range r.Warnings {
		fmt.Fprintln(w, "prism: warning: "+warn)
	}
}

// opencodeDoc is the part of OpenCode's config prism reads.
type opencodeDoc struct {
	Provider map[string]struct {
		Models map[string]any `json:"models"`
	} `json:"provider"`
	Disabled []string `json:"disabled_providers"`
	Enabled  []string `json:"enabled_providers"`
}

// writeOpencodeConfig adds prism's providers to the config at path,
// editing the JSONC in place so comments and everything else survive.
//
// By default it only adds: each provider's baseURL and apiKey are kept
// current (the router port can change), and a model gets an entry only
// if none of prism's providers has one — so a user's edits to an entry,
// a context limit say, stay. overwrite instead replaces the whole
// `provider` block with prism's providers, after backing the file up.
func writeOpencodeConfig(path string, port int, src viaPrismSource, overwrite bool) (opencodeResult, error) {
	res := opencodeResult{Added: map[string][]string{}}
	orig, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	err = nil // a missing file is an empty config
	var doc opencodeDoc
	if len(strings.TrimSpace(string(jsonc.Standardize(orig)))) > 0 {
		if err := jsonc.Unmarshal(orig, &doc); err != nil {
			return res, fmt.Errorf("parse %s: %w", path, err)
		}
	}

	known := map[string]string{}
	for id, p := range doc.Provider {
		if route, ok := viaPrismRouteOf(id); ok {
			for m := range p.Models {
				known[m] = route
			}
		}
	}
	placed := src.place(known)
	groups := byRoute(placed)
	baseURL := fmt.Sprintf("http://127.0.0.1:%d/v1", port)

	out := orig
	set := func(value any, path ...string) {
		if err == nil {
			out, err = jsonc.Set(out, path, value)
		}
	}
	if overwrite {
		providers := map[string]any{}
		for _, p := range viaPrismProviders {
			if len(groups[p.Route]) == 0 {
				continue
			}
			models := map[string]any{}
			for _, m := range groups[p.Route] {
				models[m] = map[string]any{"name": viaPrismModelName(m)}
				if _, had := known[m]; !had {
					res.Added[p.ID] = append(res.Added[p.ID], m)
				}
			}
			providers[p.ID] = opencodeProvider(p, baseURL, models)
			res.Total += len(models)
		}
		set(providers, "provider")
	} else {
		for _, p := range viaPrismProviders {
			_, exists := doc.Provider[p.ID]
			var fresh []string
			for _, m := range groups[p.Route] {
				if _, had := known[m]; !had {
					fresh = append(fresh, m)
				}
			}
			if !exists && len(fresh) == 0 {
				continue
			}
			if !exists {
				set(opencodeProvider(p, baseURL, map[string]any{}), "provider", p.ID)
			} else {
				set(npmFor(p.Route), "provider", p.ID, "npm")
				set(baseURL, "provider", p.ID, "options", "baseURL")
				set(opencodeDummyKey, "provider", p.ID, "options", "apiKey")
			}
			for _, m := range fresh {
				set(map[string]any{"name": viaPrismModelName(m)}, "provider", p.ID, "models", m)
			}
			res.Added[p.ID] = fresh
			res.Total += len(doc.Provider[p.ID].Models) + len(fresh)
		}
	}
	if err != nil {
		return res, fmt.Errorf("edit %s: %w", path, err)
	}
	if len(orig) == 0 && !strings.Contains(string(out), "$schema") {
		out, err = jsonc.Set(out, []string{"$schema"}, "https://opencode.ai/config.json")
		if err != nil {
			return res, err
		}
	}

	for _, p := range viaPrismProviders {
		if slices.Contains(doc.Disabled, p.ID) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s is in disabled_providers in %s, so OpenCode won't offer its models", p.ID, path))
		}
		if len(doc.Enabled) > 0 && !slices.Contains(doc.Enabled, p.ID) && len(groups[p.Route]) > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("enabled_providers in %s doesn't list %s, so OpenCode won't offer its models", path, p.ID))
		}
	}

	if string(out) == string(orig) {
		return res, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, err
	}
	if overwrite && len(orig) > 0 {
		res.Backup = path + ".bak"
		if err := os.WriteFile(res.Backup, orig, 0o600); err != nil {
			return res, fmt.Errorf("back up %s: %w", path, err)
		}
	}
	if err := writeFileAtomic(path, out); err != nil {
		return res, err
	}
	res.Changed = true
	return res, nil
}

// opencodeDummyKey is what OpenCode sends; the router strips it, since the
// tunnel authenticates with mTLS. OpenCode needs one to offer a provider.
const opencodeDummyKey = "teleport"

func opencodeProvider(p viaPrismProvider, baseURL string, models map[string]any) map[string]any {
	return map[string]any{
		"name": p.Name,
		"npm":  npmFor(p.Route),
		"options": map[string]any{
			"baseURL": baseURL,
			"apiKey":  opencodeDummyKey,
		},
		"models": models,
	}
}

// npmFor is the AI SDK package OpenCode loads for a route. @ai-sdk/openai
// speaks the Responses API under any provider id; the compatible package
// speaks chat/completions.
func npmFor(route string) string {
	switch route {
	case routeMessages:
		return "@ai-sdk/anthropic"
	case routeResponses:
		return "@ai-sdk/openai"
	default:
		return "@ai-sdk/openai-compatible"
	}
}

// reloadOpencode tells OpenCode's background service to re-read its
// config. The service also notices on its own, so this is belt and
// braces: a failure is a warning, never a reason not to launch.
func reloadOpencode(bin string, env []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "reload")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "prism: `opencode reload` failed (%v): %s\n", err, strings.TrimSpace(string(out)))
	}
}
