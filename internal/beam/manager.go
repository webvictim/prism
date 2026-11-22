package beam

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
	"github.com/webvictim/prism/internal/tshwrap"
)

const (
	// RotateBefore is how long before a beam expires the Manager starts
	// its successor. Long enough to create a beam and open its forwards
	// with retries to spare.
	RotateBefore = 10 * time.Minute
	// DrainMax bounds how long a retired beam waits for its last request.
	// Matches the router's longest upstream timeout.
	DrainMax = 10 * time.Minute
	// tick is how often Run checks for rotation and idle beams.
	tick = 30 * time.Second
	// verifyEvery is how often Run asks the cluster whether the beams
	// still exist, to notice one removed from elsewhere.
	verifyEvery = 5 * time.Minute
	// connectTimeout bounds opening a beam's forwards.
	connectTimeout = 90 * time.Second
	// startTimeout bounds starting a beam for a waiting request.
	startTimeout = 3 * time.Minute
	// IdleEnd is how long a beam may go unused before it is ended. Nothing
	// would otherwise stop one being replaced every 11 hours through a
	// weekend, billed to its owner; the next request starts another.
	IdleEnd = 2 * time.Hour
	// idleAtRotation is how long a beam due for replacement must have been
	// unused to be ended instead: its successor would only sit idle too.
	idleAtRotation = 30 * time.Minute
	// idleToReclaim is how long a beam must have been unused before a new
	// session may have it ended to make room, when MaxBeams are running.
	idleToReclaim = 15 * time.Minute
	// retryFailedStart is how long a failed start is remembered, so that a
	// burst of requests doesn't each wait out its own attempt. A missing
	// login is checked afresh every time: it is cheap, and `prism login`
	// should take effect on the very next request.
	retryFailedStart = 30 * time.Second
	// loginCacheFor bounds how stale a login check may be.
	loginCacheFor = 5 * time.Second
	// warnLoginWithin is how close the moment a beam needs replacing must
	// be, with the login expiring first, before prism warns about it.
	warnLoginWithin = time.Hour
	// MaxBeams is how many beams prism keeps running at once by default:
	// about what a Beams tenant allows a user. Config beam_max overrides.
	MaxBeams = 10
)

// Ops is what the Manager needs from the cluster. The default talks to
// tsh; tests substitute their own.
type Ops interface {
	Add(ctx context.Context) (*tshwrap.Beam, error)
	List(ctx context.Context) ([]tshwrap.Beam, error)
	Remove(ctx context.Context, uuid string) error
	// AppHosts returns the public addresses of the two LLM apps.
	AppHosts(ctx context.Context) (anthropic, openai string, err error)
	// Connect opens a beam's forwards and returns once traffic can flow.
	Connect(ctx context.Context, f Forward) (Connection, error)
	// Login reports until when the user's login is valid, or
	// tshwrap.ErrNoLogin when there is no usable one. Called often: it
	// must not ask the cluster.
	Login() (time.Time, error)
	// LogIn runs an SSO login with nobody at a terminal, returning when it
	// completes. With openBrowser it opens the user's browser
	// (beam_auto_login); otherwise it hands onLink the link to finish it
	// with, as soon as there is one.
	LogIn(ctx context.Context, openBrowser bool, onLink func(string)) error
}

// Connection is an open set of forwards.
type Connection interface {
	// Close tears the forwards down and waits for them to exit.
	Close()
}

// Config configures a Manager.
type Config struct {
	Switch *egress.Switch
	Logger *log.Logger
	// AnthropicTunnelPort / OpenAITunnelPort are the tunnel listener
	// ports the router sends to; the Switch recognises them.
	AnthropicTunnelPort int
	OpenAITunnelPort    int
	// Proxy is the Teleport proxy, host:port, for links and records.
	Proxy string
	// Mode returns the beam mode (config.BeamRequired, BeamPreferred or
	// BeamOff). It is called as decisions are made, so a change to the
	// config applies without a restart. Nil means required.
	Mode func() string
	// Scope returns ScopeSession or ScopeRepo (config beam_scope). Nil
	// means session.
	Scope func() string
	// Max returns how many beams may run at once (config beam_max). Nil,
	// or a value below 1, means MaxBeams.
	Max func() int
	// Notify tells the user something they need to act on, outside any
	// terminal: a desktop notification. Nil drops it (the log has it).
	Notify func(message string)
	// AutoLogin reports whether the daemon may log the user in itself when
	// a beam is needed (config beam_auto_login). Nil means no.
	AutoLogin func() bool
	// Ops defaults to tsh.
	Ops Ops
	// Now defaults to time.Now.
	Now func() time.Time
	// Store defaults to beams.json in the config directory.
	Store Store
	// RotateBefore overrides the constant of that name. Only for testing a
	// rollover inside a login shorter than a beam's lifetime.
	RotateBefore time.Duration
	// Version is the daemon's, reported in Status so a newer CLI can tell
	// it is talking to an older daemon.
	Version string
}

