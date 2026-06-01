// Package identity polls `tsh status` and notifies on session-validity
// transitions. Used by both the HTTP listener and the TCP tunnel
// supervisor so they can:
//
//   - return a clean error to the client when the user's tsh session
//     has expired (instead of leaking opaque upstream 403s or hanging
//     forever inside an interactive `tsh apps login`);
//   - auto-recover when the user runs `tsh login` and the session
//     becomes valid again (refresh app cert, restart `tsh proxy app`).
//
// The watcher polls more aggressively when expired so the recovery
// path fires within a few seconds of the user logging back in.
package identity

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/gravitational/prism/internal/tshwrap"
)

// State is the watcher's current view of the tsh session.
type State int

const (
	// StateUnknown is the initial state before the first poll completes.
	StateUnknown State = iota
	// StateHealthy means the most recent poll saw a non-expired profile.
	StateHealthy
	// StateExpired means the most recent poll saw an expired profile.
	StateExpired
	// StateError means `tsh status` itself failed (binary missing,
	// profile file unreadable, etc.). Treated as "we can't tell" for
	// callback purposes — neither expired nor healthy fires.
	StateError
)

// String returns a short human-readable form.
func (s State) String() string {
	switch s {
	case StateHealthy:
		return "healthy"
	case StateExpired:
		return "expired"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// Snapshot is the watcher's exported view: everything you'd want to show
// in `prism status` or log from the daemon.
type Snapshot struct {
	State      State
	Username   string
	Cluster    string
	ValidUntil time.Time
	LastCheck  time.Time
	LastError  error
}

// Config bundles the watcher's startup parameters. OnExpired/OnRecovered
// callbacks run on the watcher's own goroutine — keep them quick, kick
// off slow work in a new goroutine if needed.
type Config struct {
	Logger      *log.Logger
	OnExpired   func()
	OnRecovered func()

	// HealthyInterval is how often to re-check when the session looks
	// healthy. Defaults to 30s.
	HealthyInterval time.Duration
	// ExpiredInterval is how often to re-check when the session is
	// expired — faster, so we catch the user re-logging in promptly.
	// Defaults to 5s.
	ExpiredInterval time.Duration

	// StatusFn is the function used to read tsh state. Overridable
	// for tests; defaults to tshwrap.StatusJSON.
	StatusFn func() (*tshwrap.Status, error)
}

// Watcher polls tsh status in the background.
type Watcher struct {
	cfg Config

	mu       sync.Mutex
	snapshot Snapshot
}

// New constructs a Watcher. Does not start polling — call Run.
func New(cfg Config) *Watcher {
	if cfg.HealthyInterval == 0 {
		cfg.HealthyInterval = 30 * time.Second
	}
	if cfg.ExpiredInterval == 0 {
		cfg.ExpiredInterval = 5 * time.Second
	}
	if cfg.StatusFn == nil {
		cfg.StatusFn = tshwrap.StatusJSON
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(nullWriter{}, "", 0)
	}
	return &Watcher{cfg: cfg}
}

// Run polls until ctx is done. Safe to call once; second calls return
// immediately (the goroutine is owned by the first).
func (w *Watcher) Run(ctx context.Context) {
	// Initial poll fires immediately so the daemon doesn't have to
	// wait one whole interval before we know whether the session is
	// healthy.
	w.poll()
	for {
		interval := w.cfg.HealthyInterval
		if w.Snapshot().State == StateExpired {
			interval = w.cfg.ExpiredInterval
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		w.poll()
	}
}

// Snapshot returns a copy of the current state.
func (w *Watcher) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.snapshot
}

// poll runs `tsh status` once, updates the snapshot, and fires the
// expired/recovered callbacks on state transitions.
func (w *Watcher) poll() {
	status, err := w.cfg.StatusFn()
	now := time.Now()

	var next Snapshot
	next.LastCheck = now
	switch {
	case err != nil:
		next.State = StateError
		next.LastError = err
	case status.IsExpired():
		next.State = StateExpired
		next.Username = status.Username
		next.Cluster = status.Cluster
		next.ValidUntil = status.ValidUntil
	default:
		next.State = StateHealthy
		next.Username = status.Username
		next.Cluster = status.Cluster
		next.ValidUntil = status.ValidUntil
	}

	w.mu.Lock()
	prev := w.snapshot
	w.snapshot = next
	w.mu.Unlock()

	w.fireTransitions(prev.State, next.State)
}

// fireTransitions invokes the expired/recovered callbacks on edge
// transitions only. StateError is intentionally not treated as a
// transition either way — we don't want a transient `tsh` failure to
// thrash callbacks.
func (w *Watcher) fireTransitions(prev, next State) {
	if prev == next {
		return
	}
	switch {
	case prev != StateExpired && next == StateExpired:
		w.cfg.Logger.Printf("identity: tsh session has expired — run `tsh login` to restore")
		if w.cfg.OnExpired != nil {
			w.cfg.OnExpired()
		}
	case prev == StateExpired && next == StateHealthy:
		w.cfg.Logger.Printf("identity: tsh session restored — recovering")
		if w.cfg.OnRecovered != nil {
			w.cfg.OnRecovered()
		}
	}
}

// nullWriter is an io.Writer that discards everything. Used so the
// watcher works when no logger is supplied.
type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }
