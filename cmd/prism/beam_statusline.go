package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/beam"
	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
	"github.com/webvictim/prism/internal/state"
)

// statuslineBeamTimeout bounds asking the daemon which beam a session
// uses. The daemon answers from memory, so anything slower is a daemon
// in trouble, and the quota still deserves its answer.
const statuslineBeamTimeout = 2 * time.Second

// statuslineBeam is what statusline's account label says about the beam
// recording a Claude Code session, in place of the tenant: "beam: quiet-otter",
// or "" when there is nothing to say (no session, no daemon, a daemon
// that predates the question, inside a beam).
//
// statusline runs prism with Claude Code's environment, so the headers
// the launcher handed Claude Code in ANTHROPIC_CUSTOM_HEADERS — the
// repository and a `prism --beam`/`--no-beam` override — are here too,
// and the daemon is asked with exactly the headers the session's
// requests carry: its answer is the beam they are routed to.
func statuslineBeam(sessionID string, getenv func(string) string) string {
	if sessionID == "" {
		return ""
	}
	h := claudeCustomHeaders(getenv("ANTHROPIC_CUSTOM_HEADERS"))
	if h.Get(egress.BeamHeader) == egress.BeamOff {
		return "unrecorded (--no-beam)"
	}
	s, err := state.Load()
	if err != nil || s == nil || s.LocalPort == 0 {
		return ""
	}
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s/session", s.LocalPort, beam.ControlPath), nil)
	if err != nil {
		return ""
	}
	req.Header = h
	req.Header.Set("X-Claude-Code-Session-Id", sessionID)
	resp, err := (&http.Client{Timeout: statuslineBeamTimeout}).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "" // a daemon from before /_prism/beam/session
	}
	var st beam.SessionStatus
	if json.NewDecoder(resp.Body).Decode(&st) != nil {
		return ""
	}
	return describeSessionBeam(st)
}

// describeSessionBeam is the label's beam part for a daemon's answer.
func describeSessionBeam(st beam.SessionStatus) string {
	switch {
	case st.Beam != nil:
		return "beam: " + st.Beam.Alias
	case st.Starting:
		return "beam: starting…"
	case st.Mode == config.BeamOff:
		return "unrecorded"
	default:
		// The lane's beam starts on its first request.
		return "beam: none yet"
	}
}

// claudeCustomHeaders parses ANTHROPIC_CUSTOM_HEADERS: "Name: value"
// lines, as withClaudeHeader writes them.
func claudeCustomHeaders(v string) http.Header {
	h := http.Header{}
	for line := range strings.SplitSeq(v, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if name = strings.TrimSpace(name); ok && name != "" {
			h.Set(name, strings.TrimSpace(value))
		}
	}
	return h
}
