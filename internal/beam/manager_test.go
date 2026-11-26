package beam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
	"github.com/webvictim/prism/internal/tshwrap"
	"github.com/webvictim/prism/internal/tunnel"
)

// fakeCluster stands in for tsh: beams it has started, and what was asked.
type fakeCluster struct {
	mu        sync.Mutex
	now       func() time.Time
	beams     map[string]tshwrap.Beam
	n         int
	added     []string
	removed   []string
	addErr    error
	connErr   error
	listErr   error
	conns     []*fakeConn
	lifetime  time.Duration
	forwarded []Forward
	// loginUntil is when the user's login expires; zero means none.
	loginUntil time.Time
	logins     int
	autoLogins int
	loginErr   error
	linkLogins int
	finishLink chan struct{}
}

func newFakeCluster(now func() time.Time) *fakeCluster {
	return &fakeCluster{now: now, beams: map[string]tshwrap.Beam{}, lifetime: 11 * time.Hour,
		loginUntil: now().Add(30 * time.Hour), finishLink: make(chan struct{})}
}

func (c *fakeCluster) Login() (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logins++
	if c.loginUntil.IsZero() {
		return time.Time{}, tshwrap.ErrNoLogin
	}
	return c.loginUntil, nil
}

func (c *fakeCluster) LogIn(ctx context.Context, openBrowser bool, onLink func(string)) error {
	c.mu.Lock()
	if openBrowser {
		defer c.mu.Unlock()
		c.autoLogins++
		if c.loginErr != nil {
			return c.loginErr
		}
		c.loginUntil = c.now().Add(30 * time.Hour)
		return nil
	}
	c.linkLogins++
	finish := c.finishLink
	c.mu.Unlock()
	onLink("http://127.0.0.1:5555/link")
	select {
	case <-finish:
		c.setLogin(c.now().Add(30 * time.Hour))
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *fakeCluster) setLogin(until time.Time) {
	c.mu.Lock()
	c.loginUntil = until
	c.mu.Unlock()
}

func (c *fakeCluster) Add(context.Context) (*tshwrap.Beam, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.addErr != nil {
		return nil, c.addErr
	}
	c.n++
	b := tshwrap.Beam{ID: fmt.Sprintf("beam-%d", c.n), UUID: fmt.Sprintf("uuid-%d", c.n), Expires: c.now().Add(c.lifetime)}
	c.beams[b.UUID] = b
	c.added = append(c.added, b.UUID)
	return &b, nil
}

func (c *fakeCluster) List(context.Context) ([]tshwrap.Beam, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listErr != nil {
		return nil, c.listErr
	}
	var out []tshwrap.Beam
	for _, b := range c.beams {
		out = append(out, b)
	}
	return out, nil
}

func (c *fakeCluster) Remove(_ context.Context, uuid string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.beams, uuid)
	c.removed = append(c.removed, uuid)
	return nil
}

func (c *fakeCluster) AppHosts(context.Context) (string, string, error) {
	return "anthropic.example.com", "openai.example.com", nil
}

func (c *fakeCluster) Connect(_ context.Context, f Forward) (Connection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connErr != nil {
		return nil, c.connErr
	}
	fc := &fakeConn{uuid: f.UUID}
	c.conns = append(c.conns, fc)
	c.forwarded = append(c.forwarded, f)
	return fc, nil
}

func (c *fakeCluster) snapshot() (added, removed []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.added...), append([]string(nil), c.removed...)
}

type fakeConn struct {
	uuid   string
	mu     sync.Mutex
	closed int
}

func (f *fakeConn) Close() {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
}

func (f *fakeConn) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

type memStore struct {
	mu sync.Mutex
	f  File
}

func (s *memStore) Load() (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.f
	c.Beams = append([]Record(nil), s.f.Beams...)
	return &c, nil
}

func (s *memStore) Save(f *File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.f = *f
	s.f.Beams = append([]Record(nil), f.Beams...)
	return nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type rig struct {
	m       *Manager
	sw      *egress.Switch
	cluster *fakeCluster
	store   *memStore
	clock   *clock
	logs    *strings.Builder
	mode    *syncString
	notes   *notes
	// max and scope are what Config.Max and Config.Scope return; set them
	// before the Manager is used.
	max   int
	scope string
}

type syncString struct {
	mu sync.Mutex
	v  string
}

func (s *syncString) get() string  { s.mu.Lock(); defer s.mu.Unlock(); return s.v }
func (s *syncString) set(v string) { s.mu.Lock(); s.v = v; s.mu.Unlock() }

type notes struct {
	mu   sync.Mutex
	msgs []string
}

func (n *notes) add(m string) { n.mu.Lock(); n.msgs = append(n.msgs, m); n.mu.Unlock() }
func (n *notes) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.msgs...)
}

func newRig(t *testing.T, mode string, store *memStore) *rig {
	t.Helper()
	clk := &clock{t: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)}
	cluster := newFakeCluster(clk.now)
	if store == nil {
		store = &memStore{}
	}
	sw := egress.NewSwitch(nil)
	logs := &strings.Builder{}
	ms := &syncString{v: mode}
	ns := &notes{}
	r := &rig{max: MaxBeams, scope: ScopeSession}
	m, err := New(Config{
		Switch:              sw,
		Logger:              log.New(&syncWriter{w: logs}, "", 0),
		AnthropicTunnelPort: 7333,
		OpenAITunnelPort:    7334,
		Proxy:               "odd-firefly.beams.sh:443",
		Mode:                ms.get,
		Max:                 func() int { return r.max },
		Scope:               func() string { return r.scope },
		Notify:              ns.add,
		Ops:                 cluster,
		Now:                 clk.now,
		Store:               store,
	})
	if err != nil {
		t.Fatal(err)
	}
	r.m, r.sw, r.cluster, r.store, r.clock, r.logs, r.mode, r.notes = m, sw, cluster, store, clk, logs, ms, ns
	return r
}

