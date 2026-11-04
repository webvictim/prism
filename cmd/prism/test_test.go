package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/tshwrap"
)

// stubServed replaces the cluster lookup for one test, counting calls.
func stubServed(t *testing.T, served map[string][]string, err error) *int {
	t.Helper()
	calls := 0
	prev := listServedModels
	listServedModels = func(*config.Config) (map[string][]string, error) {
		calls++
		return served, err
	}
	t.Cleanup(func() { listServedModels = prev })
	return &calls
}

// The cluster this was written against lists its most expensive Claude
// first; the probes must not follow it there.
var liveServed = map[string][]string{
	router.AnthropicAppName: {"claude-opus-5-5", "claude-sonnet-5", "anthropic.claude-opus-5-5", "anthropic.claude-sonnet-5"},
	router.OpenAIAppName:    {"gpt-6-luna", "gpt-6.1-terra", "gpt-6.1-sol", "glm-5", "kimi-k2.5"},
}

// Per backend: the config key, then the cluster's list, then the built-in.
func TestProbeDefaults(t *testing.T) {
	cases := []struct {
		name         string
		cfg          *config.Config
		served       map[string][]string
		err          error
		wantA, wantO probeChoice
		wantLookups  int
	}{
		{"config wins, and asks nobody",
			&config.Config{AnthropicTestModel: "cfg-a", OpenAITestModel: "cfg-o"}, liveServed, nil,
			probeChoice{"cfg-a", sourceConfig}, probeChoice{"cfg-o", sourceConfig}, 0},
		{"the cluster's list, preferring the built-in it serves",
			&config.Config{}, liveServed, nil,
			probeChoice{"claude-sonnet-5", sourceCluster}, probeChoice{"gpt-6-luna", sourceCluster}, 1},
		{"a built-in the cluster dropped gives way to the cluster's first",
			&config.Config{}, map[string][]string{
				router.AnthropicAppName: {"claude-next"},
				router.OpenAIAppName:    {"gpt-7-luna", "gpt-7-sol"},
			}, nil,
			probeChoice{"claude-next", sourceCluster}, probeChoice{"gpt-7-luna", sourceCluster}, 1},
		{"no list at all: the built-ins",
			&config.Config{}, nil, errors.New("no tsh login"),
			probeChoice{defaultAnthropicTestModel, sourceBuiltin}, probeChoice{defaultOpenAITestModel, sourceBuiltin}, 1},
		{"one key set: the other from the cluster, one lookup",
			&config.Config{OpenAITestModel: "cfg-o"}, liveServed, nil,
			probeChoice{"claude-sonnet-5", sourceCluster}, probeChoice{"cfg-o", sourceConfig}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := stubServed(t, c.served, c.err)
			a, o := probeDefaults(c.cfg)
			if a != c.wantA || o != c.wantO {
				t.Errorf("got anthropic=%+v openai=%+v, want %+v %+v", a, o, c.wantA, c.wantO)
			}
			if *calls != c.wantLookups {
				t.Errorf("cluster asked %d times, want %d", *calls, c.wantLookups)
			}
		})
	}
}

// Flags come before every default. One --model can't suit both backends,
// so the per-backend flags must win over it and reach only their own
// probes; and a run whose flags name both makes no tsh call.
func TestResolveProbeModels(t *testing.T) {
	defaults := func() (probeChoice, probeChoice) {
		return probeChoice{"def-a", sourceCluster}, probeChoice{"def-o", sourceCluster}
	}
	cases := []struct {
		name                       string
		model, anthropicF, openaiF string
		wantAnthropic, wantOpenAI  string
		wantDefaultsAsked          bool
	}{
		{"nothing given", "", "", "", "def-a", "def-o", true},
		{"--model covers both", "m", "", "", "m", "m", false},
		{"own flags beat --model", "m", "a", "o", "a", "o", false},
		{"a flag reaches only its backend", "", "a", "", "a", "def-o", true},
		{"and the other way round", "", "", "o", "def-a", "o", true},
	}
	for _, c := range cases {
		asked := false
		got := resolveProbeModels(c.model, c.anthropicF, c.openaiF, func() (probeChoice, probeChoice) {
			asked = true
			return defaults()
		})
		if got.anthropic != c.wantAnthropic || got.openai != c.wantOpenAI {
			t.Errorf("%s: got anthropic=%q openai=%q, want %q %q", c.name, got.anthropic, got.openai, c.wantAnthropic, c.wantOpenAI)
		}
		if asked != c.wantDefaultsAsked {
			t.Errorf("%s: defaults asked = %v, want %v", c.name, asked, c.wantDefaultsAsked)
		}
	}
}