// Store persists the beam records.
type Store interface {
	Load() (*File, error)
	Save(*File) error
}

type fileStore struct{}

func (fileStore) Load() (*File, error) { return Load() }
func (fileStore) Save(f *File) error   { return Save(f) }

// Manager owns the beams prism sends inference through: one per agent
// session by default (a lane), each started when its session first needs
// it, replaced before it expires and ended once it goes unused.
type Manager struct {
	cfg Config

	// op serialises the operations that add, replace or remove lanes.
	op sync.Mutex

	// saveMu serialises writes of beams.json. Lanes save independently —
	// two idling out in the same tick is ordinary — and without it two
	// writes raced: one rename failed, and an older snapshot could land
	// after a newer one.
	saveMu sync.Mutex

	mu    sync.Mutex
	lanes map[string]*session
	// joined maps a lane key to the lane it was folded into when beams
	// ran short: another session of the same repository.
	joined  map[string]string
	file    *File
	lastErr string
	retired sync.WaitGroup

	// starting holds the starts in progress, by lane.
	starting map[string]*startCall
	// failErr and failAt remember the last failed start.
	failErr error
	failAt  time.Time
	// outage is set once a request has been refused or sent unrecorded
	// for want of a beam, so the user is told once rather than per request.
	outage bool
	// warnedUntil is the login expiry the user was last warned about, so
	// many lanes produce one warning, not one each.
	warnedUntil time.Time

	loginMu    sync.Mutex
	loginAt    time.Time
	loginUntil time.Time
	loginErr   error
	// autoLoginAt is when the daemon last ran a login itself, so a login
	// the user abandons doesn't open another browser tab per request.
	autoLoginAt time.Time
	// pending is the link login waiting for the user, if any.
	pending *pendingLogin
}

// pendingLogin is a `tsh login --browser=none` the daemon started so that
// a refused request can carry its link. One at a time, shared by every
// request refused meanwhile, so they all name the same link.
type pendingLogin struct {
	ready chan struct{} // closed once link is set
	done  chan struct{} // closed when the login ends
	link  string
}

type startCall struct {
	done   chan struct{}
	target *egress.Target
	err    error
}

type session struct {
	rec    Record
	target *egress.Target
	conn   Connection
	// base is what earlier daemons sent through the beam, so a resumed
	// beam's count carries on rather than restarting at zero.
	base int64
	// lastActive is when the beam was last seen carrying a request (or
	// was activated), and lastServed the count it was seen at.
	lastActive time.Time
	lastServed int64
}

// requests is the beam's total across every daemon that used it.
func (s *session) requests() int64 { return s.base + s.target.Served() }