type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// carried makes a lane's beam look as if it had carried a request since
// the Manager last looked.
func (r *rig) carried(key string) {
	r.m.mu.Lock()
	r.m.lanes[key].lastServed--
	r.m.mu.Unlock()
}

// req is a request from an agent session in a repository.
func req(t *testing.T, headers ...string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7333/v1/messages", nil).WithContext(t.Context())
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	return r
}

// claude is a Claude Code session's request, tagged with its repository.
func claude(t *testing.T, sid, ws string) *http.Request {
	return req(t, "X-Claude-Code-Session-Id", sid, egress.WorkspaceHeader, ws)
}

func (r *rig) acquire(t *testing.T, rq *http.Request) *egress.Target {
	t.Helper()
	target, err := r.m.Acquire(rq)
	if err != nil || target == nil {
		t.Fatalf("Acquire = %v, %v", target, err)
	}
	return target
}

func (r *rig) added() int {
	a, _ := r.cluster.snapshot()
	return len(a)
}

func (r *rig) removed() int {
	_, rm := r.cluster.snapshot()
	return len(rm)
}

func (r *rig) step(t *testing.T) {
	t.Helper()
	last := r.clock.now()
	r.m.step(t.Context(), &last)
}

func TestEachSessionGetsItsOwnBeam(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	a := r.acquire(t, claude(t, "s1", "/repo/a"))
	b := r.acquire(t, claude(t, "s2", "/repo/a"))
	if a == b || r.added() != 2 {
		t.Fatalf("two sessions share a beam (added %d)", r.added())
	}
	if again := r.acquire(t, claude(t, "s1", "/repo/a")); again != a || r.added() != 2 {
		t.Error("a session's second request started another beam")
	}
	st := r.m.Status("/repo/a")
	if !st.Active || len(st.Beams) != 2 || st.Beam.Workspace != "/repo/a" {
		t.Errorf("status = %+v", st)
	}
	f, _ := r.store.Load()
	if len(f.Lanes) != 2 || f.Lanes["claude:s1"] == "" {
		t.Errorf("lanes not recorded: %+v", f.Lanes)
	}
	// The forwards are opened to the apps by name, so the beam resolves them.
	fwd := r.cluster.forwarded[0]
	if fwd.AnthropicHost != "anthropic.example.com" || fwd.OpenAIHost != "openai.example.com" ||
		fwd.AnthropicPort == fwd.OpenAIPort || fwd.AnthropicPort == 0 {
		t.Errorf("forward = %+v", fwd)
	}
}

func TestSubagentsShareTheirParentsBeam(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	codex := r.acquire(t, req(t, "Session-Id", "thread-1"))
	sub := r.acquire(t, req(t, "Session-Id", "thread-2", "X-Codex-Parent-Thread-Id", "thread-1"))
	if codex != sub {
		t.Error("a Codex subagent got a beam of its own")
	}
	oc := r.acquire(t, req(t, "X-Opencode-Session-Id", "ses_1", "X-Opencode-Project", "p"))
	ocSub := r.acquire(t, req(t, "X-Opencode-Session-Id", "ses_2", "X-Opencode-Parent-Session-Id", "ses_1"))
	if oc != ocSub {
		t.Error("an OpenCode subagent got a beam of its own")
	}
	if r.added() != 2 {
		t.Errorf("added %d beams, want 2", r.added())
	}
}

func TestPiResponsesSessionHeaderSpellingsShareABeam(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	response := func(header, id string) *http.Request {
		rq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7334/v1/responses", nil).WithContext(t.Context())
		rq.Header.Set(header, id)
		return rq
	}
	old := r.acquire(t, response("session_id", "pi-1"))
	if upgraded := r.acquire(t, response("session-id", "pi-1")); upgraded != old {
		t.Error("upgrading Pi changed the session's beam")
	}
	if other := r.acquire(t, response("session_id", "pi-2")); other == old {
		t.Error("two Pi 1.5.0 sessions share a beam")
	}
}

func TestPiMessagesAffinityRoutesBySession(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	a := r.acquire(t, req(t, "X-Session-Affinity", "pi-1"))
	if again := r.acquire(t, req(t, "X-Session-Affinity", "pi-1")); again != a {
		t.Error("same Pi Messages session did not reuse its beam")
	}
	if otherFormat := r.acquire(t, req(t, "X-Session-Id", "pi-1")); otherFormat != a {
		t.Error("OpenRouter-format affinity changed the session's beam")
	}
	responses := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:7334/v1/responses", nil).WithContext(t.Context())
	responses.Header.Set("Session-Id", "pi-1")
	if r.acquire(t, responses) != a {
		t.Error("switching Pi from Messages to Responses changed the session's beam")
	}
	if b := r.acquire(t, req(t, "X-Session-Affinity", "pi-2")); b == a || r.added() != 2 {
		t.Errorf("distinct Pi sessions did not get separate beams: added %d", r.added())
	}
}

func TestUntaggedTrafficSharesOneBeam(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	a := r.acquire(t, req(t, "User-Agent", "pi/1.6.0"))
	b := r.acquire(t, req(t, "User-Agent", "curl"))
	if a != b || r.lane(sharedLane) == nil {
		t.Error("requests with no session or repository didn't share the shared beam")
	}
}

func (r *rig) lane(key string) *session { return r.m.lane(key) }

