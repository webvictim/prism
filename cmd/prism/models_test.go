package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/tshwrap"
)

// Beams lists each model twice: by its request name with the provider
// model it maps to, and again as the bare provider name. One line each,
// with the alias folded in.
func TestModelRowsFoldAliases(t *testing.T) {
	rows := modelRows([]tshwrap.InferenceModel{
		{Name: "claude-sonnet-5", ProviderName: "anthropic.claude-sonnet-5"},
		{Name: "anthropic.claude-sonnet-5"},
		{Name: "fast"}, // an alias of nothing listed still shows
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if rows[0].name != "claude-sonnet-5" || rows[0].detail != "or anthropic.claude-sonnet-5" {
		t.Errorf("first row = %+v", rows[0])
	}
	// With no alias listed, the provider name is only where it ends up.
	if r := modelRows([]tshwrap.InferenceModel{{Name: "fast", ProviderName: "provider.big"}}); r[0].detail != "→ provider.big" {
		t.Errorf("unaliased row = %+v", r[0])
	}
	if rows[1].name != "fast" {
		t.Errorf("second row = %+v, want the standalone alias", rows[1])
	}
}

func TestWriteModels(t *testing.T) {
	r := modelsReport{ListedAs: listedAsTbot, Discovery: true, Apps: []modelApp{
		{Name: "anthropic", Format: "anthropic", Provider: "bedrock",
			Models: []tshwrap.InferenceModel{{Name: "claude-sonnet-5", ProviderName: "anthropic.claude-sonnet-5"}}},
		{Name: "openai", Format: "openai"},
	}}
	r.Probe.Anthropic = probeEntry{"claude-sonnet-5", string(sourceCluster)}
	r.Probe.OpenAI = probeEntry{"gpt-6-luna", string(sourceBuiltin)}
	var buf bytes.Buffer
	writeModels(&buf, r)
	out := buf.String()
	for _, want := range []string{
		"anthropic  (anthropic, via bedrock)",
		"claude-sonnet-5  → anthropic.claude-sonnet-5",
		"no model list: the gateway passes any model name through",
		"anthropic  claude-sonnet-5  ",
		"(from the cluster's list)",
		"(prism's built-in: the cluster lists no models for this app)",
		"Listed as prism's tbot identity.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
