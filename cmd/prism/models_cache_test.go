package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/tshwrap"
)

// clock drives a refresher through ticks a minute apart, the way the
// daemon's ticker does while the machine is awake.
type clock struct {
	t   *testing.T
	r   *modelsRefresher
	now time.Time
}

func (c *clock) tick(fetched time.Time) bool {
	c.now = c.now.Add(modelsPoll)
	return c.r.due(c.now, fetched)
}

// minutesUntilDue ticks until the refresher wants to refresh.
func (c *clock) minutesUntilDue(fetched time.Time, limit int) int {
	c.t.Helper()
	for i := 1; i <= limit; i++ {
		if c.tick(fetched) {
			return i
		}
	}
	c.t.Fatalf("not due within %d minutes", limit)
	return 0
}

var start = time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)

func TestModelsRefresherFirstTryAndHourly(t *testing.T) {
	c := &clock{t: t, r: newModelsRefresher(start), now: start}
	// The first tick after the first-try delay refreshes an empty cache.
	if !c.r.due(start.Add(modelsFirstTry), time.Time{}) {
		t.Fatal("a fresh daemon with no cache didn't refresh promptly")
	}
	c.now = start.Add(modelsFirstTry)
	c.r.done(c.now, nil)
	fetched := c.now

	// Then not again for an hour of wall-clock time.
	if got := c.minutesUntilDue(fetched, 120); got != 60 {
		t.Errorf("refreshed after %d minutes, want 60", got)
	}
}

// A night's sleep stops Go's monotonic clock on macOS, so an hourly timer
// would fire an hour of awake time later. Comparing wall clocks sees the
// list is stale on the first tick after waking — then waits out the grace,
// because the network and tbot's identity need a moment.
func TestModelsRefresherAfterSleep(t *testing.T) {
	c := &clock{t: t, r: newModelsRefresher(start), now: start}
	c.r.done(start, nil)
	fetched := start
	for range 10 {
		if c.tick(fetched) {
			t.Fatal("refreshed while the list was fresh")
		}
	}

	// Lid closed for eight hours: the next tick arrives with a big gap.
	c.now = c.now.Add(8 * time.Hour)
	if c.r.due(c.now, fetched) {
		t.Error("refreshed the instant it woke, before the network is back")
	}
	if got := c.minutesUntilDue(fetched, 5); got != 1 {
		t.Errorf("refreshed %d minutes after waking, want 1 (the grace)", got)
	}
}

// `prism models` writes the cache too; the daemon must count that rather
// than refresh again on its own schedule.
func TestModelsRefresherCountsManualRefresh(t *testing.T) {
	c := &clock{t: t, r: newModelsRefresher(start), now: start}
	c.r.done(start, nil)
	manual := start.Add(30 * time.Minute)
	c.now = manual
	if got := c.minutesUntilDue(manual, 120); got != 60 {
		t.Errorf("refreshed %d minutes after a manual refresh, want 60", got)
	}
}

// A daemon starting minutes after `prism models` wrote the cache waits
// out the rest of the hour — and says so, since a refresher that's
// deferring looks just like one that isn't running.
func TestModelsRefresherStartsBesideFreshCache(t *testing.T) {
	r := newModelsRefresher(start)
	written := start.Add(-3 * time.Minute)
	first := start.Add(modelsFirstTry)
	if r.due(first, written) {
		t.Fatal("refreshed a three-minute-old cache")
	}
	if !r.deferredFresh {
		t.Error("deferring to a fresh cache should say so, for the daemon to log it")
	}
	if want := written.Add(modelsRefreshEvery); !r.next.Equal(want) {
		t.Errorf("next refresh at %s, want %s (an hour after it was written)", r.next, want)
	}
	// Waiting out the start delay is not the same thing.
	r2 := newModelsRefresher(start)
	if r2.due(start.Add(time.Second), written); r2.deferredFresh {
		t.Error("deferredFresh set while still inside the start delay")
	}
}

func TestModelsRefresherBacksOff(t *testing.T) {
	r := newModelsRefresher(start)
	var waits []time.Duration
	for range 6 {
		waits = append(waits, r.done(start, os.ErrDeadlineExceeded))
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("backoff = %v, want %v", waits, want)
		}
	}
	if got := r.done(start, nil); got != modelsRefreshEvery || r.failures != 0 {
		t.Errorf("a success should reset the backoff: wait %s, failures %d", got, r.failures)
	}
}

func TestModelsCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	on := &config.Config{}

	if _, err := cachedApps(on); err == nil || !strings.Contains(err.Error(), "hasn't read") {
		t.Errorf("no cache: err = %v", err)
	}

	var app tshwrap.App
	app.Metadata.Name = "openai"
	app.Spec.Inference = &tshwrap.Inference{Format: "openai", Models: []tshwrap.InferenceModel{{Name: "gpt-6-luna", ProviderName: "openai.gpt-6-luna"}}}
	if err := saveModelsCache([]tshwrap.App{app}, listedAsTbot, start); err != nil {
		t.Fatal(err)
	}
	c, err := loadModelsCache()
	if err != nil {
		t.Fatal(err)
	}
	if !c.FetchedAt.Equal(start) || c.ListedAs != listedAsTbot || len(c.Apps) != 1 ||
		c.Apps[0].Spec.Inference.Models[0].ProviderName != "openai.gpt-6-luna" {
		t.Errorf("round trip = %+v", c)
	}

	path, _ := modelsCachePath()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("cache mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	// Written by rename, so no half-written temp file is left behind.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".models-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}

	if apps, err := cachedApps(on); err != nil || len(apps) != 1 {
		t.Errorf("cachedApps = %v, %v", apps, err)
	}
	// Off means the cache is ignored: nothing keeps it current.
	off := false
	if _, err := cachedApps(&config.Config{ModelDiscovery: &off}); err == nil || !strings.Contains(err.Error(), "off") {
		t.Errorf("discovery off should ignore the cache: err = %v", err)
	}
}