func TestRepoScopeSharesABeamPerRepository(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.scope = ScopeRepo
	a := r.acquire(t, claude(t, "s1", "/repo/a"))
	b := r.acquire(t, claude(t, "s2", "/repo/a"))
	c := r.acquire(t, claude(t, "s3", "/repo/b"))
	if a != b || a == c || r.added() != 2 {
		t.Errorf("repo scope: same-repo shared=%v, other repo separate=%v, added %d", a == b, a != c, r.added())
	}
}

func TestAcquireSharesOneStart(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	var wg sync.WaitGroup
	got := make([]*egress.Target, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], _ = r.m.Acquire(claude(t, "s1", "/repo/a"))
		}()
	}
	wg.Wait()
	if r.added() != 1 {
		t.Fatalf("concurrent requests of one session started %d beams, want 1", r.added())
	}
	for i, g := range got {
		if g == nil || g != got[0] {
			t.Errorf("request %d got target %p, want %p", i, g, got[0])
		}
	}
}

func TestBeamMaxJoinsSameRepoThenReclaimsIdle(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.max = 2
	a := r.acquire(t, claude(t, "s1", "/repo/a"))
	r.acquire(t, claude(t, "s2", "/repo/b"))

	// At the limit, a session joins a beam of its own repository...
	if got := r.acquire(t, claude(t, "s3", "/repo/a")); got != a || r.added() != 2 {
		t.Fatalf("s3 didn't join /repo/a's beam (added %d)", r.added())
	}
	if again := r.acquire(t, claude(t, "s3", "/repo/a")); again != a {
		t.Error("the join wasn't remembered")
	}
	// ...and one from another repository, with nothing idle, is refused,
	// as a retryable 503 that says why.
	_, err := r.m.Acquire(claude(t, "s4", "/repo/c"))
	var refusal *egress.Refusal
	if !errors.As(err, &refusal) || refusal.Status != http.StatusServiceUnavailable || !strings.Contains(refusal.Message, "beam_max") {
		t.Fatalf("Acquire at the limit = %v", err)
	}

	// Once a beam has sat idle long enough, it makes room.
	r.clock.advance(idleToReclaim)
	r.clock.advance(retryFailedStart) // the refusal was remembered
	r.m.mu.Lock()
	r.m.failErr = nil
	r.m.mu.Unlock()
	r.acquire(t, claude(t, "s4", "/repo/c"))
	r.m.Wait()
	if r.added() != 3 || r.removed() != 1 {
		t.Errorf("added %d removed %d: want the idlest beam ended to make room", r.added(), r.removed())
	}
}

func TestStandbyIsClaimedByTheNextSession(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	if _, err := r.m.Start(t.Context(), "/repo/a"); err != nil {
		t.Fatal(err)
	}
	if !r.m.Status("").Standby {
		t.Fatal("no standby after Start")
	}
	if _, err := r.m.Start(t.Context(), "/repo/a"); err != nil || r.added() != 1 {
		t.Fatalf("a second Start warmed another beam (%v, added %d)", err, r.added())
	}
	r.acquire(t, claude(t, "s1", "/repo/a"))
	if r.added() != 1 {
		t.Errorf("the session started its own beam instead of claiming the standby")
	}
	st := r.m.Status("")
	if st.Standby || st.Beams[0].Beam.Key != "claude:s1" {
		t.Errorf("status after claim = %+v", st)
	}
	f, _ := r.store.Load()
	if f.Lanes["claude:s1"] == "" || f.Lanes[standbyLane] != "" {
		t.Errorf("lanes after claim = %v", f.Lanes)
	}
}