// New returns a Manager.
func New(cfg Config) (*Manager, error) {
	if cfg.Switch == nil {
		return nil, errors.New("beam: Switch required")
	}
	if cfg.AnthropicTunnelPort <= 0 || cfg.OpenAITunnelPort <= 0 {
		return nil, errors.New("beam: tunnel ports required")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.Ops == nil {
		cfg.Ops = NewTshOps(cfg.Logger, nil)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Store == nil {
		cfg.Store = fileStore{}
	}
	if cfg.RotateBefore <= 0 {
		cfg.RotateBefore = RotateBefore
	}
	if cfg.Mode == nil {
		cfg.Mode = func() string { return config.BeamRequired }
	}
	if cfg.Scope == nil {
		cfg.Scope = func() string { return ScopeSession }
	}
	if cfg.Max == nil {
		cfg.Max = func() int { return MaxBeams }
	}
	if cfg.Notify == nil {
		cfg.Notify = func(string) {}
	}
	if cfg.AutoLogin == nil {
		cfg.AutoLogin = func() bool { return false }
	}
	f, err := cfg.Store.Load()
	if err != nil {
		cfg.Logger.Printf("beam: %v — starting with no beam history", err)
		f = &File{}
	}
	return &Manager{cfg: cfg, file: f, lanes: map[string]*session{},
		joined: map[string]string{}, starting: map[string]*startCall{}}, nil
}

func (m *Manager) max() int {
	if n := m.cfg.Max(); n > 0 {
		return n
	}
	return MaxBeams
}

// LaneStatus is one running beam.
type LaneStatus struct {
	Beam     Record `json:"beam"`
	InFlight int64  `json:"in_flight"`
	Link     string `json:"link"`
	// Joined lists the other lanes folded into this one when beams ran
	// short.
	Joined []string `json:"joined,omitempty"`
}

// Status is a snapshot for `prism beam status` and /_prism/beam.
type Status struct {
	Active bool `json:"active"`
	// Mode is the beam mode in force: required, preferred or off.
	Mode  string `json:"mode"`
	Scope string `json:"scope"`
	Max   int    `json:"max"`
	// Beam is the beam for the caller's repository, or the most recently
	// used one: what a single-beam reader (a launcher, doctor) reports.
	Beam     *Record      `json:"beam,omitempty"`
	InFlight int64        `json:"in_flight"`
	Link     string       `json:"link,omitempty"`
	Beams    []LaneStatus `json:"beams,omitempty"`
	// Standby is set while a warmed beam waits for a session to claim it.
	Standby   bool     `json:"standby,omitempty"`
	LastError string   `json:"last_error,omitempty"`
	Recent    []Record `json:"recent,omitempty"`
	// RotateBefore is how long before expiry a beam is replaced.
	RotateBefore time.Duration `json:"rotate_before_ns"`
	// Version is the daemon's version, and Busy how many requests it is
	// carrying by any route: a launcher restarts an older daemon only
	// when Busy is zero, so an upgrade never cuts a stream off.
	Version string `json:"version,omitempty"`
	Busy    int64  `json:"busy"`
	// Unrecorded is how many requests this daemon has sent past the beams
	// because their launch asked (`prism --no-beam`).
	Unrecorded int64 `json:"unrecorded"`
}

// Status reports the running beams and the recent history. workspace, if
// set, picks which beam is reported as Beam.
func (m *Manager) Status(workspace string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Mode: m.cfg.Mode(), Scope: m.cfg.Scope(), Max: m.max(), LastError: m.lastErr, RotateBefore: m.cfg.RotateBefore,
		Version: m.cfg.Version, Busy: m.cfg.Switch.InFlight(), Unrecorded: m.cfg.Switch.Unrecorded()}
	var best *session
	for _, s := range m.lanesByActivity() {
		rec := s.rec
		rec.Requests = s.requests()
		ls := LaneStatus{Beam: rec, InFlight: s.target.InFlight(), Link: rec.Link("")}
		for k, into := range m.joined {
			if into == s.rec.Key {
				ls.Joined = append(ls.Joined, k)
			}
		}
		sort.Strings(ls.Joined)
		st.Beams = append(st.Beams, ls)
		if s.rec.Key == standbyLane {
			st.Standby = true
		}
		if best == nil || (workspace != "" && s.rec.Workspace == workspace && best.rec.Workspace != workspace) {
			best = s
		}
	}
	if best != nil {
		rec := best.rec
		rec.Requests = best.requests()
		st.Active = true
		st.Beam = &rec
		st.InFlight = best.target.InFlight()
		st.Link = rec.Link("")
	}
	st.Recent = m.file.Since(m.cfg.Now().Add(-24 * time.Hour))
	return st
}

// lanesByActivity returns the running beams, most recently used first.
// Callers hold m.mu.
func (m *Manager) lanesByActivity() []*session {
	out := make([]*session, 0, len(m.lanes))
	for _, s := range m.lanes {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].lastActive.After(out[j].lastActive) })
	return out
}

// ErrNoBeam is returned by End when no beam is in use.
var ErrNoBeam = errors.New("no beam is in use")

// Start is `prism beam start`. In off mode it starts the one beam that
// then carries everything; otherwise it warms a standby beam that the next
// new session claims, so that session's first request needn't wait.
func (m *Manager) Start(ctx context.Context, workspace string) (Record, error) {
	key := standbyLane
	if m.cfg.Mode() == config.BeamOff {
		key = manualLane
		workspace = ""
	}
	if s := m.lane(key); s != nil {
		return s.rec, nil
	}
	if err := m.ensureLogin(ctx); err != nil {
		m.setErr(err)
		return Record{}, err
	}
	if m.count() >= m.max() {
		return Record{}, fmt.Errorf("all %d beams prism may run are in use (beam_max)", m.max())
	}
	return m.startLane(ctx, key, workspace)
}

// startLane creates a beam for key and makes it the lane's. It doesn't
// hold m.op while the beam starts, so sessions starting together don't
// queue; Acquire's starting map keeps one start per lane.
func (m *Manager) startLane(ctx context.Context, key, workspace string) (Record, error) {
	if s := m.lane(key); s != nil {
		return s.rec, nil
	}
	s, err := m.create(ctx, key, workspace)
	if err != nil {
		m.setErr(err)
		return Record{}, err
	}
	m.activate(s)
	return s.rec, nil
}

