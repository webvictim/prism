package beam

import (
	"net/http"

	"github.com/webvictim/prism/internal/egress"
)

// Scope decides how requests are divided between beams.
const (
	// ScopeSession gives every agent session a beam of its own, so each
	// recording (and each PR's evidence) holds one session's work. The
	// default.
	ScopeSession = "session"
	// ScopeRepo gives every repository one beam, shared by its sessions.
	ScopeRepo = "repo"
)

// Lane keys that aren't a session's or a repository's.
const (
	// sharedLane carries requests with neither a session nor a workspace
	// header, such as Pi's Messages traffic when affinity is disabled.
	sharedLane = "shared"
	// manualLane is the beam `prism beam start` starts in off mode, which
	// carries everything until `prism beam end`.
	manualLane = "manual"
	// standbyLane is a beam a launcher warmed before its session had an
	// id; the first new session to need a beam claims it.
	standbyLane = "standby"
)

// requestKey is what a request says about its origin: the agent session,
// as each harness names it, and the repository prism's launcher tagged.
type requestKey struct {
	Session   string
	Workspace string
}

// keyOf reads a request's session and workspace from the headers each
// harness sends. This picks a beam, independently of how Beams replay
// groups conversations within a recording:
//
//	Claude Code  X-Claude-Code-Session-Id, on every request, subagents too
//	Codex        Session-Id (= its thread id); a subagent's
//	             X-Codex-Parent-Thread-Id names its parent's
//	OpenCode     X-Opencode-Session-Id, and X-Opencode-Parent-Session-Id
//	             on a subagent's; X-Opencode-Project names the project
//	Pi           Session-Id on Responses (session_id in Pi 1.5.0);
//	             X-Session-Affinity or X-Session-Id on Messages with
//	             affinity enabled
//
// A subagent is keyed by its parent, so it shares the parent's beam.
func keyOf(h http.Header) requestKey {
	k := requestKey{Workspace: h.Get(egress.WorkspaceHeader)}
	if k.Workspace == "" {
		if p := h.Get("X-Opencode-Project"); p != "" {
			k.Workspace = "opencode-project:" + p
		}
	}
	for _, c := range []struct{ header, prefix string }{
		{"X-Claude-Code-Session-Id", "claude:"},
		{"X-Opencode-Parent-Session-Id", "opencode:"},
		{"X-Opencode-Session-Id", "opencode:"},
		{"X-Codex-Parent-Thread-Id", "session:"},
		{"Session-Id", "session:"},
		{"session_id", "session:"}, // Pi 1.5.0; keep the same lane key as Session-Id.
		{"X-Session-Id", "session:"},
		{"X-Session-Affinity", "session:"},
	} {
		if v := h.Get(c.header); v != "" {
			k.Session = c.prefix + v
			break
		}
	}
	return k
}

// lane is the beam a request with key k belongs to, by scope.
func (k requestKey) lane(scope string) string {
	if scope != ScopeRepo && k.Session != "" {
		return k.Session
	}
	if k.Workspace != "" {
		return "repo:" + k.Workspace
	}
	if k.Session != "" {
		return k.Session
	}
	return sharedLane
}
