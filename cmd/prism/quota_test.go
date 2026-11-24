package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/quota"
	"github.com/webvictim/prism/internal/tshwrap"
	usagepkg "github.com/webvictim/prism/internal/usage"
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
	apps     []tshwrap.App
	appsErr  error
}

func (f *fakeQuotaDeps) deps() quotaDeps {
	return quotaDeps{
		apps:   func(*config.Config) ([]tshwrap.App, error) { return f.apps, f.appsErr },
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
		r, err := readQuota(context.Background(), cfg, f.deps())
		if err != nil || r.Endpoints != nil || r.Problem == "" {
			t.Errorf("%s: got %+v %v", name, r, err)
		}
		if f.tctlRan {
			// Without a live login tctl could start an SSO login.
			t.Errorf("%s: tctl ran", name)
		}
	}
}

func TestReadQuotaNeedsAConfiguredProxy(t *testing.T) {
	f := &fakeQuotaDeps{st: liveLogin(), out: quotaFixture}
	t.Setenv("BEAM_ID", "")
	r, err := readQuota(context.Background(), &config.Config{}, f.deps())
	if err != nil || r.Problem != quotaNoProxy || f.tctlRan {
		t.Errorf("got %q %v, tctl ran: %v", r.Problem, err, f.tctlRan)
	}
}