// End is `prism beam end`: it ends the beams for workspace (every session
// of that repository), or every beam when workspace is empty or none
// matches it. Each is removed once its in-flight requests have finished,
// which starts its summary.
func (m *Manager) End(ctx context.Context, workspace string) ([]Record, error) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	var all, mine []*session
	for _, s := range m.lanes {
		all = append(all, s)
		if workspace != "" && s.rec.Workspace == workspace {
			mine = append(mine, s)
		}
	}
	m.mu.Unlock()
	if len(mine) == 0 {
		mine = all
	}
	if len(mine) == 0 {
		return nil, ErrNoBeam
	}
	var out []Record
	for _, s := range mine {
		m.endLocked(s, "")
		out = append(out, s.rec)
	}
	return out, nil
}

// endLocked stops using a lane's beam and removes it once drained. why, if
// set, is why prism ended it on its own. Callers hold m.op.
func (m *Manager) endLocked(old *session, why string) {
	m.drop(old)
	switch {
	case why != "":
		m.cfg.Logger.Printf("beam: ending %s (%s): %s; its next request starts another", old.rec.Alias, old.rec.Key, why)
	default:
		m.cfg.Logger.Printf("beam: ended %s (%s)", old.rec.Alias, old.rec.Key)
	}
	m.retire(old, true)
}

// drop takes a lane out of service, and anything joined to it.
func (m *Manager) drop(old *session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lanes[old.rec.Key] == old {
		delete(m.lanes, old.rec.Key)
	}
	for k, into := range m.joined {
		if into == old.rec.Key {
			delete(m.joined, k)
		}
	}
	m.syncFileLocked()
}

// Peek implements egress.Provider: the beam a request would use, if it is
// already running. It never starts one.
func (m *Manager) Peek(req *http.Request) *egress.Target {
	if s := m.laneFor(keyOf(req.Header), m.modeFor(req.Header)); s != nil {
		return s.target
	}
	return nil
}

// modeFor is the beam mode for one request: the configured one, unless
// its launch asked for a beam regardless (`prism --beam`, egress.BeamHeader).
// `prism --no-beam` never gets this far: the Switch sends it to the
// tunnels itself.
func (m *Manager) modeFor(h http.Header) string {
	if h.Get(egress.BeamHeader) == egress.BeamRequired {
		return config.BeamRequired
	}
	return m.cfg.Mode()
}

// laneFor returns the running beam for a request's key under mode,
// following a join and, in off mode, the one manual beam.
func (m *Manager) laneFor(k requestKey, mode string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mode == config.BeamOff {
		return m.lanes[manualLane]
	}
	key := k.lane(m.cfg.Scope())
	if s := m.lanes[key]; s != nil {
		return s
	}
	if into, ok := m.joined[key]; ok {
		return m.lanes[into]
	}
	return nil
}