func TestOffModeUsesOnlyTheManualBeam(t *testing.T) {
	r := newRig(t, config.BeamOff, nil)
	if target, err := r.m.Acquire(claude(t, "s1", "/repo/a")); target != nil || err != nil {
		t.Fatalf("off mode with no beam = %v, %v; want the tunnels", target, err)
	}
	if r.added() != 0 {
		t.Fatal("off mode started a beam on its own")
	}
	if _, err := r.m.Start(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	a := r.acquire(t, claude(t, "s1", "/repo/a"))
	b := r.acquire(t, req(t))
	if a != b || r.added() != 1 {
		t.Error("off mode's manual beam doesn't carry everything")
	}
}

func TestEndByWorkspace(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.acquire(t, claude(t, "s1", "/repo/a"))
	r.acquire(t, claude(t, "s2", "/repo/a"))
	r.acquire(t, claude(t, "s3", "/repo/b"))
	ended, err := r.m.End(t.Context(), "/repo/a")
	if err != nil || len(ended) != 2 {
		t.Fatalf("End(/repo/a) = %v, %v", ended, err)
	}
	r.m.Wait()
	if r.removed() != 2 || len(r.m.Status("").Beams) != 1 {
		t.Errorf("removed %d, left %d", r.removed(), len(r.m.Status("").Beams))
	}
	if conn := r.cluster.conns[0]; conn.closes() != 1 {
		t.Errorf("forward closed %d times, want 1", conn.closes())
	}
	if ended, err := r.m.End(t.Context(), ""); err != nil || len(ended) != 1 {
		t.Fatalf("End(all) = %v, %v", ended, err)
	}
	r.m.Wait()
	if _, err := r.m.End(t.Context(), ""); !errors.Is(err, ErrNoBeam) {
		t.Errorf("End with no beam = %v, want ErrNoBeam", err)
	}
	f, _ := r.store.Load()
	if len(f.Lanes) != 0 || f.Find("uuid-1").Ended.IsZero() {
		t.Errorf("store after End = %+v", f)
	}
}

func TestRotationReplacesEachLaneBeforeExpiry(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	first := r.acquire(t, claude(t, "s1", "/repo/a"))
	for elapsed := time.Duration(0); elapsed < 11*time.Hour-RotateBefore+time.Minute; elapsed += 5 * time.Minute {
		r.carried("claude:s1")
		r.clock.advance(5 * time.Minute)
		r.step(t)
	}
	r.m.Wait()
	if r.added() != 2 || r.removed() != 1 {
		t.Fatalf("added %d removed %d: want one successor and the old beam removed", r.added(), r.removed())
	}
	next := r.acquire(t, claude(t, "s1", "/repo/a"))
	if next == first {
		t.Error("the session still goes through the old beam")
	}
	if rec := r.lane("claude:s1").rec; rec.Workspace != "/repo/a" || rec.UUID != "uuid-2" {
		t.Errorf("successor record = %+v", rec)
	}
}

func TestFailedRotationKeepsOldUntilExpiry(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.acquire(t, claude(t, "s1", "/repo/a"))
	r.cluster.addErr = errors.New("quota")
	r.clock.advance(11*time.Hour - time.Minute)
	r.carried("claude:s1")
	r.step(t)
	if r.lane("claude:s1") == nil {
		t.Fatal("old beam dropped while it still had a minute left")
	}
	if st := r.m.Status(""); !strings.Contains(st.LastError, "quota") {
		t.Errorf("last error = %q", st.LastError)
	}
	r.clock.advance(2 * time.Minute)
	r.step(t)
	r.m.Wait()
	if r.lane("claude:s1") != nil {
		t.Fatal("still routing to an expired beam")
	}
	if r.removed() != 0 {
		t.Errorf("an expired beam was removed")
	}
}

func TestLostBeamIsReplacedByTheNextRequest(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.acquire(t, claude(t, "s1", "/repo/a"))
	_ = r.cluster.Remove(t.Context(), "uuid-1") // removed out from under prism
	last := r.clock.now().Add(-verifyEvery)
	r.m.step(t.Context(), &last)
	r.m.Wait()
	if r.lane("claude:s1") != nil {
		t.Fatal("still routing to a removed beam")
	}
	r.acquire(t, claude(t, "s1", "/repo/a"))
	if r.lane("claude:s1").rec.UUID != "uuid-2" {
		t.Error("the next request didn't start a replacement")
	}
}

func TestAcquireWithoutLogin(t *testing.T) {
	for _, tt := range []struct {
		mode       string
		wantRefuse bool
	}{
		{config.BeamRequired, true},
		{config.BeamPreferred, false},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			r := newRig(t, tt.mode, nil)
			r.cluster.setLogin(time.Time{})
			for range 3 {
				target, err := r.m.Acquire(claude(t, "s1", "/repo/a"))
				if target != nil {
					t.Fatal("got a beam with no login")
				}
				var refusal *egress.Refusal
				if refused := errors.As(err, &refusal); refused != tt.wantRefuse {
					t.Fatalf("Acquire err = %v, want refusal %v", err, tt.wantRefuse)
				}
				if tt.wantRefuse && (!strings.Contains(refusal.Message, "`prism login`") ||
					!strings.Contains(refusal.Message, "http://127.0.0.1:5555/link")) {
					t.Errorf("refusal doesn't give the link and prism login: %q", refusal.Message)
				}
				if tt.wantRefuse && refusal.Status != http.StatusBadRequest {
					t.Errorf("refusal status = %d, want 400: a retry can't fix a missing login", refusal.Status)
				}
				if tt.wantRefuse && strings.Contains(refusal.Message, "--no-beam") {
					t.Errorf("refusal suggests skipping the beam: %q", refusal.Message)
				}
				if tt.wantRefuse && strings.Contains(refusal.Message, "beam_mode") {
					t.Errorf("refusal suggests going unrecorded: %q", refusal.Message)
				}
			}
			if r.added() != 0 {
				t.Errorf("tried to start beams with no login")
			}
			if n := r.notes.all(); len(n) != 1 || !strings.Contains(n[0], "prism login") {
				t.Errorf("notifications = %q, want one naming prism login", n)
			}
			r.cluster.setLogin(r.clock.now().Add(30 * time.Hour))
			r.clock.advance(loginCacheFor)
			r.acquire(t, claude(t, "s1", "/repo/a"))
		})
	}
}

func TestLinkLoginIsSharedAndFinishes(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.cluster.setLogin(time.Time{})
	for _, sid := range []string{"s1", "s2", "s3"} {
		if _, err := r.m.Acquire(claude(t, sid, "/repo/a")); err == nil {
			t.Fatal("served with no login")
		}
	}
	r.cluster.mu.Lock()
	links := r.cluster.linkLogins
	r.cluster.mu.Unlock()
	if links != 1 {
		t.Fatalf("started %d link logins for 3 refused requests, want 1", links)
	}
	close(r.cluster.finishLink) // the user opens the link
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.m.loginMu.Lock()
		done := r.m.pending == nil
		r.m.loginMu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the link login never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.acquire(t, claude(t, "s1", "/repo/a"))
}

func TestFailedStartIsRememberedBriefly(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.cluster.addErr = errors.New("quota exceeded")
	for range 3 {
		var refusal *egress.Refusal
		if _, err := r.m.Acquire(claude(t, "s1", "/repo/a")); !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "quota exceeded") {
			t.Fatalf("Acquire err = %v, want a refusal naming the cause", err)
		}
		if refusal.Status != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 so agents retry", refusal.Status)
		}
	}
	r.cluster.mu.Lock()
	r.cluster.addErr = nil
	r.cluster.mu.Unlock()
	if _, err := r.m.Acquire(claude(t, "s1", "/repo/a")); err == nil {
		t.Fatal("retried within retryFailedStart")
	}
	r.clock.advance(retryFailedStart)
	r.acquire(t, claude(t, "s1", "/repo/a"))
}

