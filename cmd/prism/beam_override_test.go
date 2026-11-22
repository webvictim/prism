package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/egress"
)

func TestTakeBeamOverride(t *testing.T) {
	for _, tc := range []struct {
		in       []string
		override string
		rest     []string
		err      bool
	}{
		{[]string{"claude", "-p", "hi"}, "", []string{"claude", "-p", "hi"}, false},
		{[]string{"--no-beam", "claude", "-p", "hi"}, egress.BeamOff, []string{"claude", "-p", "hi"}, false},
		{[]string{"--beam", "codex", "exec", "hi"}, egress.BeamRequired, []string{"codex", "exec", "hi"}, false},
		{[]string{"-no-beam", "-no-beam", "claude"}, egress.BeamOff, []string{"claude"}, false},
		{[]string{"--beam", "--no-beam", "claude"}, "", nil, true},
		// After the tool, it is the tool's argument, not prism's.
		{[]string{"claude", "--no-beam"}, "", []string{"claude", "--no-beam"}, false},
	} {
		o, rest, err := takeBeamOverride(tc.in)
		if (err != nil) != tc.err || o != tc.override || !slices.Equal(rest, tc.rest) {
			t.Errorf("%v: got %q %v %v", tc.in, o, rest, err)
		}
	}
}

func TestBeamOverrideOnlyForToolsThatCarryIt(t *testing.T) {
	for _, cmd := range []string{"claude", "codex"} {
		if err := checkBeamOverride(egress.BeamOff, cmd); err != nil {
			t.Errorf("%s refused: %v", cmd, err)
		}
	}
	for _, cmd := range []string{"exec", "opencode", "pi", "up", "env"} {
		err := checkBeamOverride(egress.BeamOff, cmd)
		if err == nil || !strings.Contains(err.Error(), "--no-beam works only with `prism claude` and `prism codex`") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	if err := checkBeamOverride(egress.BeamRequired, "pi"); err == nil || !strings.HasPrefix(err.Error(), "--beam ") {
		t.Errorf("--beam with pi: %v", err)
	}
	if err := checkBeamOverride("", "pi"); err != nil {
		t.Errorf("no override: %v", err)
	}
}

func TestBeamOverrideReachesBothTools(t *testing.T) {
	env := withClaudeHeader(withClaudeWorkspaceHeader([]string{"ANTHROPIC_CUSTOM_HEADERS=X-Mine: yes"}, "/repo"),
		egress.BeamHeader, egress.BeamOff)
	want := "ANTHROPIC_CUSTOM_HEADERS=X-Mine: yes\nX-Prism-Workspace: /repo\nX-Prism-Beam: off"
	if !slices.Contains(env, want) {
		t.Errorf("claude env: %q", env)
	}
	if env := withClaudeHeader([]string{"A=1"}, egress.BeamHeader, ""); !slices.Equal(env, []string{"A=1"}) {
		t.Errorf("no override must add nothing: %q", env)
	}
	got := codexProviderOverrideWith(7331, "/repo", egress.BeamRequired)
	if !strings.Contains(got, `http_headers={"X-Prism-Workspace"="/repo","X-Prism-Beam"="required"}`) {
		t.Errorf("codex: %s", got)
	}
	if got := codexProviderOverrideWith(7331, "", egress.BeamOff); !strings.Contains(got, `http_headers={"X-Prism-Beam"="off"}`) {
		t.Errorf("codex without a workspace: %s", got)
	}
}