// Acquire implements egress.Provider: it is asked which beam a request
// goes through. A session whose beam is running gets it at once. One
// without a beam has one started, the request held until it is ready;
// every request of that session arriving meanwhile waits on the same
// start. When MaxBeams are running, a session joins a beam of its own
// repository, or one that has gone idle is ended to make room. When no
// beam can be had, required mode refuses the request and preferred mode
// sends it to the tunnels, unrecorded.
func (m *Manager) Acquire(req *http.Request) (*egress.Target, error) {
	ctx := req.Context()
	mode := m.modeFor(req.Header)
	k := keyOf(req.Header)
	if s := m.laneFor(k, mode); s != nil {
		return s.target, nil
	}
	if mode == config.BeamOff {
		return nil, nil
	}
	if _, err := m.login(); err != nil && !m.cfg.AutoLogin() {
		if mode == config.BeamRequired {
			err = m.loginLink(ctx)
		}
		return m.unavailable(mode, err)
	}
	key := k.lane(m.cfg.Scope())

	m.mu.Lock()
	if errors.Is(m.failErr, tshwrap.ErrNoLogin) {
		m.failErr = nil // the login is back
	}
	if m.failErr != nil && m.cfg.Now().Sub(m.failAt) < retryFailedStart {
		err := m.failErr
		m.mu.Unlock()
		return m.unavailable(mode, err)
	}
	call := m.starting[key]
	if call == nil {
		// A warmed standby beam goes to the first session that needs one.
		if sb := m.lanes[standbyLane]; sb != nil {
			claimed := m.claimLocked(sb, key, k.Workspace)
			m.mu.Unlock()
			m.save()
			return claimed.target, nil
		}
		if len(m.lanes)+len(m.starting) >= m.max() {
			if s := m.joinLocked(key, k.Workspace); s != nil {
				m.mu.Unlock()
				return s.target, nil
			}
			victim := m.idlestLocked()
			if victim == nil {
				m.mu.Unlock()
				return m.unavailable(mode, fmt.Errorf("all %d beams prism may run are busy (beam_max); "+
					"`prism beam end` in a repository ends its beams", m.max()))
			}
			m.mu.Unlock()
			m.op.Lock()
			m.endLocked(victim, "its beam was needed for another session")
			m.op.Unlock()
			m.mu.Lock()
		}
		if call = m.starting[key]; call == nil {
			call = &startCall{done: make(chan struct{})}
			m.starting[key] = call
			go m.runStart(call, key, k.Workspace, mode)
		}
	}
	m.mu.Unlock()
	select {
	case <-call.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if call.err != nil {
		return m.unavailable(mode, call.err)
	}
	return call.target, nil
}

// claimLocked hands the standby beam to key, returning the lane now
// carrying it. The lane is a copy: Run may be looking at the standby, and
// the copy's record changes under nobody. Callers hold m.mu, and save
// once they release it.
func (m *Manager) claimLocked(sb *session, key, workspace string) *session {
	delete(m.lanes, standbyLane)
	claimed := *sb
	claimed.rec.Key = key
	if claimed.rec.Workspace == "" {
		claimed.rec.Workspace = workspace
	}
	m.lanes[key] = &claimed
	m.file.Put(claimed.rec)
	m.syncFileLocked()
	m.cfg.Logger.Printf("beam: %s now carries %s", claimed.rec.Alias, key)
	return &claimed
}

// joinLocked folds key into a running beam of the same repository, when
// beams have run short. Callers hold m.mu.
func (m *Manager) joinLocked(key, workspace string) *session {
	if workspace == "" {
		return nil
	}
	for _, s := range m.lanesByActivity() {
		if s.rec.Workspace == workspace && s.rec.Key != standbyLane {
			m.joined[key] = s.rec.Key
			m.cfg.Logger.Printf("beam: %d beams are running (beam_max), so %s shares %s with %s, from the same repository",
				len(m.lanes), key, s.rec.Alias, s.rec.Key)
			return s
		}
	}
	return nil
}

// idlestLocked is the beam unused for longest, if that is at least
// idleToReclaim. Callers hold m.mu.
func (m *Manager) idlestLocked() *session {
	lanes := m.lanesByActivity()
	if len(lanes) == 0 {
		return nil
	}
	s := lanes[len(lanes)-1]
	if s.target.InFlight() > 0 || m.cfg.Now().Sub(s.lastActive) < idleToReclaim {
		return nil
	}
	return s
}

// runStart starts a beam for the requests waiting on call. It runs apart
// from any one request, so a client giving up doesn't abandon a beam
// half made.
func (m *Manager) runStart(call *startCall, key, workspace, mode string) {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	m.cfg.Logger.Printf("beam: %s needs a beam (beam_mode=%s); starting one", key, mode)
	err := m.ensureLogin(ctx)
	if err == nil {
		_, err = m.startLane(ctx, key, workspace)
	}
	m.mu.Lock()
	delete(m.starting, key)
	call.err = err
	if err != nil {
		m.failErr, m.failAt = err, m.cfg.Now()
	} else if s := m.lanes[key]; s != nil {
		call.target = s.target
	}
	m.mu.Unlock()
	close(call.done)
}

// Wait blocks until every retired beam has finished draining. For tests
// and shutdown.
func (m *Manager) Wait() { m.retired.Wait() }

// Run reattaches to the beams recorded by an earlier daemon, then keeps
// them fresh until ctx ends. On return, the forwards are closed but the
// beams are left running: a daemon restart should pick up where it left
// off, not end the sessions.
func (m *Manager) Run(ctx context.Context) {
	m.reattach(ctx)
	t := time.NewTicker(tick)
	defer t.Stop()
	lastVerify := m.cfg.Now()
	for {
		m.step(ctx, &lastVerify)
		select {
		case <-ctx.Done():
			m.shutdown()
			return
		case <-t.C:
		}
	}
}

// step is one pass of Run's loop, split out so tests can drive it.
func (m *Manager) step(ctx context.Context, lastVerify *time.Time) {
	now := m.cfg.Now()
	if now.Sub(*lastVerify) >= verifyEvery {
		*lastVerify = now
		m.verify(ctx)
	}
	for _, s := range m.snapshot() {
		m.noteActivity(s)
		m.rotateIfDue(ctx, s)
		m.endIfIdle(s)
	}
}

// snapshot lists the running beams.
func (m *Manager) snapshot() []*session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lanesByActivity()
}

func (m *Manager) lane(key string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lanes[key]
}

func (m *Manager) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.lanes) + len(m.starting)
}

// live reports whether s is still a running lane.
func (m *Manager) live(s *session) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lanes[s.rec.Key] == s
}