func TestAutoLoginHoldsRequestUntilBeam(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.m.cfg.AutoLogin = func() bool { return true }
	r.cluster.setLogin(time.Time{})
	r.acquire(t, claude(t, "s1", "/repo/a"))
	if r.cluster.autoLogins != 1 {
		t.Errorf("logged in %d times, want 1", r.cluster.autoLogins)
	}
}

func TestAutoLoginFailureIsntRetriedPerRequest(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.m.cfg.AutoLogin = func() bool { return true }
	r.cluster.setLogin(time.Time{})
	r.cluster.loginErr = errors.New("browser closed")
	for range 5 {
		var refusal *egress.Refusal
		if _, err := r.m.Acquire(claude(t, "s1", "/repo/a")); !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "prism login") {
			t.Fatalf("Acquire = %v, want a refusal naming prism login", err)
		}
		r.clock.advance(retryFailedStart)
	}
	if r.cluster.autoLogins != 1 {
		t.Errorf("ran %d logins, want 1: each opens a browser tab", r.cluster.autoLogins)
	}
	r.clock.advance(autoLoginEvery)
	_, _ = r.m.Acquire(claude(t, "s1", "/repo/a"))
	if r.cluster.autoLogins != 2 {
		t.Errorf("no new attempt after autoLoginEvery: %d", r.cluster.autoLogins)
	}
}

func TestIdleBeamIsEnded(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.acquire(t, claude(t, "s1", "/repo/a"))
	r.clock.advance(IdleEnd - time.Minute)
	r.step(t)
	if r.lane("claude:s1") == nil {
		t.Fatal("ended before IdleEnd")
	}
	r.clock.advance(2 * time.Minute)
	r.step(t)
	r.m.Wait()
	if r.lane("claude:s1") != nil || r.removed() != 1 {
		t.Fatal("an idle beam was kept, or not removed (so not summarised)")
	}

	// A beam started by hand in off mode is its owner's to end.
	off := newRig(t, config.BeamOff, nil)
	if _, err := off.m.Start(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	off.clock.advance(IdleEnd + time.Hour)
	off.step(t)
	if off.lane(manualLane) == nil {
		t.Error("off mode ended a beam the user started")
	}
}

func TestIdleBeamIsEndedRatherThanReplaced(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.m.cfg.RotateBefore = 3 * time.Hour // due for replacement inside IdleEnd's reach
	r.acquire(t, claude(t, "s1", "/repo/a"))
	// One look, 8h on: due for replacement, and unused all that time.
	r.clock.advance(8*time.Hour + time.Minute)
	r.step(t)
	r.m.Wait()
	if r.added() != 1 || r.removed() != 1 {
		t.Errorf("added %d, removed %d: want the idle beam ended, not replaced", r.added(), r.removed())
	}
}

func TestLoginExpiryWarnedOnceAcrossBeams(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.acquire(t, claude(t, "s1", "/repo/a"))
	r.acquire(t, claude(t, "s2", "/repo/b"))
	replaceAt := r.clock.now().Add(11*time.Hour - RotateBefore)
	r.cluster.setLogin(replaceAt.Add(-time.Hour))
	for r.clock.now().Before(replaceAt.Add(-2 * time.Minute)) {
		r.carried("claude:s1")
		r.carried("claude:s2")
		r.clock.advance(5 * time.Minute)
		last := r.clock.now().Add(-verifyEvery)
		r.m.step(t.Context(), &last)
	}
	n := r.notes.all()
	if len(n) != 1 || !strings.Contains(n[0], "prism login") {
		t.Fatalf("notifications = %q, want exactly one warning naming the fix", n)
	}
}

func TestCreateRemovesBeamWhenForwardFails(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.cluster.connErr = errors.New("ssh refused")
	if _, err := r.m.Acquire(claude(t, "s1", "/repo/a")); err == nil {
		t.Fatal("served without a forward")
	}
	if r.lane("claude:s1") != nil || r.removed() != 1 {
		t.Errorf("half-made beam left behind: removed %d", r.removed())
	}
}

func TestReattachResumesEveryLane(t *testing.T) {
	first := newRig(t, config.BeamRequired, nil)
	first.acquire(t, claude(t, "s1", "/repo/a"))
	first.acquire(t, claude(t, "s2", "/repo/b"))
	first.m.shutdown()
	if first.removed() != 0 {
		t.Fatal("shutdown removed beams")
	}
	first.store.mu.Lock()
	first.store.f.Beams[0].Requests = 26
	first.store.mu.Unlock()

	second := newRig(t, config.BeamRequired, first.store)
	second.cluster = first.cluster
	second.m.cfg.Ops = first.cluster
	second.m.reattach(t.Context())
	if st := second.m.Status(""); len(st.Beams) != 2 {
		t.Fatalf("resumed %d lanes, want 2: %+v", len(st.Beams), st)
	}
	if s := second.lane("claude:s1"); s == nil || s.requests() != 26 || s.rec.Workspace != "/repo/a" {
		t.Errorf("resumed lane = %+v", s)
	}
	if first.added() != 2 {
		t.Errorf("reattach created beams")
	}
}

func TestReattachForgetsVanishedBeam(t *testing.T) {
	store := &memStore{f: File{Lanes: map[string]string{"claude:s1": "gone"}, Beams: []Record{{UUID: "gone", Alias: "old"}}}}
	r := newRig(t, config.BeamRequired, store)
	r.m.reattach(t.Context())
	if r.lane("claude:s1") != nil {
		t.Fatal("resumed a beam that doesn't exist")
	}
	f, _ := store.Load()
	if len(f.Lanes) != 0 || f.Find("gone").Ended.IsZero() {
		t.Errorf("store = %+v", f)
	}
}

func TestPeekNeverStarts(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	if r.m.Peek(claude(t, "s1", "/repo/a")) != nil || r.added() != 0 {
		t.Fatal("Peek started a beam")
	}
	target := r.acquire(t, claude(t, "s1", "/repo/a"))
	if r.m.Peek(claude(t, "s1", "/repo/a")) != target {
		t.Error("Peek didn't find the running beam")
	}
}

func TestKeyOf(t *testing.T) {
	for _, tt := range []struct {
		headers []string
		scope   string
		want    string
	}{
		{[]string{"X-Claude-Code-Session-Id", "c1", egress.WorkspaceHeader, "/r"}, ScopeSession, "claude:c1"},
		{[]string{"X-Claude-Code-Session-Id", "c1", egress.WorkspaceHeader, "/r"}, ScopeRepo, "repo:/r"},
		{[]string{"Session-Id", "t1"}, ScopeRepo, "session:t1"},
		{[]string{"session_id", "old", "Session-Id", "new"}, ScopeSession, "session:new"},
		{[]string{"session_id", "child", "X-Codex-Parent-Thread-Id", "parent"}, ScopeSession, "session:parent"},
		{[]string{"X-Session-Affinity", "pi1", "X-Session-Id", "pi2", "Session-Id", "t1"}, ScopeSession, "session:t1"},
		{[]string{"X-Session-Affinity", "pi1", "X-Session-Id", "pi2", "session_id", "old"}, ScopeSession, "session:old"},
		{[]string{"X-Session-Affinity", "pi1", "X-Codex-Parent-Thread-Id", "parent"}, ScopeSession, "session:parent"},
		{[]string{"X-Session-Id", "pi2", "X-Opencode-Parent-Session-Id", "parent"}, ScopeSession, "opencode:parent"},
		{[]string{"X-Session-Affinity", "pi1", egress.WorkspaceHeader, "/r"}, ScopeRepo, "repo:/r"},
		{[]string{"X-Opencode-Session-Id", "o1", "X-Opencode-Project", "p"}, ScopeRepo, "repo:opencode-project:p"},
		{[]string{"User-Agent", "pi"}, ScopeSession, sharedLane},
		{[]string{egress.WorkspaceHeader, "/r"}, ScopeSession, "repo:/r"},
	} {
		r := req(t, tt.headers...)
		if got := keyOf(r.Header, r.URL.Path).lane(tt.scope); got != tt.want {
			t.Errorf("%v (%s) = %q, want %q", tt.headers, tt.scope, got, tt.want)
		}
	}
}

// The affinity headers aren't any one harness's, so off Messages they
// mustn't pick a beam: a client sending a fresh value per request would
// start a beam per request.
func TestAffinityHeadersCountOnMessagesOnly(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/v1/embeddings"} {
		for _, h := range []string{"X-Session-Affinity", "X-Session-Id"} {
			hdr := http.Header{}
			hdr.Set(h, "per-request")
			if got := keyOf(hdr, path).lane(ScopeSession); got != sharedLane {
				t.Errorf("%s on %s = %q, want %q", h, path, got, sharedLane)
			}
			hdr.Set(egress.WorkspaceHeader, "/r")
			if got := keyOf(hdr, path).lane(ScopeSession); got != "repo:/r" {
				t.Errorf("%s on %s with a workspace = %q, want the repository's", h, path, got)
			}
		}
	}
	hdr := http.Header{}
	hdr.Set("X-Session-Affinity", "pi-1")
	if got := keyOf(hdr, messagesPath).lane(ScopeSession); got != "session:pi-1" {
		t.Errorf("on Messages = %q", got)
	}
}

