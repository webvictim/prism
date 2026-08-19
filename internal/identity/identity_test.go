package identity

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/tshwrap"
)

// TestWatcherTransitions exercises the full Healthy → Expired → Healthy
// cycle that the real daemon will see when a user runs `tsh logout` and
// later `tsh login`. Uses a stubbed StatusFn so the test doesn't shell
// out to tsh.
func TestWatcherTransitions(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		current  *tshwrap.Status
		callErr  error
		expired  atomic.Int64
		restored atomic.Int64
	)
	setStatus := func(s *tshwrap.Status, err error) {
		mu.Lock()
		current = s
		callErr = err
		mu.Unlock()
	}
	getStatus := func() (*tshwrap.Status, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, callErr
	}

	// Start healthy.
	setStatus(&tshwrap.Status{
		Username:   "alice",
		Cluster:    "example",
		ValidUntil: time.Now().Add(1 * time.Hour),
	}, nil)

	w := New(Config{
		Logger:          log.New(io.Discard, "", 0),
		HealthyInterval: 30 * time.Millisecond,
		ExpiredInterval: 30 * time.Millisecond,
		StatusFn:        getStatus,
		OnExpired:       func() { expired.Add(1) },
		OnRecovered:     func() { restored.Add(1) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	if err := waitFor(func() bool { return w.Snapshot().State == StateHealthy }, time.Second); err != nil {
		t.Fatalf("initial poll never produced healthy state: %v", err)
	}

	// Simulate `tsh logout`: tshwrap.StatusJSON returns &Status{} (zero
	// ValidUntil) with no error. The watcher should classify that as
	// Expired and fire OnExpired once.
	setStatus(&tshwrap.Status{}, nil)
	if err := waitFor(func() bool { return w.Snapshot().State == StateExpired }, time.Second); err != nil {
		t.Fatalf("watcher did not detect logged-out state: %v (snap=%+v)", err, w.Snapshot())
	}
	if got := expired.Load(); got != 1 {
		t.Fatalf("OnExpired fired %d times, want 1", got)
	}

	// Simulate `tsh login`: a future ValidUntil. OnRecovered must fire
	// exactly once, even across multiple subsequent polls.
	setStatus(&tshwrap.Status{
		Username:   "alice",
		Cluster:    "example",
		ValidUntil: time.Now().Add(1 * time.Hour),
	}, nil)
	if err := waitFor(func() bool { return w.Snapshot().State == StateHealthy }, time.Second); err != nil {
		t.Fatalf("watcher did not detect re-login: %v (snap=%+v)", err, w.Snapshot())
	}
	if got := restored.Load(); got != 1 {
		t.Fatalf("OnRecovered fired %d times, want 1", got)
	}

	// Stay healthy across additional polls — no spurious callbacks.
	time.Sleep(200 * time.Millisecond)
	if got := expired.Load(); got != 1 {
		t.Fatalf("OnExpired re-fired during healthy stretch: %d", got)
	}
	if got := restored.Load(); got != 1 {
		t.Fatalf("OnRecovered re-fired during healthy stretch: %d", got)
	}
}

// TestWatcherStatusErrorIsNotATransition guards against the regression
// where a transient `tsh status` failure (binary missing, profile
// unreadable) would spuriously fire OnExpired and trigger a cascade of
// pointless rotation attempts.
func TestWatcherStatusErrorIsNotATransition(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		current  *tshwrap.Status
		callErr  error
		expired  atomic.Int64
		restored atomic.Int64
	)
	setStatus := func(s *tshwrap.Status, err error) {
		mu.Lock()
		current, callErr = s, err
		mu.Unlock()
	}
	getStatus := func() (*tshwrap.Status, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, callErr
	}

	setStatus(&tshwrap.Status{
		ValidUntil: time.Now().Add(1 * time.Hour),
	}, nil)

	w := New(Config{
		Logger:          log.New(io.Discard, "", 0),
		HealthyInterval: 20 * time.Millisecond,
		ExpiredInterval: 20 * time.Millisecond,
		StatusFn:        getStatus,
		OnExpired:       func() { expired.Add(1) },
		OnRecovered:     func() { restored.Add(1) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	if err := waitFor(func() bool { return w.Snapshot().State == StateHealthy }, time.Second); err != nil {
		t.Fatalf("never healthy: %v", err)
	}

	// Inject a transient tsh failure (StateError). Should not call
	// OnExpired — we don't know whether the session is actually gone.
	setStatus(nil, fmt.Errorf("simulated tsh binary error"))
	if err := waitFor(func() bool { return w.Snapshot().State == StateError }, time.Second); err != nil {
		t.Fatalf("never reached error state: %v", err)
	}
	if got := expired.Load(); got != 0 {
		t.Fatalf("OnExpired fired during transient error: %d", got)
	}
	if got := restored.Load(); got != 0 {
		t.Fatalf("OnRecovered fired during transient error: %d", got)
	}
}

// waitFor polls cond every 5ms until it returns true or timeout elapses.
func waitFor(cond func() bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("condition never became true within %s", timeout)
}