func (m *Manager) setErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.lastErr = ""
		return
	}
	m.lastErr = err.Error()
}

// noteActivity records whether a beam has carried anything since the last
// look. Counting rather than timestamping keeps every time on the
// Manager's own clock.
func (m *Manager) noteActivity(s *session) {
	if served := s.target.Served(); served != s.lastServed || s.target.InFlight() > 0 {
		m.mu.Lock()
		s.lastServed = served
		s.lastActive = m.cfg.Now()
		m.mu.Unlock()
	}
}

// endIfIdle ends a beam that has gone unused for IdleEnd. The manual beam
// (`prism beam start` in off mode) was started by hand, and nothing would
// start the next one, so it is left to its owner. A session's beam is
// ended whatever the mode: in off mode it was started by `prism --beam`,
// and would otherwise sit, billed, until it expired.
func (m *Manager) endIfIdle(s *session) {
	if s.rec.Key == manualLane || s.target.InFlight() > 0 {
		return
	}
	idle := m.cfg.Now().Sub(s.lastActive)
	if idle < IdleEnd {
		return
	}
	m.op.Lock()
	defer m.op.Unlock()
	if m.live(s) {
		m.endLocked(s, fmt.Sprintf("unused for %s", idle.Round(time.Minute)))
	}
}

// warnLoginExpiry tells the user, once per login, when it runs out before
// a beam in use has to be replaced — while there is still time to log in
// and keep recording without a pause.
func (m *Manager) warnLoginExpiry() {
	if m.cfg.AutoLogin() {
		return
	}
	now := m.cfg.Now()
	var first *session
	for _, s := range m.snapshot() {
		if s.rec.Key == manualLane {
			continue // started by hand; its owner looks after it
		}
		if now.Sub(s.lastActive) >= idleAtRotation {
			continue // it will be ended rather than replaced
		}
		if first == nil || s.rec.Expires.Before(first.rec.Expires) {
			first = s
		}
	}
	if first == nil {
		return
	}
	until, err := m.login()
	replaceAt := first.rec.Expires.Add(-m.cfg.RotateBefore)
	if err == nil && !until.Before(replaceAt) {
		return
	}
	if replaceAt.Sub(now) > warnLoginWithin {
		return
	}
	m.mu.Lock()
	if !m.warnedUntil.IsZero() && m.warnedUntil.Equal(until) {
		m.mu.Unlock()
		return
	}
	m.warnedUntil = until
	m.mu.Unlock()
	msg := fmt.Sprintf("Your Teleport login expires at %s, before beam %s has to be replaced at %s. Run `prism login` to keep recording without a pause.",
		until.Local().Format("15:04"), first.rec.Alias, replaceAt.Local().Format("15:04"))
	if err != nil {
		msg = fmt.Sprintf("Your Teleport login has expired, and beam %s has to be replaced at %s. Run `prism login` to keep recording without a pause.",
			first.rec.Alias, replaceAt.Local().Format("15:04"))
	}
	m.cfg.Logger.Printf("beam: %s", msg)
	m.cfg.Notify(msg)
}

// create starts a new beam for key and opens its forwards. On failure the
// beam is removed again, so a half-made one doesn't linger for 11 hours.
func (m *Manager) create(ctx context.Context, key, workspace string) (*session, error) {
	b, err := m.cfg.Ops.Add(ctx)
	if err != nil {
		return nil, fmt.Errorf("start beam: %w", err)
	}
	m.cfg.Logger.Printf("beam: started %s (%s) for %s, expires %s", b.ID, b.UUID, key, b.Expires.Local().Format(time.Kitchen))
	rec := Record{UUID: b.UUID, Alias: b.ID, Proxy: m.cfg.Proxy, Started: m.cfg.Now(), Expires: b.Expires,
		Key: key, Workspace: workspace}
	s, err := m.connect(ctx, rec)
	if err != nil {
		if rmErr := m.cfg.Ops.Remove(context.WithoutCancel(ctx), b.UUID); rmErr != nil {
			m.cfg.Logger.Printf("beam: could not remove %s after failing to connect: %v", b.ID, rmErr)
		}
		return nil, err
	}
	return s, nil
}

