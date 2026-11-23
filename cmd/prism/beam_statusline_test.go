package main

import (
	"testing"

	"github.com/webvictim/prism/internal/beam"
	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
)

func TestClaudeCustomHeaders(t *testing.T) {
	h := claudeCustomHeaders("X-Prism-Workspace: /Users/me/repo\nX-Other: a:b\n\n  bogus\n" + egress.BeamHeader + ": off")
	if h.Get(egress.WorkspaceHeader) != "/Users/me/repo" || h.Get("X-Other") != "a:b" || h.Get(egress.BeamHeader) != "off" || len(h) != 3 {
		t.Errorf("parsed %v", h)
	}
}

func TestDescribeSessionBeam(t *testing.T) {
	for _, tc := range []struct {
		st   beam.SessionStatus
		want string
	}{
		{beam.SessionStatus{Mode: config.BeamRequired, Beam: &beam.Record{Alias: "quiet-otter"}}, "beam: quiet-otter"},
		{beam.SessionStatus{Mode: config.BeamRequired, Starting: true}, "beam: starting…"},
		{beam.SessionStatus{Mode: config.BeamOff}, "unrecorded"},
		{beam.SessionStatus{Mode: config.BeamRequired}, "beam: none yet"},
	} {
		if got := describeSessionBeam(tc.st); got != tc.want {
			t.Errorf("%+v = %q, want %q", tc.st, got, tc.want)
		}
	}
}

func TestStatuslineBeamNeedsASession(t *testing.T) {
	env := func(string) string { return "" }
	if got := statuslineBeam("", env); got != "" {
		t.Errorf("no session = %q", got)
	}
	off := func(k string) string {
		if k == "ANTHROPIC_CUSTOM_HEADERS" {
			return egress.BeamHeader + ": " + egress.BeamOff
		}
		return ""
	}
	if got := statuslineBeam("s1", off); got != "unrecorded (--no-beam)" {
		t.Errorf("--no-beam = %q", got)
	}
}
