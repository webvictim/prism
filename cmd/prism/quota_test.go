package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/tshwrap"
)

const quotaFixture = `[{"kind":"llm_endpoint_quota","metadata":{"name":"anthropic"},
  "status":{"active_windows":[{"interval_name":"daily","starts_at":"2026-11-22T00:00:00Z",
  "ends_at":"2026-11-23T00:00:00Z","input_tokens":"6752","output_tokens":"56067"}]}}]`

// fakeQuotaDeps records whether tctl ran, and against which cluster.
type fakeQuotaDeps struct {
	st       *tshwrap.Status
	stErr    error
	out      string
	tctlErr  error
	tctlRan  bool
	gotProxy string
	gotArgs  []string
}

func (f *fakeQuotaDeps) deps() quotaDeps {
	return quotaDeps{
		status: func() (*tshwrap.Status, error) { return f.st, f.stErr },
		tctl: func(_ context.Context, proxy string, args ...string) ([]byte, error) {
			f.tctlRan, f.gotProxy, f.gotArgs = true, proxy, args
			return []byte(f.out), f.tctlErr
		},
	}
}

func liveLogin() *tshwrap.Status {
	return &tshwrap.Status{Cluster: "fresh-beans.beams.sh", ValidUntil: time.Now().Add(time.Hour)}
}

func TestReadQuotaNeverRunsTctlWithoutALogin(t *testing.T) {
	cfg := &config.Config{Proxy: "fresh-beans.beams.sh:443"}
	for name, st := range map[string]*tshwrap.Status{
		"never logged in": {},
		"expired":         {Cluster: "fresh-beans.beams.sh", ValidUntil: time.Now().Add(-time.Minute)},
	} {
		f := &fakeQuotaDeps{st: st}
		eps, problem, err := readQuota(context.Background(), cfg, f.deps())
		if err != nil || eps != nil || problem == "" {
			t.Errorf("%s: got %v %q %v", name, eps, problem, err)
		}
		if f.tctlRan {
			// Without a live login tctl could start an SSO login.
			t.Errorf("%s: tctl ran", name)
		}
	}
}

func TestReadQuotaNeedsAConfiguredProxy(t *testing.T) {
	f := &fakeQuotaDeps{st: liveLogin(), out: quotaFixture}
	_, problem, err := readQuota(context.Background(), &config.Config{}, f.deps())
	if err != nil || problem != quotaNoProxy || f.tctlRan {
		t.Errorf("got %q %v, tctl ran: %v", problem, err, f.tctlRan)
	}
}

func TestReadQuotaNamesTheConfiguredCluster(t *testing.T) {
	cfg := &config.Config{Proxy: "fresh-beans.beams.sh:443"}
	f := &fakeQuotaDeps{st: liveLogin(), out: quotaFixture}
	eps, problem, err := readQuota(context.Background(), cfg, f.deps())
	if err != nil || problem != "" || len(eps) != 1 || eps[0].Windows[0].OutputTokens != 56067 {
		t.Fatalf("got %+v %q %v", eps, problem, err)
	}
	if f.gotProxy != cfg.Proxy {
		t.Errorf("tctl aimed at %q, want the configured proxy", f.gotProxy)
	}
	if got := tctlArgs(f.gotProxy, f.gotArgs...); got[0] != "--auth-server=fresh-beans.beams.sh:443" {
		t.Errorf("tctl args %v", got)
	}
}

func TestReadQuotaExpectedRefusals(t *testing.T) {
	cfg := &config.Config{Proxy: "fresh-beans.beams.sh:443"}
	for name, tc := range map[string]struct {
		err     error
		problem string
		isErr   bool
	}{
		"tbot-like denial": {errors.New(`ERROR: getting resource "" of type "llm_endpoint_quota"` + "\n\taccess denied to perform action \"create\" on \"app_server\""), quotaNotAllowed, false},
		"no tctl":          {errNoTctl, quotaNoTctl, false},
		"other failure":    {errors.New("connection refused"), "", true},
	} {
		f := &fakeQuotaDeps{st: liveLogin(), tctlErr: tc.err}
		_, problem, err := readQuota(context.Background(), cfg, f.deps())
		if problem != tc.problem || (err != nil) != tc.isErr {
			t.Errorf("%s: got %q %v", name, problem, err)
		}
	}
}

func TestCaptureTctlQuietRefusesWithoutAProxy(t *testing.T) {
	if _, err := captureTctlQuiet(context.Background(), "", "get", "llm_endpoint_quota"); err == nil {
		t.Fatal("ran tctl without naming a cluster")
	}
}

func TestQuotaRefresherSchedule(t *testing.T) {
	t0 := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	r := newQuotaRefresher(t0)
	if r.due(t0.Add(10*time.Second), time.Time{}, false) {
		t.Fatal("due before the first try")
	}
	now := t0.Add(quotaFirstTry)
	if !r.due(now, time.Time{}, false) {
		t.Fatal("first try not due")
	}
	r.done(now, nil)

	// Idle: nothing until quotaIdleEvery.
	tick := func(d time.Duration, traffic time.Time, nudged bool) bool {
		now = now.Add(d)
		return r.due(now, traffic, nudged)
	}
	if tick(time.Minute, time.Time{}, false) || tick(time.Minute, time.Time{}, false) {
		t.Fatal("idle refresh after 2 minutes")
	}
	// Traffic since the last read: due once quotaBusyEvery has passed.
	if !tick(time.Minute, now, false) {
		t.Fatal("busy refresh not due after 3 minutes")
	}
	r.done(now, nil)

	// A nudge right away is ignored, a few seconds later honoured.
	if tick(time.Second, time.Time{}, true) {
		t.Fatal("nudge inside the gap honoured")
	}
	if !tick(quotaNudgeGap, time.Time{}, true) {
		t.Fatal("nudge ignored")
	}
	r.done(now, nil)

	// A night's sleep: the first tick after waking waits out the grace.
	if tick(8*time.Hour, time.Time{}, false) {
		t.Fatal("due straight after a wake")
	}
	if !tick(quotaWakeGrace, time.Time{}, false) {
		t.Fatal("not due after the wake grace")
	}

	// Failures back off.
	if w := r.done(now, errors.New("boom")); w != quotaRetryMin {
		t.Fatalf("first retry in %s", w)
	}
	if w := r.done(now, errors.New("boom")); w != 2*quotaRetryMin {
		t.Fatalf("second retry in %s", w)
	}
	for range 10 {
		r.done(now, errors.New("boom"))
	}
	if w := r.done(now, errors.New("boom")); w != quotaRetryMax {
		t.Fatalf("retry capped at %s", w)
	}
}