// connect opens the forwards for rec and builds its egress target.
func (m *Manager) connect(ctx context.Context, rec Record) (*session, error) {
	anth, oai, err := m.cfg.Ops.AppHosts(ctx)
	if err != nil {
		return nil, fmt.Errorf("find the LLM apps: %w", err)
	}
	ports, err := freePorts(2)
	if err != nil {
		return nil, err
	}
	fwd := Forward{UUID: rec.UUID, AnthropicHost: anth, OpenAIHost: oai, AnthropicPort: ports[0], OpenAIPort: ports[1]}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := m.cfg.Ops.Connect(cctx, fwd)
	if err != nil {
		return nil, fmt.Errorf("open forwards to %s: %w", rec.Alias, err)
	}
	target, err := egress.NewTarget(rec.Alias,
		egress.Route{TunnelPort: m.cfg.AnthropicTunnelPort, AppHost: anth, ForwardAddr: fmt.Sprintf("127.0.0.1:%d", fwd.AnthropicPort)},
		egress.Route{TunnelPort: m.cfg.OpenAITunnelPort, AppHost: oai, ForwardAddr: fmt.Sprintf("127.0.0.1:%d", fwd.OpenAIPort)},
	)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &session{rec: rec, target: target, conn: conn, base: rec.Requests}, nil
}

// activate puts s in service as its lane's beam and records it. Whatever
// it replaced is the caller's to retire.
func (m *Manager) activate(s *session) (previous *session) {
	s.lastActive = m.cfg.Now()
	s.lastServed = s.target.Served()
	m.mu.Lock()
	previous = m.lanes[s.rec.Key]
	m.lanes[s.rec.Key] = s
	m.file.Put(s.rec)
	m.syncFileLocked()
	m.lastErr = ""
	m.failErr = nil
	m.outage = false
	m.mu.Unlock()
	m.save()
	m.cfg.Logger.Printf("beam: %s now goes through %s", s.rec.Key, s.rec.Alias)
	return previous
}

// syncFileLocked mirrors the running lanes into the file. Callers hold
// m.mu.
func (m *Manager) syncFileLocked() {
	m.file.Lanes = map[string]string{}
	for k, s := range m.lanes {
		m.file.Lanes[k] = s.rec.UUID
	}
}

