package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/quota"
	"github.com/webvictim/prism/internal/tshwrap"
	usagepkg "github.com/webvictim/prism/internal/usage"
)

// The tenant's token quotas, read for display only. See internal/quota
// for what the cluster does and doesn't tell a client.
//
// Who can read them decides everything here. The cluster answers
// `llm_endpoint_quota` reads only for someone allowed `create` on
// `app_server` — a tenant's own user, through the `editor` role Beams
// gives them, but not prism's tbot identity, which is refused. So the
// quota is read as the user's tsh login, and only when there is a live
// one: without it tctl would fail anyway, and prism must never be what
// starts an SSO login. No login, no tctl, no permission — then nothing is
// shown, and nothing fails.

// Why a read didn't happen, as shown to the user.
const (
	quotaNoProxy     = "no proxy configured (`prism config set proxy <addr>`)"
	quotaNoLogin     = "no live tsh login (`tsh login --proxy=%s` to see the tenant's quota)"
	quotaNoTctl      = "tctl is not on PATH (it ships with Teleport)"
	quotaNotAllowed  = "your Teleport roles can't read the tenant's quota"
	quotaProxyMoved  = "the daemon was started for another proxy; restart it (`prism down && prism up`)"
	quotaTctlTimeout = 10 * time.Second
)

// quotaDeps is what a read needs from the world, so tests can show that
// with no login tctl is never run.
type quotaDeps struct {
	status func() (*tshwrap.Status, error)
	tctl   func(ctx context.Context, proxy string, args ...string) ([]byte, error)
}

var liveQuotaDeps = quotaDeps{status: tshwrap.StatusJSON, tctl: captureTctlQuiet}

// readQuota reads the quota for cfg.Proxy. problem is set, with a nil
// error, for the expected reasons there is nothing to read; err is a real
// failure (a timeout, a parse error).
//
// The cluster is always named: tctl gets --auth-server=<config.Proxy> and
// nothing else, never the sidecar's proxy or tsh's active profile, so the
// numbers can only ever be the configured tenant's.
func readQuota(ctx context.Context, cfg *config.Config, d quotaDeps) (eps []quota.Endpoint, problem string, err error) {
	if cfg == nil || cfg.Proxy == "" {
		return nil, quotaNoProxy, nil
	}
	// tshwrap is aimed at the proxy the process started with
	// (applyConfigEnv). A daemon outliving a `prism config set proxy`
	// would check one cluster's login and then read another's quota.
	if p := tshwrap.Proxy(); p != "" && p != cfg.Proxy {
		return nil, quotaProxyMoved, nil
	}
	// `tsh status` only reads the local profile; it never logs in.
	st, err := d.status()
	if err != nil {
		return nil, "", fmt.Errorf("tsh status: %w", err)
	}
	if !st.LoggedIn() || st.IsExpired() {
		return nil, fmt.Sprintf(quotaNoLogin, withDefaultPort(cfg.Proxy)), nil
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTctlTimeout)
	defer cancel()
	out, err := d.tctl(ctx, cfg.Proxy, "get", "llm_endpoint_quota", "--format=json")
	switch {
	case errors.Is(err, errNoTctl):
		return nil, quotaNoTctl, nil
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "access denied"):
		return nil, quotaNotAllowed, nil
	case err != nil:
		return nil, "", err
	}
	eps, err = quota.ParseTctl(out)
	return eps, "", err
}

var errNoTctl = errors.New("tctl is not on PATH")

