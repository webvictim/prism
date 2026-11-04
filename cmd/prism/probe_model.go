package main

import (
	"cmp"
	"strings"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/tshwrap"
)

// Built-in probe models, the last resort for `prism test`, `prism doctor`
// and `prism tbot reset` when neither a flag, the config key nor the
// cluster's own model list names one. They are the only model names
// compiled into prism, by decision: a gateway with no fallback refuses a
// request that names no model, and these commands must work out of the
// box even when the cluster's list can't be read (no tbot identity yet, no
// tsh login, a tsh too old to show it).
//
// claude-sonnet-5 is the Anthropic model served both before and after the
// 2026-11-03 Beams model update; gpt-6-luna is the cheapest GPT model on
// that update's list (Luna below Terra below Sol). Either going stale costs
// nothing while the cluster's list is readable, and the probes say which
// config key to set when it isn't.
const (
	defaultAnthropicTestModel = "claude-sonnet-5"
	defaultOpenAITestModel    = "gpt-6-luna"
)

// probeSource says where a probe model came from, for `prism models`.
type probeSource string

const (
	sourceConfig  probeSource = "config"
	sourceCluster probeSource = "cluster"
	sourceBuiltin probeSource = "built-in"
)

// probeChoice is one backend's default probe model and its origin.
type probeChoice struct {
	Model  string
	Source probeSource
}

// listServedModels reads the cluster's served models per app; a variable
// so tests needn't reach a cluster.
var listServedModels = func(cfg *config.Config) (map[string][]string, error) {
	apps, err := clusterApps(cfg)
	if err != nil {
		return nil, err
	}
	return servedModels(apps), nil
}

// probeDefaults is each backend's probe model before any flag: its config
// key, then the cluster's list, then the built-in. The cluster is asked
// only when a backend has no config key, and at most once — a set key
// costs no tsh call at all.
func probeDefaults(cfg *config.Config) (anthropic, openai probeChoice) {
	return probeDefaultsFrom(cfg, func() (map[string][]string, error) { return listServedModels(cfg) })
}

// probeDefaultsFrom is probeDefaults with the cluster's list supplied, for
// a caller that has already read it.
func probeDefaultsFrom(cfg *config.Config, served func() (map[string][]string, error)) (anthropic, openai probeChoice) {
	anthropic = probeChoice{cfg.AnthropicTestModel, sourceConfig}
	openai = probeChoice{cfg.OpenAITestModel, sourceConfig}
	if anthropic.Model != "" && openai.Model != "" {
		return anthropic, openai
	}
	// A listing that fails leaves the map empty, and the built-ins apply.
	list, _ := served()
	if anthropic.Model == "" {
		anthropic = fromCluster(list[router.AnthropicAppName], defaultAnthropicTestModel)
	}
	if openai.Model == "" {
		openai = fromCluster(list[router.OpenAIAppName], defaultOpenAITestModel)
	}
	return anthropic, openai
}

// anthropicProbeModel is the Anthropic probe's default, for the commands
// that only probe Anthropic (doctor, reset).
func anthropicProbeModel(cfg *config.Config) string {
	a, _ := probeDefaults(cfg)
	return a.Model
}

// fromCluster picks a probe model from an app's served models.
//
// The built-in wins when the cluster serves it: it is known cheap and
// known to work on every route the probes use, whereas the first entry is
// whatever the cluster's configuration happens to list first — on the
// cluster this was written against, the most expensive Claude model, and
// a reordered OpenAI list could lead with a model that only speaks
// chat/completions. Otherwise the first served model, since the cluster
// is the authority on what it serves; and with no list at all, the
// built-in.
func fromCluster(served []string, builtin string) probeChoice {
	for _, m := range served {
		if strings.EqualFold(m, builtin) {
			return probeChoice{m, sourceCluster}
		}
	}
	if len(served) > 0 {
		return probeChoice{served[0], sourceCluster}
	}
	return probeChoice{builtin, sourceBuiltin}
}

// servedModels maps each LLM app to the model names a client may send it,
// in the cluster's order. A model's own entries (those that name a
// provider model) come before bare aliases, which on Beams are the
// provider names spelled out again.
func servedModels(apps []tshwrap.App) map[string][]string {
	out := map[string][]string{}
	for _, app := range apps {
		inf := app.Spec.Inference
		if inf == nil {
			continue
		}
		var primary, aliases []string
		for _, m := range inf.Models {
			switch {
			case m.Name == "":
			case m.ProviderName != "":
				primary = append(primary, m.Name)
			default:
				aliases = append(aliases, m.Name)
			}
		}
		out[app.Metadata.Name] = append(primary, aliases...)
	}
	return out
}

// resolveProbeModels applies, per backend: its own flag, then --model,
// then the defaults (config key, cluster, built-in). The per-backend flags
// win because one --model can't suit both: an Anthropic name sent to the
// OpenAI gateway is simply refused. defaults is only called when a
// backend still needs one, so a fully-flagged run makes no tsh call.
func resolveProbeModels(model, anthropicFlag, openaiFlag string, defaults func() (probeChoice, probeChoice)) probeModels {
	m := probeModels{anthropic: cmp.Or(anthropicFlag, model), openai: cmp.Or(openaiFlag, model)}
	if m.anthropic == "" || m.openai == "" {
		a, o := defaults()
		m.anthropic = cmp.Or(m.anthropic, a.Model)
		m.openai = cmp.Or(m.openai, o.Model)
	}
	return m
}
