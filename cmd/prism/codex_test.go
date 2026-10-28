package main

import (
	"slices"
	"strings"
	"testing"
)

func TestCodexConfigArgsPlacement(t *testing.T) {
	provider := codexProviderOverride(7331)
	overrides := []string{"-c", provider, "-c", "model_provider=prism"}

	for _, tt := range []struct {
		name string
		args []string
		want []string
	}{
		{
			// Nothing to land after: the root list is the only one.
			name: "no arguments",
			want: overrides,
		},
		{
			// codex 0.155 and older throw the root-level list away once
			// the subcommand has one of its own, so prism's overrides
			// have to join that list rather than sit above it.
			name: "after the subcommand",
			args: []string{"exec", "--skip-git-repo-check", "say hi"},
			want: append(append([]string{"exec"}, overrides...), "--skip-git-repo-check", "say hi"),
		},
		{
			// A bare prompt is a root-level positional; `-c` after it
			// still parses (checked on 0.145, 0.155 and 0.165).
			name: "after a bare prompt",
			args: []string{"explain this repo"},
			want: append([]string{"explain this repo"}, overrides...),
		},
		{
			name: "flags only, no subcommand",
			args: []string{"--model", "gpt-nonsense"},
			want: append([]string{"--model", "gpt-nonsense"}, overrides...),
		},
		{
			// The user's own -c value must not be mistaken for the
			// subcommand, or the overrides land above `exec` and 0.155
			// drops them.
			name: "a user override before the subcommand",
			args: []string{"-c", "model_reasoning_effort=high", "exec", "say hi"},
			want: append(append([]string{"-c", "model_reasoning_effort=high", "exec"}, overrides...), "say hi"),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := codexConfigArgs(7331, tt.args)
			if !slices.Equal(got, tt.want) {
				t.Errorf("codexConfigArgs() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

func TestCodexProviderOverride(t *testing.T) {
	override := codexProviderOverride(9999)
	if !strings.Contains(override, "http://127.0.0.1:9999/v1") {
		t.Errorf("base_url does not point at the router port: %q", override)
	}
	// prism's router has no WebSocket support, and codex's built-in
	// openai provider opens one to api.openai.com that no base URL
	// redirects. This is what keeps it on HTTPS.
	if !strings.Contains(override, "supports_websockets=false") {
		t.Errorf("websockets not disabled: %q", override)
	}
	if !strings.HasPrefix(override, "model_providers.prism={") {
		t.Errorf("not a model_providers override: %q", override)
	}
}

// The user's own `-c` has to be able to win, which is the escape hatch
// for anyone who wants codex pointed somewhere else. codex applies them
// in order and the last value for a key is the one used.
func TestCodexConfigArgsLetsUserOverridesFollow(t *testing.T) {
	args := codexConfigArgs(7331, []string{"exec", "-c", "model_provider=openai", "say hi"})
	theirs := slices.Index(args, "model_provider=openai")
	ours := slices.Index(args, "model_provider=prism")
	if theirs < 0 || ours < 0 || theirs < ours {
		t.Errorf("user override does not come last: %q", args)
	}
}
