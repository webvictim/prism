package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/webvictim/prism/internal/chatcompat"
	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tshwrap"
)

// `prism setup opencode` and `prism setup pi` add a "<model> via prism"
// entry for every model the cluster serves — the list `prism models`
// prints — under providers of prism's own, so a tool's built-in providers
// are left alone. There is one provider per wire API, because that is
// what a tool's provider definition picks:
//
//   - the anthropic app's models speak the Messages API;
//   - the openai app's models speak the Responses API, except those the
//     gateway serves only on /v1/chat/completions (Kimi, GLM).
//
// Nothing in the list says which OpenAI model is which, and prism keeps
// no per-model knowledge, so each one's route is asked of the gateway
// once — see probeOpenAIRoute — and is then remembered by the provider
// the entry sits under.
const (
	routeMessages  = "messages"
	routeResponses = "responses"
	routeChat      = "chat"
)

// viaPrismProvider is one of the providers prism adds to a tool's config.
type viaPrismProvider struct {
	ID    string // the provider key in the tool's config
	Name  string // the provider's display name
	Route string
}

var viaPrismProviders = []viaPrismProvider{
	{"prism-anthropic", "Anthropic via prism", routeMessages},
	{"prism-openai", "OpenAI via prism", routeResponses},
	{"prism-openai-chat", "OpenAI chat/completions via prism", routeChat},
}

func viaPrismProviderFor(route string) viaPrismProvider {
	for _, p := range viaPrismProviders {
		if p.Route == route {
			return p
		}
	}
	panic("unknown route " + route)
}

// viaPrismRouteOf maps a provider id back to its route, for reading which
// route an existing entry was given.
func viaPrismRouteOf(providerID string) (string, bool) {
	for _, p := range viaPrismProviders {
		if p.ID == providerID {
			return p.Route, true
		}
	}
	return "", false
}

func viaPrismModelName(model string) string { return model + " via prism" }

// viaPrismSource is what the config writers need: the cluster's model
// names, and a way to learn an OpenAI model's route.
type viaPrismSource struct {
	Anthropic []string
	OpenAI    []string
	Probe     func(model string) (string, error)
	Warn      io.Writer
}

// newViaPrismSource lists the models of the cluster's two LLM apps, by the
// names `prism models` shows (provider-name aliases folded away).
func newViaPrismSource(apps []tshwrap.App, port int, warn io.Writer) viaPrismSource {
	s := viaPrismSource{Warn: warn, Probe: func(model string) (string, error) {
		return probeOpenAIRoute(port, model)
	}}
	for _, a := range llmApps(apps) {
		var names []string
		for _, row := range modelRows(a.Models) {
			names = append(names, row.name)
		}
		switch a.Name {
		case router.AnthropicAppName:
			s.Anthropic = names
		case router.OpenAIAppName:
			s.OpenAI = names
		}
	}
	return s
}

func (s viaPrismSource) empty() bool { return len(s.Anthropic) == 0 && len(s.OpenAI) == 0 }

// place returns the route for every listed model: known says where the
// tool's config already has a model, and only the OpenAI models it
// doesn't cover are probed. A model whose probe fails is left out, with a
// warning, rather than guessed at — a wrong guess would be remembered.
func (s viaPrismSource) place(known map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range s.Anthropic {
		out[m] = routeMessages
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range s.OpenAI {
		if r, ok := known[m]; ok && r != routeMessages {
			out[m] = r
			continue
		}
		wg.Add(1)
		go func(m string) {
			defer wg.Done()
			r, err := s.Probe(m)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fmt.Fprintf(s.Warn, "prism: left out %s: couldn't ask the gateway which API serves it: %v\n", m, err)
				return
			}
			out[m] = r
		}(m)
	}
	wg.Wait()
	return out
}

// byRoute groups placed models per route, sorted.
func byRoute(placed map[string]string) map[string][]string {
	out := map[string][]string{}
	for m, r := range placed {
		out[r] = append(out[r], m)
	}
	for _, ms := range out {
		sort.Strings(ms)
	}
	return out
}

// probeOpenAIRoute asks the gateway, through the router, whether model is
// served on /v1/responses. The gateway's refusal names the other route;
// anything else — success, or some other complaint — means Responses.
// It is one tiny real request per model, made only for a model the
// tool's config doesn't have yet.
func probeOpenAIRoute(port int, model string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":             model,
		"input":             "Reply with OK.",
		"max_output_tokens": 16,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/responses", port), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode == http.StatusBadGateway {
		return "", fmt.Errorf("the router answered %d: %s", resp.StatusCode, bytes.TrimSpace(reply))
	}
	if chatcompat.RefusesResponses(resp.StatusCode, reply) {
		return routeChat, nil
	}
	return routeResponses, nil
}

// configModelApps is the model list for an explicit config command: a
// live look, as `prism models` takes (and, like it, refreshing the
// daemon's copy), falling back to that copy when the cluster can't be
// asked.
func configModelApps(cfg *config.Config) ([]tshwrap.App, error) {
	apps, listedAs, err := clusterAppsFrom(cfg)
	if err == nil {
		if cfg.ModelDiscoveryEnabled() {
			if err := saveModelsCache(apps, listedAs, time.Now()); err != nil {
				fmt.Fprintf(os.Stderr, "prism: could not update the cached model list: %v\n", err)
			}
		}
		return apps, nil
	}
	c, cerr := loadModelsCache()
	if cerr != nil {
		return nil, fmt.Errorf("can't read the cluster's model list: %w", err)
	}
	fmt.Fprintf(os.Stderr, "prism: couldn't ask the cluster (%v); using the list cached %s ago\n",
		err, time.Since(c.FetchedAt).Round(time.Minute))
	return c.Apps, nil
}

// launchModelApps is the model list for a launch, which never asks the
// cluster: the daemon's cached copy, or nothing (with one line saying
// why) so the tool starts anyway with whatever its config already has.
func launchModelApps(cfg *config.Config, tool string) []tshwrap.App {
	if !cfg.ModelDiscoveryEnabled() {
		return nil
	}
	apps, err := cachedApps(cfg)
	if err != nil {
		if !quiet() {
			fmt.Fprintf(os.Stderr, "prism: not refreshing %s's \"via prism\" models: %v\n", tool, err)
		}
		return nil
	}
	return apps
}

// localPort is the router port from the state file, or the default.
func localPort() int {
	s, _ := state.Load()
	if s != nil && s.LocalPort != 0 {
		return s.LocalPort
	}
	return defaultLocalPort
}

var errNoViaPrismModels = errors.New("the cluster's model list is empty")