// captureTctlQuiet is captureTctl for the daemon: there is no terminal to
// answer a prompt on, so stdin is closed (a prompt fails rather than
// hangs), stderr is kept rather than tee'd, and the call has a deadline.
// Reads need no per-session MFA, so nothing prism asks this way prompts.
func captureTctlQuiet(ctx context.Context, proxy string, args ...string) ([]byte, error) {
	if proxy == "" {
		// tctlArgs would drop --auth-server, and tctl would pick
		// whichever cluster the user's shell last logged into.
		return nil, errors.New("refusing to run tctl without a cluster to name")
	}
	bin, err := tshwrap.LookPathStrict("tctl")
	if err != nil {
		return nil, errNoTctl
	}
	cmd := exec.CommandContext(ctx, bin, tctlArgs(proxy, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tctl took longer than %s", quotaTctlTimeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}

// loadQuota is the cache for the configured proxy, or nil. Readers never
// fail on it: a quota is an extra, not something to stop a command for.
func loadQuota(cfg *config.Config) *quota.Cache {
	if cfg == nil {
		return nil
	}
	dir, err := config.Dir()
	if err != nil {
		return nil
	}
	c, _ := quota.Load(dir, cfg.Proxy)
	return c
}

// quotaOverrides parses `quota_limits`, ignoring a value that doesn't
// parse (config set validates it; a hand edit falls back to defaults).
func quotaOverrides(cfg *config.Config) map[string]uint64 {
	if cfg == nil || cfg.QuotaLimits == "" {
		return nil
	}
	m, err := quota.ParseLimits(cfg.QuotaLimits)
	if err != nil {
		return nil
	}
	return m
}

// --- the daemon's refresher ---

const (
	quotaPoll      = time.Minute
	quotaFirstTry  = 20 * time.Second
	quotaWakeGrace = time.Minute
	// quotaBusyEvery is how stale a busy daemon lets the numbers get. The
	// app service reports usage to auth every 2 minutes, so reading more
	// often only re-reads the same numbers.
	quotaBusyEvery = 2 * time.Minute
	// quotaIdleEvery is the rate with no traffic since the last read:
	// someone else's usage of the tenant still moves the numbers.
	quotaIdleEvery = 30 * time.Minute
	// quotaNudgeGap keeps a burst of refusals from becoming a burst of
	// tctl runs.
	quotaNudgeGap = 10 * time.Second
	quotaRetryMin = time.Minute
	quotaRetryMax = 10 * time.Minute
)

// quotaRefresher decides when to read the quota. Like modelsRefresher it
// is a pure state machine over wall-clock times, so a test can put the
// laptop to sleep: Go's timers stop with the monotonic clock while a Mac
// sleeps, and a long gap between ticks is a wake, after which the network
// and the login may not be back yet.
type quotaRefresher struct {
	next     time.Time
	lastTick time.Time
	last     time.Time // last attempt
	failures int
}

func newQuotaRefresher(now time.Time) *quotaRefresher {
	now = now.Round(0)
	return &quotaRefresher{next: now.Add(quotaFirstTry), lastTick: now}
}

// due reports whether to read on this tick. lastTraffic is when this
// machine last spent tokens (the usage file's mtime); nudged is an
// observed quota refusal, which skips the wait but not a wake's grace.
func (r *quotaRefresher) due(now, lastTraffic time.Time, nudged bool) bool {
	now = now.Round(0)
	if now.Sub(r.lastTick) > 3*quotaPoll {
		r.next = laterOf(r.next, now.Add(quotaWakeGrace))
	}
	r.lastTick = now
	since := now.Sub(r.last)
	// A refusal comes from a request that just reached the gateway, so
	// the network is up; only a failing read, or one moments ago, waits.
	if nudged && r.failures == 0 && !r.last.IsZero() && since >= quotaNudgeGap {
		return true
	}
	if now.Before(r.next) {
		return false
	}
	if r.last.IsZero() || r.failures > 0 {
		return true
	}
	if lastTraffic.Round(0).After(r.last) {
		return since >= quotaBusyEvery
	}
	return since >= quotaIdleEvery
}

// done records an attempt. A refusal for a reason only the user can fix
// (no login) is not a failure to back off from: it is rechecked on the
// ordinary schedule, so a fresh `tsh login` shows up within minutes.
func (r *quotaRefresher) done(now time.Time, err error) time.Duration {
	now = now.Round(0)
	r.last = now
	if err == nil {
		r.failures = 0
		r.next = now.Add(quotaBusyEvery)
		return quotaBusyEvery
	}
	r.failures++
	wait := min(quotaRetryMin<<min(r.failures-1, 4), quotaRetryMax)
	r.next = now.Add(wait)
	return wait
}

// lastUsageWrite is when this machine last recorded spending tokens.
func lastUsageWrite() time.Time {
	dir, err := usagepkg.Dir()
	if err != nil {
		return time.Time{}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}
	}
	var latest time.Time
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}

// runQuotaRefresher keeps <config dir>/quota.json current for as long as
// the daemon runs. The config is read on every tick.
func runQuotaRefresher(ctx context.Context, logger *log.Logger) {
	r := newQuotaRefresher(time.Now())
	t := time.NewTicker(quotaPoll)
	defer t.Stop()
	first := time.NewTimer(quotaFirstTry)
	defer first.Stop()
	var lastProblem string
	for {
		nudged := false
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-t.C:
		case <-quota.Nudged():
			nudged = true
		}
		now := time.Now()
		if !r.due(now, lastUsageWrite(), nudged) {
			continue
		}
		cfg, err := config.Load()
		if err != nil {
			continue
		}
		problem, err := refreshQuota(ctx, cfg, now)
		r.done(time.Now(), err)
		switch {
		case err != nil:
			logger.Printf("quota: could not read the tenant's quota (%v); keeping the last reading", err)
		case problem != "" && problem != lastProblem:
			logger.Printf("quota: not shown: %s", problem)
		}
		lastProblem = problem
	}
}

// refreshQuota reads the quota and records the outcome in the cache. The
// last good numbers survive a failure or a lapsed login: they carry their
// own age, and statusline dims them once old. With no proxy configured
// nothing is written, since there is no cluster to file it under.
func refreshQuota(ctx context.Context, cfg *config.Config, now time.Time) (problem string, err error) {
	eps, problem, err := readQuota(ctx, cfg, liveQuotaDeps)
	if cfg.Proxy == "" {
		return problem, err
	}
	dir, derr := config.Dir()
	if derr != nil {
		return problem, derr
	}
	c, _ := quota.Load(dir, cfg.Proxy)
	if c == nil {
		c = &quota.Cache{Proxy: cfg.Proxy}
	}
	c.CheckedAt = now
	switch {
	case err != nil:
		c.Problem = err.Error()
	case problem != "":
		c.Problem = problem
	default:
		c.Problem, c.FetchedAt, c.Endpoints = "", now, eps
	}
	if serr := quota.Save(dir, c); serr != nil && err == nil {
		err = fmt.Errorf("writing %s: %w", filepath.Join(dir, quota.FileName), serr)
	}
	return problem, err
}