// retire waits for old's requests to finish, closes its forwards, and,
// if remove is set, removes the beam so its summary can start.
func (m *Manager) retire(old *session, remove bool) {
	// The goroutine works on its own copy: callers return old.rec.
	rec := old.rec
	m.retired.Add(1)
	go func() {
		defer m.retired.Done()
		idle := old.target.WaitIdle(context.Background(), DrainMax)
		if !idle {
			m.cfg.Logger.Printf("beam: %s still had %d requests after %s; closing anyway",
				rec.Alias, old.target.InFlight(), DrainMax)
		}
		old.conn.Close()
		old.target.Close()
		m.mu.Lock()
		rec.Ended = m.cfg.Now()
		rec.Requests = old.requests()
		m.file.Put(rec)
		m.mu.Unlock()
		m.save()
		if !remove {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := m.cfg.Ops.Remove(ctx, rec.UUID); err != nil {
			m.cfg.Logger.Printf("beam: could not remove %s: %v — it expires at %s anyway, and is summarised then",
				rec.Alias, err, rec.Expires.Local().Format(time.Kitchen))
			return
		}
		m.cfg.Logger.Printf("beam: removed %s after %d requests; its summary appears in about 15 minutes: %s",
			rec.Alias, rec.Requests, rec.Link(""))
	}()
}

// rotateIfDue replaces a beam that is about to expire. If the next one
// can't be started the old one keeps serving until it expires; then its
// lane is dropped, and its next request starts another or is refused.
func (m *Manager) rotateIfDue(ctx context.Context, cur *session) {
	now := m.cfg.Now()
	left := cur.rec.Expires.Sub(now)
	if left > m.cfg.RotateBefore {
		return
	}
	m.op.Lock()
	defer m.op.Unlock()
	if !m.live(cur) {
		return
	}
	if left <= 0 {
		m.lose(cur, "it expired before a replacement could be started")
		return
	}
	if cur.rec.Key != manualLane && cur.target.InFlight() == 0 && now.Sub(cur.lastActive) >= idleAtRotation {
		// Its next request starts a fresh beam; replacing this one now
		// would only start another to sit idle.
		m.endLocked(cur, fmt.Sprintf("it is due for replacement and has been idle for %s", now.Sub(cur.lastActive).Round(time.Minute)))
		return
	}
	m.cfg.Logger.Printf("beam: %s expires in %s; starting its successor", cur.rec.Alias, left.Round(time.Second))
	if err := m.ensureLogin(ctx); err != nil && m.cfg.AutoLogin() {
		m.setErr(err)
		m.cfg.Logger.Printf("beam: rotation needs a login: %v (%s keeps serving)", err, cur.rec.Alias)
		return
	}
	next, err := m.create(ctx, cur.rec.Key, cur.rec.Workspace)
	if err != nil {
		m.setErr(err)
		m.cfg.Logger.Printf("beam: rotation failed: %v (%s keeps serving; retrying in %s)", err, cur.rec.Alias, tick)
		return
	}
	m.activate(next)
	m.retire(cur, true)
}

// verify notices beams that have gone away — removed with tsh, say — and
// warns when the login will lapse before a beam needs replacing.
func (m *Manager) verify(ctx context.Context) {
	lanes := m.snapshot()
	if len(lanes) == 0 {
		return
	}
	m.warnLoginExpiry()
	beams, err := m.cfg.Ops.List(ctx)
	if err != nil {
		// Can't tell; a forward that really is dead shows up in requests.
		return
	}
	for _, s := range lanes {
		if slices.ContainsFunc(beams, func(b tshwrap.Beam) bool { return b.UUID == s.rec.UUID }) {
			continue
		}
		m.op.Lock()
		if m.live(s) {
			m.lose(s, "it no longer exists")
		}
		m.op.Unlock()
	}
}

// lose drops a beam that can't be used any more. Callers hold m.op.
func (m *Manager) lose(cur *session, why string) {
	m.drop(cur)
	m.setErr(fmt.Errorf("beam %s was lost: %s", cur.rec.Alias, why))
	switch {
	case cur.rec.Key == manualLane:
		m.cfg.Logger.Printf("beam: WARNING: lost %s (%s) — inference goes through the tunnels and is NOT recorded under a beam until another starts",
			cur.rec.Alias, why)
		m.cfg.Notify(fmt.Sprintf("Beam %s was lost (%s), so inference is no longer recorded in Beams. Run `prism beam start` to record again.", cur.rec.Alias, why))
	default:
		m.cfg.Logger.Printf("beam: lost %s (%s) — its next request starts another", cur.rec.Alias, why)
	}
	m.retire(cur, false)
}

// reattach resumes the beams an earlier daemon was using, if they still
// exist. A daemon restart — an upgrade, a crash — shouldn't split one
// session's work across two beams.
func (m *Manager) reattach(ctx context.Context) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	want := map[string]Record{}
	for key, uuid := range m.file.Lanes {
		rec := Record{UUID: uuid}
		if r := m.file.Find(uuid); r != nil {
			rec = *r
		}
		rec.Key = key
		want[key] = rec
	}
	m.mu.Unlock()
	if len(want) == 0 {
		return
	}
	forget := func(rec Record, why string) {
		m.cfg.Logger.Printf("beam: not resuming %s: %s", cmpStr(rec.Alias, rec.UUID), why)
		m.mu.Lock()
		if r := m.file.Find(rec.UUID); r != nil && r.Ended.IsZero() {
			r.Ended = m.cfg.Now()
		}
		m.mu.Unlock()
	}
	defer func() {
		m.mu.Lock()
		m.syncFileLocked()
		m.mu.Unlock()
		m.save()
	}()
	beams, err := m.cfg.Ops.List(ctx)
	if err != nil {
		for _, rec := range want {
			forget(rec, fmt.Sprintf("could not list beams: %v", err))
		}
		return
	}
	for key, rec := range want {
		i := slices.IndexFunc(beams, func(b tshwrap.Beam) bool { return b.UUID == rec.UUID })
		if i < 0 {
			forget(rec, "it no longer exists")
			continue
		}
		live := beams[i]
		if rec.Alias == "" {
			rec = Record{UUID: live.UUID, Alias: live.ID, Proxy: m.cfg.Proxy, Started: m.cfg.Now(), Key: key}
		}
		rec.Expires = live.Expires
		if !rec.Expires.After(m.cfg.Now()) {
			forget(rec, "it has expired")
			continue
		}
		s, err := m.connect(ctx, rec)
		if err != nil {
			forget(rec, err.Error())
			continue
		}
		m.cfg.Logger.Printf("beam: resuming %s for %s", rec.Alias, key)
		m.activate(s)
	}
}

func cmpStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// shutdown closes every beam's forwards without removing the beams.
func (m *Manager) shutdown() {
	for _, s := range m.snapshot() {
		s.conn.Close()
		s.target.Close()
		m.mu.Lock()
		r := s.rec
		r.Requests = s.requests()
		m.file.Put(r)
		m.mu.Unlock()
	}
	m.save()
}

func (m *Manager) save() {
	// Snapshot inside saveMu, so the last write is always the latest state.
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.Lock()
	snapshot := *m.file
	snapshot.Beams = append([]Record(nil), m.file.Beams...)
	snapshot.Lanes = make(map[string]string, len(m.file.Lanes))
	for k, v := range m.file.Lanes {
		snapshot.Lanes[k] = v
	}
	m.mu.Unlock()
	if err := m.cfg.Store.Save(&snapshot); err != nil {
		m.cfg.Logger.Printf("beam: could not save beam records: %v", err)
	}
}
