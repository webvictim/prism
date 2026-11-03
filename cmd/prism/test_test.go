package main

import (
	"slices"
	"testing"

	"github.com/webvictim/prism/internal/config"
)

// Per backend: its own flag, then --model, then its config key, then the
// built-in default. One --model can't suit both backends, so the
// per-backend flags must win over it and reach only their own probes.
func TestResolveProbeModels(t *testing.T) {
	none := &config.Config{}
	configured := &config.Config{AnthropicTestModel: "cfg-a", OpenAITestModel: "cfg-o"}
	cases := []struct {
		name                       string
		model, anthropicF, openaiF string
		cfg                        *config.Config
		wantAnthropic, wantOpenAI  string
	}{
		{"nothing given", "", "", "", none, defaultAnthropicTestModel, ""},
		{"config", "", "", "", configured, "cfg-a", "cfg-o"},
		{"--model beats config", "m", "", "", configured, "m", "m"},
		{"own flags beat --model", "m", "a", "o", configured, "a", "o"},
		{"a flag reaches only its backend", "", "a", "", none, "a", ""},
		{"and the other way round", "", "", "o", none, defaultAnthropicTestModel, "o"},
	}
	for _, c := range cases {
		got := resolveProbeModels(c.model, c.anthropicF, c.openaiF, c.cfg)
		if got.anthropic != c.wantAnthropic || got.openai != c.wantOpenAI {
			t.Errorf("%s: got anthropic=%q openai=%q, want anthropic=%q openai=%q",
				c.name, got.anthropic, got.openai, c.wantAnthropic, c.wantOpenAI)
		}
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
		if got := resolveProbeModels("", anthropicF, openaiF, &config.Config{}).forFormat(f); got != "x" {
			t.Errorf("%s: the hint names %s, but setting it gives model %q", f, flag, got)
		}

		cfg := &config.Config{}
		if !slices.Contains(configKeys, key) {
			t.Fatalf("%s: hint names %q, not a key prism config knows", f, key)
		}
		*testModelField(cfg, key) = "y"
		if got := resolveProbeModels("", "", "", cfg).forFormat(f); got != "y" {
			t.Errorf("%s: the hint names %s, but setting it gives model %q", f, key, got)
		}
	}
}

// The keys are the lever for a gateway that stops serving the default,
// so set must reach the probes and unset must restore the built-in.
func TestTestModelConfigKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := configSet("anthropic_test_model", "  claude-opus-5-5 "); err != nil {
		t.Fatal(err)
	}
	if err := configSet("openai_test_model", "gpt-6-luna"); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	got := resolveProbeModels("", "", "", cfg)
	if got.anthropic != "claude-opus-5-5" || got.openai != "gpt-6-luna" {
		t.Errorf("after set: %+v, want the configured models, trimmed", got)
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
	got = resolveProbeModels("", "", "", cfg)
	if got.anthropic != defaultAnthropicTestModel || got.openai != "" {
		t.Errorf("after unset: %+v, want the built-in defaults back", got)
	}
}
