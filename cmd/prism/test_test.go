package main

import "testing"

// One --model can't suit both backends, so the per-backend flags must win
// over it and each must only reach its own backend's probes.
func TestProbeModel(t *testing.T) {
	cases := []struct {
		format, model, anthropic, openai, want string
	}{
		{formatAnthropic, "", "", "", ""},
		{formatOpenAIResponses, "", "", "", ""},
		{formatAnthropic, "m", "", "", "m"},
		{formatOpenAICompletions, "m", "", "", "m"},
		{formatAnthropic, "m", "a", "o", "a"},
		{formatOpenAIResponses, "m", "a", "o", "o"},
		{formatOpenAICompletions, "", "a", "o", "o"},
		{formatAnthropic, "", "", "o", ""},
		{formatOpenAIResponses, "", "a", "", ""},
	}
	for _, c := range cases {
		got := probeModel(c.format, c.model, c.anthropic, c.openai)
		if got != c.want {
			t.Errorf("probeModel(%s, model=%q, anthropic=%q, openai=%q) = %q, want %q",
				c.format, c.model, c.anthropic, c.openai, got, c.want)
		}
	}
}

// The hint has to name a flag that actually feeds the probe that failed.
func TestModelFlagMatchesProbeModel(t *testing.T) {
	for _, f := range allFormats {
		var anthropic, openai string
		switch modelFlag(f) {
		case "--anthropic-model":
			anthropic = "x"
		case "--openai-model":
			openai = "x"
		default:
			t.Fatalf("modelFlag(%s) = %q, not a flag prism test has", f, modelFlag(f))
		}
		if got := probeModel(f, "", anthropic, openai); got != "x" {
			t.Errorf("%s: the hint names %s, but setting it gives model %q", f, modelFlag(f), got)
		}
	}
}