func TestForwardArgs(t *testing.T) {
	got := strings.Join(Forward{
		UUID: "1510d6b9", AnthropicHost: "anthropic.c", OpenAIHost: "openai.c",
		AnthropicPort: 17400, OpenAIPort: 17401,
	}.Args(), " ")
	want := "ssh -N --no-relogin -L 127.0.0.1:17400:anthropic.c:443 -L 127.0.0.1:17401:openai.c:443 beams@1510d6b9"
	if !strings.HasSuffix(got, want) {
		t.Errorf("args = %q, want suffix %q", got, want)
	}
}

func TestRecordLink(t *testing.T) {
	r := Record{UUID: "u", Proxy: "odd-firefly.beams.sh:443"}
	if got := r.Link(""); got != "https://odd-firefly.beams.sh/web/cluster/odd-firefly.beams.sh/session/u?recordingType=beam" {
		t.Errorf("link = %s", got)
	}
	if got := r.Link("leaf"); !strings.Contains(got, "/web/cluster/leaf/") {
		t.Errorf("link with cluster = %s", got)
	}
}

func TestFileSinceAndBound(t *testing.T) {
	base := time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)
	f := &File{}
	for i := range maxHistory + 5 {
		f.Put(Record{UUID: fmt.Sprint(i), Ended: base.Add(time.Duration(i) * time.Hour)})
	}
	if len(f.Beams) != maxHistory {
		t.Errorf("history = %d, want capped at %d", len(f.Beams), maxHistory)
	}
	f.Put(Record{UUID: "live"})
	got := f.Since(base.Add(time.Duration(maxHistory+2) * time.Hour))
	if len(got) != 3 || got[len(got)-1].UUID != "live" {
		t.Errorf("Since = %+v", got)
	}
}

