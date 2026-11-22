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
// Two reads, with different permissions, so each degrades on its own:
//
//   - the limits are in the apps listing (`inference.quota`, Teleport
//     v19), which anyone who can use the apps may read: prism's tbot
//     identity, the user's login, a beam's identity. With an 18.x tsh the
//     field is absent and prism's defaults stand in.
//   - the usage is `llm_endpoint_quota`, which the cluster answers only
//     for someone allowed `create` on `app_server` — a tenant's own user,
//     through the `editor` role Beams gives them, or a beam carrying that
//     user's standing roles. prism's tbot identity and a `beam-user`-only
//     beam are refused. So it is read as the tsh login, and only when
//     there is a live one: without it tctl would fail anyway, and prism
//     must never be what starts an SSO login.
//
// Whatever can't be read is simply not shown, with the reason, and
// nothing fails.

// Why the usage wasn't read, as shown to the user.
const (
	quotaNoProxy = "no proxy configured (`prism config set proxy <addr>`)"
	quotaNoLogin = quotaNoLoginPrefix + " (`tsh login --proxy=%s` to see the tenant's usage)"
	// quotaNoLoginPrefix picks a missing login out of a cached problem.
	quotaNoLoginPrefix = "no live tsh login"
	quotaNoTctl        = "tctl is not on PATH (it ships with Teleport)"
	quotaNotAllowed    = "your Teleport roles can't read the tenant's usage (it needs the editor role's app_server access)"
	quotaProxyMoved    = "the daemon was started for another proxy; restart it (`prism down && prism up`)"
	quotaTctlTimeout   = 10 * time.Second
)

// quotaProxy is the cluster whose quota prism reads: the configured proxy,
// which is always the authority. Inside a beam with none configured, it is
// the beam's own cluster, which the beam names in TELEPORT_PROXY: a beam
// belongs to exactly one cluster, so there is nothing to confuse it with.
func quotaProxy(cfg *config.Config) string {
	if cfg != nil && cfg.Proxy != "" {
		return cfg.Proxy
	}
	if os.Getenv("BEAM_ID") != "" {
		return os.Getenv("TELEPORT_PROXY")
	}
	return ""
}

// quotaDeps is what a read needs from the world, so tests can show that
// with no login tctl is never run.
type quotaDeps struct {
	status func() (*tshwrap.Status, error)
	tctl   func(ctx context.Context, proxy string, args ...string) ([]byte, error)
	apps   func(cfg *config.Config) ([]tshwrap.App, error)
}

var liveQuotaDeps = quotaDeps{status: tshwrap.StatusJSON, tctl: captureTctlQuiet, apps: quotaApps}

// quotaApps is the apps listing the limits are read from: the daemon's
// model cache when there is one, which is refreshed hourly and limits
// rarely change; else a listing now (tbot identity, then a live login —
// clusterAppsFrom never starts a login), written back to the cache.
func quotaApps(cfg *config.Config) ([]tshwrap.App, error) {
	if apps, err := cachedApps(cfg); err == nil && len(apps) > 0 {
		return apps, nil
	}
	apps, listedAs, err := clusterAppsFrom(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.ModelDiscoveryEnabled() {
		_ = saveModelsCache(apps, listedAs, time.Now())
	}
	return apps, nil
}

// appLimits are the limits the apps listing carries, which is none at
// all from an 18.x tsh.
func appLimits(apps []tshwrap.App) []quota.AppLimits {
	var out []quota.AppLimits
	for _, a := range apps {
		if a.Spec.Inference == nil || a.Spec.Inference.Quota == nil {
			continue
		}
		al := quota.AppLimits{App: a.Metadata.Name}
		for _, iv := range a.Spec.Inference.Quota.Intervals {
			al.Intervals = append(al.Intervals, quota.LimitInterval{
				Interval:     iv.Interval,
				InputTokens:  uint64(iv.InputTokens),
				OutputTokens: uint64(iv.OutputTokens),
			})
		}
		if len(al.Intervals) > 0 {
			out = append(out, al)
		}
	}
	return out
}

// quotaReading is one read: the usage, the limits, and why the usage is
// missing when it is. Limits can be present without usage, and vice versa.
type quotaReading struct {
	Endpoints []quota.Endpoint
	Limits    []quota.AppLimits
	Problem   string
}

// readQuota reads the quota for quotaProxy(cfg). Problem is set, with a
// nil error, for the expected reasons the usage can't be read; err is a
// real failure (a timeout, a parse error).
//
// The cluster is always named: tctl gets --auth-server=<proxy> and
// nothing else, never the sidecar's proxy or tsh's active profile, so the
// numbers can only ever be the configured tenant's.
func readQuota(ctx context.Context, cfg *config.Config, d quotaDeps) (r quotaReading, err error) {
	proxy := quotaProxy(cfg)
	if proxy == "" {
		r.Problem = quotaNoProxy
		return r, nil
	}
	// tshwrap is aimed at the proxy the process started with
	// (applyConfigEnv). A daemon outliving a `prism config set proxy`
	// would check one cluster's login and then read another's quota.
	if p := tshwrap.Proxy(); p != "" && p != proxy {
		r.Problem = quotaProxyMoved
		return r, nil
	}
	// The limits first: they need less, so a refusal of the usage below
	// still leaves them. A failed listing only means the defaults apply.
	if d.apps != nil {
		if apps, err := d.apps(cfg); err == nil {
			r.Limits = appLimits(apps)
		}
	}
	// `tsh status` only reads the local profile; it never logs in.
	st, err := d.status()
	if err != nil {
		return r, fmt.Errorf("tsh status: %w", err)
	}
	if !st.LoggedIn() || st.IsExpired() {
		r.Problem = fmt.Sprintf(quotaNoLogin, withDefaultPort(proxy))
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, quotaTctlTimeout)
	defer cancel()
	out, err := d.tctl(ctx, proxy, "get", "llm_endpoint_quota", "--format=json")
	switch {
	case errors.Is(err, errNoTctl):
		r.Problem = quotaNoTctl
		return r, nil
	case err != nil && strings.Contains(strings.ToLower(err.Error()), "access denied"):
		r.Problem = quotaNotAllowed
		return r, nil
	case err != nil:
		return r, err
	}
	r.Endpoints, err = quota.ParseTctl(out)
	return r, err
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
	c, _ := quota.Load(dir, quotaProxy(cfg))
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
// own age, and statusline dims them once old. So do the last limits read,
// through a listing that failed. With no proxy nothing is written, since
// there is no cluster to file it under.
func refreshQuota(ctx context.Context, cfg *config.Config, now time.Time) (problem string, err error) {
	r, err := readQuota(ctx, cfg, liveQuotaDeps)
	proxy := quotaProxy(cfg)
	if proxy == "" {
		return r.Problem, err
	}
	dir, derr := config.Dir()
	if derr != nil {
		return r.Problem, derr
	}
	c, _ := quota.Load(dir, proxy)
	if c == nil {
		c = &quota.Cache{Proxy: proxy}
	}
	c.CheckedAt = now
	if r.Limits != nil {
		c.Limits = r.Limits
	}
	switch {
	case err != nil:
		c.Problem = err.Error()
	case r.Problem != "":
		c.Problem = r.Problem
	default:
		c.Problem, c.FetchedAt, c.Endpoints = "", now, r.Endpoints
	}
	if serr := quota.Save(dir, c); serr != nil && err == nil {
		err = fmt.Errorf("writing %s: %w", filepath.Join(dir, quota.FileName), serr)
	}
	return r.Problem, err
}
