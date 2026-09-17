package main

import (
	"slices"
	"strings"
	"testing"
)

// envValue returns the value of key in env, and whether it was present.
// Later entries win, matching how exec applies a duplicated variable.
func envValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if after, ok := strings.CutPrefix(kv, key+"="); ok {
			value, found = after, true
		}
	}
	return value, found
}

func TestToolEnvEndpoints(t *testing.T) {
	const port = 7331

	for _, tt := range []struct {
		name         string
		tool         string
		forwardProxy bool
		want         map[string]string // key -> value, "" means must be absent
	}{
		{
			name: "claude gets the base URL and no anthropic credential",
			tool: "claude",
			want: map[string]string{
				"ANTHROPIC_BASE_URL":   "http://127.0.0.1:7331",
				"OPENAI_BASE_URL":      "http://127.0.0.1:7331/v1",
				"OPENAI_API_KEY":       "teleport",
				"ANTHROPIC_API_KEY":    "",
				"ANTHROPIC_AUTH_TOKEN": "",
				"HTTPS_PROXY":          "",
			},
		},
		{
			name: "codex matches claude",
			tool: "codex",
			want: map[string]string{
				"ANTHROPIC_BASE_URL": "http://127.0.0.1:7331",
				"OPENAI_API_KEY":     "teleport",
				"ANTHROPIC_API_KEY":  "",
			},
		},
		{
			// OpenCode only registers a provider whose catalog env var is
			// set, so without a dummy key its Anthropic models vanish.
			name: "opencode also gets a dummy anthropic key",
			tool: "opencode",
			want: map[string]string{
				"ANTHROPIC_BASE_URL": "http://127.0.0.1:7331",
				"ANTHROPIC_API_KEY":  "teleport",
				"OPENAI_BASE_URL":    "http://127.0.0.1:7331/v1",
				"OPENAI_API_KEY":     "teleport",
			},
		},
		{
			// Pi routes through models.json, but its catalog refresh still
			// needs both providers to appear configured on a fresh install.
			name: "pi gets dummy keys for catalog bootstrap",
			tool: "pi",
			want: map[string]string{
				"ANTHROPIC_BASE_URL": "http://127.0.0.1:7331",
				"ANTHROPIC_API_KEY":  "teleport",
				"OPENAI_BASE_URL":    "http://127.0.0.1:7331/v1",
				"OPENAI_API_KEY":     "teleport",
			},
		},
		{
			name:         "claude in forward-proxy mode swaps base URL for a proxy",
			tool:         "claude",
			forwardProxy: true,
			want: map[string]string{
				"HTTPS_PROXY":         "http://127.0.0.1:7331",
				"NODE_EXTRA_CA_CERTS": "/tmp/ca.pem",
				"OPENAI_BASE_URL":     "http://127.0.0.1:7331/v1",
				"ANTHROPIC_BASE_URL":  "",
				"ANTHROPIC_API_KEY":   "",
			},
		},
		{
			// The forward-proxy gate is claude-only, so opencode keeps the
			// base URL even with the mode enabled.
			name:         "opencode ignores forward-proxy mode",
			tool:         "opencode",
			forwardProxy: false, // runToolWithPrism resolves this per tool
			want: map[string]string{
				"ANTHROPIC_BASE_URL": "http://127.0.0.1:7331",
				"ANTHROPIC_API_KEY":  "teleport",
				"HTTPS_PROXY":        "",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := toolEnv(tt.tool, port, tt.forwardProxy, "/tmp/ca.pem", nil)
			for key, want := range tt.want {
				got, found := envValue(env, key)
				if want == "" {
					if found {
						t.Errorf("%s = %q, want it unset", key, got)
					}
					continue
				}
				if !found {
					t.Errorf("%s is unset, want %q", key, want)
				} else if got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}

// Inherited endpoint and credential variables must not leak through: a
// real key in the caller's shell would otherwise be forwarded to the
// gateway, which rejects it.
func TestToolEnvStripsInheritedVars(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-real",
		"ANTHROPIC_BASE_URL=https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN=oauth-token",
		"OPENAI_BASE_URL=https://api.openai.com/v1",
		"OPENAI_API_KEY=sk-openai-real",
		"HOME=/home/gus",
	}

	env := toolEnv("claude", 7331, false, "", environ)

	for _, unwanted := range []string{
		"ANTHROPIC_API_KEY=sk-ant-real",
		"ANTHROPIC_BASE_URL=https://api.anthropic.com",
		"ANTHROPIC_AUTH_TOKEN=oauth-token",
		"OPENAI_BASE_URL=https://api.openai.com/v1",
		"OPENAI_API_KEY=sk-openai-real",
	} {
		if slices.Contains(env, unwanted) {
			t.Errorf("inherited %q survived", unwanted)
		}
	}
	if _, found := envValue(env, "ANTHROPIC_AUTH_TOKEN"); found {
		t.Error("ANTHROPIC_AUTH_TOKEN should be dropped entirely, not replaced")
	}

	// Unrelated variables are preserved.
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/gus"} {
		if !slices.Contains(env, want) {
			t.Errorf("%q was dropped", want)
		}
	}
}

// Only forward-proxy mode may clear an inherited proxy: otherwise a
// user's corporate HTTPS_PROXY has to keep working.
func TestToolEnvPreservesInheritedProxyOutsideForwardMode(t *testing.T) {
	environ := []string{"HTTPS_PROXY=http://corp:3128", "NODE_EXTRA_CA_CERTS=/corp/ca.pem"}

	kept := toolEnv("opencode", 7331, false, "", environ)
	if got, _ := envValue(kept, "HTTPS_PROXY"); got != "http://corp:3128" {
		t.Errorf("HTTPS_PROXY = %q, want the inherited corporate proxy", got)
	}
	if got, _ := envValue(kept, "NODE_EXTRA_CA_CERTS"); got != "/corp/ca.pem" {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q, want the inherited value", got)
	}

	replaced := toolEnv("claude", 7331, true, "/tmp/ca.pem", environ)
	if got, _ := envValue(replaced, "HTTPS_PROXY"); got != "http://127.0.0.1:7331" {
		t.Errorf("HTTPS_PROXY = %q, want prism's proxy", got)
	}
	if got, _ := envValue(replaced, "NODE_EXTRA_CA_CERTS"); got != "/tmp/ca.pem" {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q, want prism's CA", got)
	}
}
