package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/tshwrap"
)

// The cluster's served models are cached in <config dir>/models.json by
// the daemon, so nothing on a launch path waits on the cluster: listing
// apps takes about a second when all is well and up to the 10s timeout
// when it isn't, which is too long to put in front of `prism codex` or
// `prism test`. `prism models` is the exception — it queries live, and
// writes what it reads back here.
//
// All of it is governed by model_discovery. Off means prism never
// reads the list on its own: no background refresh, and the cache is
// ignored, since nothing would keep it current.

const modelsCacheFile = "models.json"

// modelsCache is the file's shape.
type modelsCache struct {
	FetchedAt time.Time     `json:"fetched_at"`
	ListedAs  string        `json:"listed_as"`
	Apps      []tshwrap.App `json:"apps"`
}

func modelsCachePath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, modelsCacheFile), nil
}

// loadModelsCache reads the cache, which is fine to be missing — a fresh
// daemon writes it within a minute or so.
func loadModelsCache() (*modelsCache, error) {
	path, err := modelsCachePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c modelsCache
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// cachedApps is the cached app list when model discovery is on, and
// nothing when it's off or there is no cache yet.
func cachedApps(cfg *config.Config) ([]tshwrap.App, error) {
	if !cfg.ModelDiscoveryEnabled() {
		return nil, errors.New("model discovery is off (model_discovery=false)")
	}
	c, err := loadModelsCache()
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("prism hasn't read the cluster's model list yet — the daemon does within a couple of minutes of starting")
	}
	if err != nil {
		return nil, err
	}
	return c.Apps, nil
}

// saveModelsCache writes the cache atomically: a reader mid-write would
// otherwise see half a file, and that reader is usually a launch.
func saveModelsCache(apps []tshwrap.App, listedAs string, now time.Time) error {
	path, err := modelsCachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(modelsCache{FetchedAt: now.UTC(), ListedAs: listedAs, Apps: apps}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".models-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// --- the daemon's refresher ---

const (
	modelsRefreshEvery = time.Hour
	modelsPoll         = time.Minute
	modelsFirstTry     = 15 * time.Second // tbot writes its identity seconds after joining
	modelsWakeGrace    = time.Minute
	modelsRetryMin     = time.Minute
	modelsRetryMax     = 10 * time.Minute
)

// modelsRefresher decides when to refresh the cache. It is a pure state
// machine over wall-clock times so a test can put the laptop to sleep.
//
// Sleep is why it polls rather than setting an hourly timer. Go's timers
// run on the monotonic clock, which on macOS stops while the machine
// sleeps, so an hour-long timer set before a night's sleep fires an hour
// of *awake* time later. Comparing wall-clock times on a short tick sees a
// stale list on the first tick after waking. A long gap between ticks is
// itself a wake, and gets a grace period: right after one the network may
// not be back, and tbot's short-lived identity has expired until tbot
// renews it, so an immediate try would only fail.
type modelsRefresher struct {
	next     time.Time // wall clock; no attempt before this
	lastTick time.Time
	failures int
	// deferredFresh is set when the last due() said no because the cache
	// was fresh, rather than waiting out the start delay or a wake grace.
	deferredFresh bool
}

func newModelsRefresher(now time.Time) *modelsRefresher {
	now = now.Round(0)
	return &modelsRefresher{next: now.Add(modelsFirstTry), lastTick: now}
}

// due reports whether to attempt a refresh on this tick. fetched is when
// the cache was last written, by anyone — `prism models` included, which
// is why it is read rather than remembered.
func (r *modelsRefresher) due(now, fetched time.Time) bool {
	now = now.Round(0) // strip the monotonic reading: compare wall clocks
	if now.Sub(r.lastTick) > 3*modelsPoll {
		r.next = laterOf(r.next, now.Add(modelsWakeGrace))
	}
	r.lastTick = now
	r.deferredFresh = false
	if now.Before(r.next) {
		return false
	}
	if !fetched.IsZero() && now.Sub(fetched.Round(0)) < modelsRefreshEvery {
		r.next = fetched.Round(0).Add(modelsRefreshEvery)
		r.deferredFresh = true
		return false
	}
	return true
}

// done records an attempt's outcome and schedules the next.
func (r *modelsRefresher) done(now time.Time, err error) time.Duration {
	now = now.Round(0)
	if err == nil {
		r.failures = 0
		r.next = now.Add(modelsRefreshEvery)
		return modelsRefreshEvery
	}
	r.failures++
	wait := modelsRetryMin << min(r.failures-1, 4)
	wait = min(wait, modelsRetryMax)
	r.next = now.Add(wait)
	return wait
}

func laterOf(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// runModelsRefresher keeps the cache current for as long as the daemon
// runs. The config is read on every tick, so turning discovery off (or
// on) needs no restart.
func runModelsRefresher(ctx context.Context, logger *log.Logger) {
	r := newModelsRefresher(time.Now())
	// Said once per daemon: a refresher deferring to a fresh cache is
	// otherwise indistinguishable from one that isn't running.
	announced := false
	t := time.NewTicker(modelsPoll)
	defer t.Stop()
	first := time.NewTimer(modelsFirstTry)
	defer first.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-t.C:
		}
		cfg, err := config.Load()
		if err != nil || !cfg.ModelDiscoveryEnabled() {
			continue
		}
		var fetched time.Time
		var cachedAs string
		if c, err := loadModelsCache(); err == nil {
			fetched, cachedAs = c.FetchedAt, c.ListedAs
		}
		now := time.Now()
		if !r.due(now, fetched) {
			if r.deferredFresh && !announced {
				announced = true
				logger.Printf("models: the cached model list is %s old (as %s); next refresh around %s",
					now.Sub(fetched).Round(time.Minute), cachedAs, r.next.Local().Format("15:04"))
			}
			continue
		}
		announced = true
		apps, listedAs, err := clusterAppsFrom(cfg)
		if err == nil {
			err = saveModelsCache(apps, listedAs, now)
		}
		wait := r.done(time.Now(), err)
		if err != nil {
			logger.Printf("models: could not refresh the cluster's model list (%v); keeping the last one, retrying in %s", err, wait)
			continue
		}
		served := servedModels(apps)
		logger.Printf("models: refreshed the cluster's model list as %s (anthropic %d, openai %d)",
			listedAs, len(served[router.AnthropicAppName]), len(served[router.OpenAIAppName]))
	}
}
