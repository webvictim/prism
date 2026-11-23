package beam

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
)

// Session answers with the beam a session's requests are routed to,
// following a join, and never starts one.
func TestSessionReportsTheSessionsBeam(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	if st := r.m.Session(claude(t, "s1", "/repo/a").Header); st.Beam != nil || st.Starting || st.Lane != "claude:s1" {
		t.Fatalf("before any request = %+v", st)
	}
	if r.added() != 0 {
		t.Fatal("Session started a beam")
	}
	r.acquire(t, claude(t, "s1", "/repo/a"))
	r.acquire(t, claude(t, "s2", "/repo/b"))
	st := r.m.Session(claude(t, "s1", "/repo/a").Header)
	if st.Beam == nil || st.Beam.Key != "claude:s1" {
		t.Fatalf("s1 = %+v", st)
	}
	if other := r.m.Session(claude(t, "s2", "/repo/b").Header); other.Beam == nil || other.Beam.UUID == st.Beam.UUID {
		t.Fatalf("s2 = %+v; want its own beam", other)
	}

	// At the cap, a new session of the same repository joins s1's beam.
	r.max = 2
	r.acquire(t, claude(t, "s3", "/repo/a"))
	if j := r.m.Session(claude(t, "s3", "/repo/a").Header); j.Beam == nil || j.Beam.UUID != st.Beam.UUID {
		t.Fatalf("joined s3 = %+v; want s1's beam", j)
	}
}

func TestSessionInOffMode(t *testing.T) {
	r := newRig(t, config.BeamOff, nil)
	if st := r.m.Session(claude(t, "s1", "/repo/a").Header); st.Mode != config.BeamOff || st.Beam != nil {
		t.Fatalf("off, no beam = %+v", st)
	}
	// A `prism --beam` launch is required mode whatever beam_mode says.
	forced := forcedClaude(t, "s1", "/repo/a")
	r.acquire(t, forced)
	if st := r.m.Session(forced.Header); st.Mode != config.BeamRequired || st.Beam == nil {
		t.Fatalf("--beam = %+v", st)
	}
}

func TestSessionEndpointIsReadOnly(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	h := r.m.Handler()
	rq := httptest.NewRequest(http.MethodGet, ControlPath+"/session", nil)
	rq.Header.Set("X-Claude-Code-Session-Id", "s1")
	rq.Header.Set(egress.WorkspaceHeader, "/repo/a")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, rq)
	var st SessionStatus
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Lane != "claude:s1" {
		t.Fatalf("GET = %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, ControlPath+"/session", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d", w.Code)
	}
}