func TestControlHandler(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	h := r.m.Handler()
	do := func(method, path string, header bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if header {
			req.Header.Set(ControlHeader, "1")
		}
		req.Header.Set(egress.WorkspaceHeader, "/repo/a")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do(http.MethodGet, ControlPath, false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active": false`) {
		t.Errorf("status = %d %s", rec.Code, rec.Body)
	}
	// A cross-origin page can POST, but can't add the header without a
	// preflight prism never answers.
	if rec := do(http.MethodPost, ControlPath+"/start", false); rec.Code != http.StatusForbidden {
		t.Errorf("start without header = %d, want 403", rec.Code)
	}
	if r.added() != 0 {
		t.Fatal("a beam was started without the control header")
	}
	if rec := do(http.MethodPost, ControlPath+"/start", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"standby": true`) {
		t.Errorf("start = %d %s", rec.Code, rec.Body)
	}
	if rec := do(http.MethodGet, ControlPath+"/start", true); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET start = %d, want 405", rec.Code)
	}
	if rec := do(http.MethodPost, ControlPath+"/end", true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"uuid": "uuid-1"`) {
		t.Errorf("end = %d %s", rec.Code, rec.Body)
	}
	r.m.Wait()
	if rec := do(http.MethodPost, ControlPath+"/end", true); rec.Code != http.StatusConflict {
		t.Errorf("end with no beam = %d, want 409", rec.Code)
	}
	if rec := do(http.MethodPost, ControlPath+"/bogus", true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown action = %d, want 404", rec.Code)
	}
}

func TestPrepareWaitsForNode(t *testing.T) {
	calls := 0
	orig := nodeVisible
	nodeVisible = func(context.Context, string) (bool, error) {
		calls++
		return calls >= 2, nil
	}
	t.Cleanup(func() { nodeVisible = orig })

	r := &runtime{fwd: Forward{UUID: "u"}}
	if err := r.Prepare(t.Context(), tunnel.AppInfo{}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("checked the node %d times, want 2", calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	nodeVisible = func(context.Context, string) (bool, error) { return false, nil }
	if err := r.Prepare(ctx, tunnel.AppInfo{}); err == nil {
		t.Error("Prepare returned nil for a node that never appeared")
	}
}

func TestTrialRemovesItsBeamUnlessKept(t *testing.T) {
	for _, keep := range []bool{false, true} {
		cluster := newFakeCluster(time.Now)
		trial := NewTrial(cluster)
		if _, err := trial.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := trial.Connect(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := trial.Close(t.Context(), keep); err != nil {
			t.Fatal(err)
		}
		_, removed := cluster.snapshot()
		if removed := len(removed) == 1; removed == keep {
			t.Errorf("keep=%v: removed=%v", keep, removed)
		}
		if cluster.conns[0].closes() != 1 {
			t.Errorf("keep=%v: forward left open", keep)
		}
	}
}

// Two lanes idling out in the same tick save at once; on the soak run's
// daemon that failed with "rename beams.json.tmp …: no such file".
func TestConcurrentSavesDontCollide(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f := &File{Lanes: map[string]string{"lane": fmt.Sprint(i)}}
			if err := Save(f); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if _, err := Load(); err != nil {
		t.Fatalf("beams.json after concurrent saves: %v", err)
	}
	p, _ := Path()
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".beams-*.json")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// `prism --beam` in off mode: the launch gets a beam of its own, like a
// session in required mode, while everything else keeps to the tunnels
// (or the manual beam). And that beam idles out like any session's, rather
// than sitting until it expires the way the manual one does.
func TestBeamHeaderInOffModeGetsASessionBeam(t *testing.T) {
	r := newRig(t, config.BeamOff, nil)
	forced := func() *http.Request {
		rq := claude(t, "s1", "/repo/a")
		rq.Header.Set(egress.BeamHeader, egress.BeamRequired)
		return rq
	}
	target := r.acquire(t, forced())
	if target == nil || r.added() != 1 || r.lane("claude:s1") == nil {
		t.Fatalf("--beam in off mode: target %v, %d beams", target, r.added())
	}
	if again := r.acquire(t, forced()); again != target {
		t.Error("the launch's next request got another beam")
	}
	// A request without the flag still goes to the tunnels.
	if other, err := r.m.Acquire(claude(t, "s2", "/repo/a")); other != nil || err != nil {
		t.Errorf("an ordinary off-mode request = %v, %v; want the tunnels", other, err)
	}
	r.clock.advance(IdleEnd + time.Minute)
	r.step(t)
	r.m.Wait()
	if r.lane("claude:s1") != nil || r.removed() != 1 {
		t.Error("a --beam session's beam wasn't ended for idling")
	}
}

// forcedClaude is a Claude Code request from a `prism --beam` launch.
func forcedClaude(t *testing.T, sid, ws string) *http.Request {
	rq := claude(t, sid, ws)
	rq.Header.Set(egress.BeamHeader, egress.BeamRequired)
	return rq
}

// A run of `prism --beam claude -p …` one-offs, each a new session, costs
// one beam per repository, not one per launch.
func TestBeamLaunchesFromOneRepositoryShareABeam(t *testing.T) {
	for _, mode := range []string{config.BeamOff, config.BeamPreferred, config.BeamRequired} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t, mode, nil)
			a1 := r.acquire(t, forcedClaude(t, "s1", "/repo/a"))
			if a2 := r.acquire(t, forcedClaude(t, "s2", "/repo/a")); a2 != a1 || r.added() != 1 {
				t.Fatalf("a second --beam launch in the same repository started beam %d", r.added())
			}
			// Its later requests keep to the shared beam.
			if again := r.acquire(t, forcedClaude(t, "s2", "/repo/a")); again != a1 {
				t.Error("the adopting session left the shared beam")
			}
			// Another repository gets its own.
			if b := r.acquire(t, forcedClaude(t, "s3", "/repo/b")); b == a1 || r.added() != 2 {
				t.Errorf("another repository shared the beam (beams started: %d)", r.added())
			}
			// Outside any repository, launches share with each other only.
			n1 := r.acquire(t, forcedClaude(t, "s4", ""))
			if n2 := r.acquire(t, forcedClaude(t, "s5", "")); n2 != n1 || n1 == a1 || r.added() != 3 {
				t.Errorf("launches outside a repository: %d beams", r.added())
			}
			if rec := r.lane("claude:s1").rec; !rec.Adoptable {
				t.Error("the first --beam launch's beam isn't marked adoptable")
			}
			f, _ := r.store.Load()
			if rec := f.Find(r.lane("claude:s1").rec.UUID); rec == nil || !rec.Adoptable {
				t.Error("adoptable wasn't saved, so a restarted daemon would forget it")
			}
		})
	}
}