// Bare aliases that spell the provider name out again come after the
// models' own names, so "first served" is never an alias.
func TestServedModels(t *testing.T) {
	var app tshwrap.App
	app.Metadata.Name = router.AnthropicAppName
	app.Spec.Inference = &tshwrap.Inference{Format: "anthropic", Models: []tshwrap.InferenceModel{
		{Name: "anthropic.claude-sonnet-5"},
		{Name: "claude-sonnet-5", ProviderName: "anthropic.claude-sonnet-5"},
	}}
	var plain tshwrap.App
	plain.Metadata.Name = "grafana"
	got := servedModels([]tshwrap.App{app, plain})
	if want := []string{"claude-sonnet-5", "anthropic.claude-sonnet-5"}; !slices.Equal(got[router.AnthropicAppName], want) {
		t.Errorf("served = %v, want %v", got[router.AnthropicAppName], want)
	}
	if _, ok := got["grafana"]; ok {
		t.Error("an app with no inference block was listed as serving models")
	}
}

// Every probe a format runs must take its model from the backend it
// talks to.
func TestProbeModelsForFormat(t *testing.T) {
	m := probeModels{anthropic: "a", openai: "o"}
	want := map[string]string{
		formatAnthropic:         "a",
		formatOpenAICompletions: "o",
		formatOpenAIResponses:   "o",
	}
	for _, f := range allFormats {
		if got := m.forFormat(f); got != want[f] {
			t.Errorf("forFormat(%s) = %q, want %q", f, got, want[f])
		}
	}
}

// The hint names a flag and a config key; both must actually feed the
// probe that failed, or the advice is a dead end.
func TestModelLeversFeedTheirProbe(t *testing.T) {
	stubServed(t, liveServed, nil)
	for _, f := range allFormats {
		flag, key := modelLevers(f)

		var anthropicF, openaiF string
		switch flag {
		case "--anthropic-model":
			anthropicF = "x"
		case "--openai-model":
			openaiF = "x"
		default:
			t.Fatalf("%s: hint names %q, not a flag prism test has", f, flag)
		}
		none := func() (probeChoice, probeChoice) { return probeDefaults(&config.Config{}) }
		if got := resolveProbeModels("", anthropicF, openaiF, none).forFormat(f); got != "x" {
			t.Errorf("%s: the hint names %s, but setting it gives model %q", f, flag, got)
		}

		cfg := &config.Config{}
		if !slices.Contains(configKeys, key) {
			t.Fatalf("%s: hint names %q, not a key prism config knows", f, key)
		}
		*testModelField(cfg, key) = "y"
		fromCfg := func() (probeChoice, probeChoice) { return probeDefaults(cfg) }
		if got := resolveProbeModels("", "", "", fromCfg).forFormat(f); got != "y" {
			t.Errorf("%s: the hint names %s, but setting it gives model %q", f, key, got)
		}
	}
}

// The keys are the lever for a gateway that stops serving the default,
// so set must reach the probes ahead of the cluster's list, and unset
// must hand back to it.
func TestTestModelConfigKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	stubServed(t, liveServed, nil)

	if err := configSet("anthropic_test_model", "  claude-opus-5-5 "); err != nil {
		t.Fatal(err)
	}
	if err := configSet("openai_test_model", "gpt-6.1-sol"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if a, o := probeDefaults(cfg); a.Model != "claude-opus-5-5" || o.Model != "gpt-6.1-sol" {
		t.Errorf("after set: %s, %s; want the configured models, trimmed", a.Model, o.Model)
	}

	if err := configSet("anthropic_test_model", " "); err == nil {
		t.Error("setting a blank model should be refused in favour of unset")
	}

	for _, k := range []string{"anthropic_test_model", "openai_test_model"} {
		if err := configUnset(k); err != nil {
			t.Fatal(err)
		}
	}
	if cfg, err = config.Load(); err != nil {
		t.Fatal(err)
	}
	if a, o := probeDefaults(cfg); a.Source != sourceCluster || o.Source != sourceCluster {
		t.Errorf("after unset: %+v, %+v; want the cluster's picks back", a, o)
	}
}