func TestReadQuotaNamesTheConfiguredCluster(t *testing.T) {
	cfg := &config.Config{Proxy: "fresh-beans.beams.sh:443"}
	f := &fakeQuotaDeps{st: liveLogin(), out: quotaFixture}
	r, err := readQuota(context.Background(), cfg, f.deps())
	if err != nil || r.Problem != "" || len(r.Endpoints) != 1 || r.Endpoints[0].Windows[0].OutputTokens != 56067 {
		t.Fatalf("got %+v %v", r, err)
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
		r, err := readQuota(context.Background(), cfg, f.deps())
		if r.Problem != tc.problem || (err != nil) != tc.isErr {
			t.Errorf("%s: got %q %v", name, r.Problem, err)
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

func writeUsageRecords(t *testing.T, recs ...usagepkg.Record) {
	t.Helper()
	dir, err := usagepkg.Dir()
	if err != nil {
		t.Fatal(err)
	}
	w, err := usagepkg.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		w.Write(r)
	}
	_ = w.Close()
}

func TestEstimatedRows(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const proxy = "fresh-beans.beams.sh:443"
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	writeUsageRecords(t,
		usagepkg.Record{Timestamp: now.Add(-time.Minute), Backend: "anthropic", Proxy: proxy, InputTokens: 10, OutputTokens: 1000},
		usagepkg.Record{Timestamp: now.Add(-2 * time.Minute), Backend: "anthropic", Proxy: proxy, InputTokens: 5, OutputTokens: 500},
		// Another cluster's, and yesterday's, don't count.
		usagepkg.Record{Timestamp: now.Add(-time.Minute), Backend: "anthropic", Proxy: "odd-firefly.beams.sh:443", OutputTokens: 1e6},
		usagepkg.Record{Timestamp: today.Add(-time.Hour), Backend: "anthropic", Proxy: proxy, OutputTokens: 1e6},
	)
	cfg := &config.Config{Proxy: proxy}
	// The last good reading was yesterday's daily window; the login has
	// since lapsed.
	ys, ye := today.Add(-24*time.Hour), today
	c := &quota.Cache{Proxy: proxy, FetchedAt: ys.Add(time.Hour), Problem: "no live tsh login",
		Endpoints: []quota.Endpoint{{Name: "anthropic", Windows: []quota.Window{{Interval: "daily", StartsAt: &ys, EndsAt: &ye, OutputTokens: 77}}}}}

	rows := quotaRows(cfg, c, now)
	if len(rows) != 1 || !rows[0].Estimated || rows[0].App != "anthropic" {
		t.Fatalf("rows: %+v", rows)
	}
	r := rows[0]
	if r.Window.OutputTokens != 1500 || r.Window.InputTokens != 15 || !r.Window.StartsAt.Equal(today) {
		t.Errorf("counted %+v", r.Window)
	}
	if !r.HasPct || r.Pct != 0.03 {
		t.Errorf("pct %v against %+v", r.Pct, r.Limit)
	}
	// openai has no records in the window: no row, rather than a 0%.
	for _, row := range rows {
		if row.App == "openai" {
			t.Error("estimated an app this machine didn't use")
		}
	}

	// A read that worked and found no quota means there is none.
	none := &quota.Cache{Proxy: proxy, FetchedAt: now}
	if rows := quotaRows(cfg, none, now); len(rows) != 0 {
		t.Errorf("estimated for a cluster without quotas: %+v", rows)
	}
	// No evidence at all: nothing...
	if rows := quotaRows(cfg, &quota.Cache{Proxy: proxy, Problem: "no live tsh login"}, now); len(rows) != 0 {
		t.Errorf("estimated without evidence: %+v", rows)
	}
	// ...unless quota_limits says there is a quota.
	cfg.QuotaLimits = "anthropic=5M"
	if rows := quotaRows(cfg, nil, now); len(rows) != 1 || rows[0].Limit.Source != quota.SourceConfig {
		t.Errorf("with quota_limits: %+v", rows)
	}
}

func TestEstimateIsMarkedEverywhere(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const proxy = "fresh-beans.beams.sh:443"
	now := time.Now()
	writeUsageRecords(t, usagepkg.Record{Timestamp: now.Add(-time.Minute), Backend: "anthropic", Proxy: proxy, OutputTokens: 250_000})
	cfg := &config.Config{Proxy: proxy, QuotaLimits: "anthropic=5M"}

	var buf bytes.Buffer
	printQuotaSection(&buf, cfg, now)
	if out := buf.String(); !strings.Contains(out, "≈250.0k") || !strings.Contains(out, "≈5.0%") || !strings.Contains(out, "usually higher") {
		t.Errorf("prism usage:\n%s", out)
	}
	if line := quotaStatusLine(cfg, now); !strings.Contains(line, "≈5.0%") || !strings.Contains(line, "estimated") {
		t.Errorf("status line: %q", line)
	}
	q := buildQuotaJSON(cfg, now)
	if q == nil || q.FetchedAt != nil || len(q.Apps) != 1 || !q.Apps[0].Approximate {
		t.Errorf("json: %+v", q)
	}
}

// A shell left exporting another cluster's TELEPORT_PROXY must not label
// the daemon's records: the configured proxy is where tsh actually goes.
func TestDaemonProxyPrefersTheConfiguredOne(t *testing.T) {
	defer tshwrap.SetProxy(tshwrap.Proxy())
	t.Setenv("TELEPORT_PROXY", "odd-firefly.beams.sh:443")
	tshwrap.SetProxy("fresh-beans.beams.sh:443")
	if got := daemonProxy(); got != "fresh-beans.beams.sh:443" {
		t.Errorf("got %q", got)
	}
	tshwrap.SetProxy("")
	if got := daemonProxy(); got != "odd-firefly.beams.sh:443" {
		t.Errorf("with nothing configured, the environment counts: %q", got)
	}
}

func TestStatuslineRequestReadIsBounded(t *testing.T) {
	r, w := io.Pipe() // held open, never written: a stalled caller
	defer w.Close()
	start := time.Now()
	b, err := readStatuslineRequest(r, 50*time.Millisecond)
	if err != nil || string(b) != statuslineEmptyRequest || time.Since(start) > time.Second {
		t.Fatalf("stalled stdin: %q %v after %v", b, err, time.Since(start))
	}
	b, err = readStatuslineRequest(strings.NewReader(`{"version":1,"key":"k"}`), time.Second)
	if err != nil || string(b) != `{"version":1,"key":"k"}` {
		t.Fatalf("normal request: %q %v", b, err)
	}
	if b, _ := readStatuslineRequest(strings.NewReader(""), time.Second); string(b) != statuslineEmptyRequest {
		t.Fatalf("empty stdin: %q", b)
	}
}

// A reading the daemon can no longer refresh (the login lapsed) is shown
// as an estimate: the tenant's count then, plus this machine's since.
func TestStaleReadingIsToppedUp(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const proxy = "fresh-beans.beams.sh:443"
	now := time.Now().UTC()
	start := now.Add(-6 * time.Hour)
	end := start.Add(24 * time.Hour)
	readAt := now.Add(-4 * time.Hour)
	writeUsageRecords(t,
		usagepkg.Record{Timestamp: readAt.Add(-time.Minute), Backend: "anthropic", Proxy: proxy, OutputTokens: 9e6}, // already in the reading
		usagepkg.Record{Timestamp: now.Add(-time.Hour), Backend: "anthropic", Proxy: proxy, InputTokens: 7, OutputTokens: 250_000},
	)
	cfg := &config.Config{Proxy: proxy}
	c := &quota.Cache{Proxy: proxy, FetchedAt: readAt, Problem: fmt.Sprintf(quotaNoLogin, proxy),
		Endpoints: []quota.Endpoint{
			{Name: "anthropic", Windows: []quota.Window{{Interval: "daily", StartsAt: &start, EndsAt: &end, InputTokens: 100, OutputTokens: 750_000}}},
			{Name: "openai", Windows: []quota.Window{{Interval: "daily", StartsAt: &start, EndsAt: &end, OutputTokens: 5}}},
		}}

	rows := quotaRows(cfg, c, now)
	if len(rows) != 2 || !rowsEstimated(rows) || !rowsReadAt(rows).Equal(readAt) {
		t.Fatalf("rows: %+v", rows)
	}
	if w := rows[0].Window; w.OutputTokens != 1_000_000 || w.InputTokens != 107 || rows[0].Pct != 20 {
		t.Errorf("anthropic: %+v %v", w, rows[0].Pct)
	}
	// No traffic since: still the reading, still marked.
	if w := rows[1].Window; w.OutputTokens != 5 || !rows[1].Estimated {
		t.Errorf("openai: %+v", rows[1])
	}
	if got := statuslineLoginNote(c); got != "tsh login expired" {
		t.Errorf("note %q", got)
	}
	// A recent reading stands as it is.
	c.FetchedAt = now.Add(-10 * time.Minute)
	if rows := quotaRows(cfg, c, now); rowsEstimated(rows) || rows[0].Window.OutputTokens != 750_000 {
		t.Errorf("fresh reading: %+v", rows)
	}
	for problem, want := range map[string]string{"": "", quotaNotAllowed: "", fmt.Sprintf(quotaNoLogin, proxy): "tsh login expired"} {
		if got := statuslineLoginNote(&quota.Cache{FetchedAt: now, Problem: problem}); got != want {
			t.Errorf("%q: note %q", problem, got)
		}
	}
	if got := statuslineLoginNote(&quota.Cache{Problem: fmt.Sprintf(quotaNoLogin, proxy)}); got != "no tsh login" {
		t.Errorf("never read: %q", got)
	}
}

// appsListing is `tsh apps ls --format=json` from a v19 tsh, as a beam's
// tsh printed it on fresh-beans, trimmed to the inference block.
const appsListing = `[
 {"metadata":{"name":"anthropic"},"spec":{"inference":{"format":"anthropic","provider":"bedrock",
   "models":[{"name":"claude-opus-5-5"}],
   "quota":{"requests":{},"intervals":[{"interval":"daily","input_tokens":35000000,"output_tokens":5000000,"reset_options":{}}]}}}},
 {"metadata":{"name":"openai"},"spec":{"inference":{"format":"openai","provider":"bedrock",
   "quota":{"intervals":[{"interval":"lifetime","input_tokens":"90000000","output_tokens":"30000000"}]}}}},
 {"metadata":{"name":"grafana"},"spec":{}}
]`

func TestAppLimitsFromAV19Listing(t *testing.T) {
	var apps []tshwrap.App
	if err := json.Unmarshal([]byte(appsListing), &apps); err != nil {
		t.Fatal(err)
	}
	got := appLimits(apps)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	a, o := got[0].Intervals[0], got[1].Intervals[0]
	if got[0].App != "anthropic" || a.Interval != "daily" || a.OutputTokens != 5e6 || a.InputTokens != 35e6 {
		t.Errorf("anthropic: %+v", got[0])
	}
	// protojson's string-encoded integers must parse too.
	if got[1].App != "openai" || o.Interval != "lifetime" || o.OutputTokens != 30e6 || o.InputTokens != 90e6 {
		t.Errorf("openai: %+v", got[1])
	}
	if len(apps[0].Spec.Inference.Models) != 1 {
		t.Error("the model list was lost")
	}
}

func TestAppLimitsFromAnOlderTsh(t *testing.T) {
	var apps []tshwrap.App
	_ = json.Unmarshal([]byte(`[{"metadata":{"name":"anthropic"},"spec":{"inference":{"format":"anthropic"}}}]`), &apps)
	if got := appLimits(apps); got != nil {
		t.Errorf("no quota field must mean no limits: %+v", got)
	}
}

// The beam-user case: the limits are readable, the usage refused.
func TestReadQuotaKeepsTheLimitsWhenUsageIsRefused(t *testing.T) {
	var apps []tshwrap.App
	_ = json.Unmarshal([]byte(appsListing), &apps)
	cfg := &config.Config{Proxy: "fresh-beans.beams.sh:443"}
	f := &fakeQuotaDeps{st: liveLogin(), apps: apps,
		tctlErr: errors.New("access denied to perform action \"create\" on \"app_server\"")}
	r, err := readQuota(context.Background(), cfg, f.deps())
	if err != nil || r.Problem != quotaNotAllowed || r.Endpoints != nil || len(r.Limits) != 2 {
		t.Fatalf("got %+v %v", r, err)
	}
	// And with no login at all: limits still, and tctl never runs.
	f = &fakeQuotaDeps{st: &tshwrap.Status{}, apps: apps}
	r, _ = readQuota(context.Background(), cfg, f.deps())
	if len(r.Limits) != 2 || f.tctlRan {
		t.Errorf("no login: %+v, tctl ran %v", r, f.tctlRan)
	}
	// A failed listing costs only the limits.
	f = &fakeQuotaDeps{st: liveLogin(), appsErr: errors.New("tsh too slow"), out: quotaFixture}
	r, err = readQuota(context.Background(), cfg, f.deps())
	if err != nil || r.Limits != nil || len(r.Endpoints) != 1 {
		t.Errorf("failed listing: %+v %v", r, err)
	}
}

func TestQuotaProxyInsideABeam(t *testing.T) {
	t.Setenv("TELEPORT_PROXY", "fresh-beans.beams.sh:443")
	t.Setenv("BEAM_ID", "")
	if got := quotaProxy(&config.Config{}); got != "" {
		t.Errorf("outside a beam, TELEPORT_PROXY must not count: %q", got)
	}
	t.Setenv("BEAM_ID", "157b24b2-a00f-4923-a93a-d1a9adf5673d")
	if got := quotaProxy(&config.Config{}); got != "fresh-beans.beams.sh:443" {
		t.Errorf("in a beam: %q", got)
	}
	if got := quotaProxy(&config.Config{Proxy: "odd-firefly.beams.sh:443"}); got != "odd-firefly.beams.sh:443" {
		t.Errorf("the configured proxy always wins: %q", got)
	}
}

func TestStatuslineAccountLabel(t *testing.T) {
	const proxy = "fresh-beans.beams.sh:443"
	const expired = "tsh login expired"
	for _, c := range []struct {
		mode, beam, note, want string
	}{
		{config.StatuslineBeamOn, "beam: quiet-otter", "", "beam: quiet-otter"},
		{config.StatuslineBeamOn, "beam: quiet-otter", expired, "beam: quiet-otter (tsh login expired)"},
		// A daemon that can't say which beam: the tenant.
		{config.StatuslineBeamOn, "", "", "fresh-beans"},
		{config.StatuslineBeamCluster, "", expired, "fresh-beans (tsh login expired)"},
		// Display off: nothing, until the login lapses.
		{config.StatuslineBeamOff, "", "", ""},
		{config.StatuslineBeamOff, "", expired, expired},
	} {
		if got := statuslineAccountLabel(c.mode, c.beam, proxy, c.note); got != c.want {
			t.Errorf("%+v: got %q", c, got)
		}
	}
}