// One-off requests must never land in an interactive session's
// recording, nor an interactive session in a one-off's.
func TestAdoptionKeepsToBeamLaunches(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	interactive := r.acquire(t, claude(t, "chat", "/repo/a"))
	if oneOff := r.acquire(t, forcedClaude(t, "s1", "/repo/a")); oneOff == interactive || r.added() != 2 {
		t.Fatalf("a --beam launch adopted an interactive session's beam (beams started: %d)", r.added())
	}
	if other := r.acquire(t, claude(t, "chat2", "/repo/a")); other == r.lane("claude:s1").target || r.added() != 3 {
		t.Errorf("an ordinary session adopted a --beam launch's beam (beams started: %d)", r.added())
	}
	if r.lane("claude:chat").rec.Adoptable || r.lane("claude:chat2").rec.Adoptable {
		t.Error("an ordinary session's beam was marked adoptable")
	}
}

// The adopting launches' requests are what keep the shared beam alive;
// once they stop, it idles out on the usual clock.
func TestAdoptedBeamStaysAliveWhileUsed(t *testing.T) {
	r := newRig(t, config.BeamOff, nil)
	r.acquire(t, forcedClaude(t, "s1", "/repo/a"))
	r.clock.advance(IdleEnd - 10*time.Minute)
	r.step(t)
	// A new launch adopts it and sends a request through it.
	r.acquire(t, forcedClaude(t, "s2", "/repo/a"))
	r.carried("claude:s1")
	r.step(t)
	r.clock.advance(20 * time.Minute) // past IdleEnd since the first, not since the last
	r.step(t)
	if r.lane("claude:s1") == nil {
		t.Fatal("the shared beam was ended while its adopter was using it")
	}
	r.clock.advance(IdleEnd)
	r.step(t)
	r.m.Wait()
	if r.lane("claude:s1") != nil || r.removed() != 1 {
		t.Error("the shared beam outlived IdleEnd of no use")
	}
	// The next --beam launch starts a fresh one.
	r.acquire(t, forcedClaude(t, "s3", "/repo/a"))
	if r.added() != 2 {
		t.Errorf("beams started: %d", r.added())
	}
}

// Its successor at rotation stays adoptable.
func TestAdoptableSurvivesRotation(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	r.acquire(t, forcedClaude(t, "s1", "/repo/a"))
	for elapsed := time.Duration(0); elapsed < 11*time.Hour-RotateBefore+time.Minute; elapsed += 5 * time.Minute {
		r.carried("claude:s1")
		r.clock.advance(5 * time.Minute)
		r.step(t)
	}
	r.m.Wait()
	if rec := r.lane("claude:s1").rec; rec.UUID != "uuid-2" || !rec.Adoptable {
		t.Fatalf("successor = %+v", rec)
	}
	if r.acquire(t, forcedClaude(t, "s2", "/repo/a")) != r.lane("claude:s1").target || r.added() != 2 {
		t.Error("a --beam launch didn't adopt the successor")
	}
}

// In required mode, a --beam launch claiming the warmed standby makes it
// adoptable too.
func TestBeamLaunchClaimingTheStandby(t *testing.T) {
	r := newRig(t, config.BeamRequired, nil)
	if _, err := r.m.Start(t.Context(), "/repo/a"); err != nil {
		t.Fatal(err)
	}
	s := r.acquire(t, forcedClaude(t, "s1", "/repo/a"))
	if r.added() != 1 || !r.lane("claude:s1").rec.Adoptable {
		t.Fatalf("standby claim: %d beams, adoptable %v", r.added(), r.lane("claude:s1").rec.Adoptable)
	}
	if r.acquire(t, forcedClaude(t, "s2", "/repo/a")) != s || r.added() != 1 {
		t.Error("the next --beam launch didn't adopt the claimed standby")
	}
}

// A restarted daemon still knows which beams --beam launches may share.
func TestAdoptionSurvivesARestart(t *testing.T) {
	first := newRig(t, config.BeamOff, nil)
	first.acquire(t, forcedClaude(t, "s1", "/repo/a"))
	first.m.shutdown()

	second := newRig(t, config.BeamOff, first.store)
	second.cluster = first.cluster
	second.m.cfg.Ops = first.cluster
	second.m.reattach(t.Context())
	if s := second.lane("claude:s1"); s == nil || !s.rec.Adoptable {
		t.Fatalf("resumed lane = %+v", s)
	}
	if second.acquire(t, forcedClaude(t, "s2", "/repo/a")) != second.lane("claude:s1").target || first.added() != 1 {
		t.Errorf("after a restart a --beam launch started beam %d rather than adopting", first.added())
	}
}
