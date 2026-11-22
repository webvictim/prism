package quota

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// tctlJSON is `tctl get llm_endpoint_quota --format=json` as fresh-beans
// (daily) and odd-firefly (lifetime) printed it on 2026-11-22, with
// protojson's string-encoded counters.
const tctlJSON = `[
  {"kind":"llm_endpoint_quota","metadata":{"name":"anthropic"},"version":"v1",
   "status":{"active_windows":[{"interval_name":"daily",
     "starts_at":"2026-11-22T00:00:00Z","ends_at":"2026-11-23T00:00:00Z",
     "input_tokens":"6752","output_tokens":"56067"}]}},
  {"kind":"llm_endpoint_quota","metadata":{"name":"openai"},"version":"v1",
   "status":{"active_windows":[{"interval_name":"lifetime",
     "input_tokens":"12645466","output_tokens":599431}]}}
]`

func TestParseTctl(t *testing.T) {
	eps, err := ParseTctl([]byte(tctlJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 2 {
		t.Fatalf("got %d endpoints, want 2", len(eps))
	}
	a := eps[0].Windows[0]
	if eps[0].Name != "anthropic" || a.Interval != IntervalDaily || a.InputTokens != 6752 || a.OutputTokens != 56067 {
		t.Errorf("anthropic = %+v", a)
	}
	if a.EndsAt == nil || !a.EndsAt.Equal(time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("ends_at = %v", a.EndsAt)
	}
	o := eps[1].Windows[0]
	if o.Interval != IntervalLifetime || o.StartsAt != nil || o.EndsAt != nil || o.OutputTokens != 599431 {
		t.Errorf("openai = %+v (a bare number must parse too)", o)
	}
}

func TestParseTctlEmpty(t *testing.T) {
	eps, err := ParseTctl([]byte(`[]`))
	if err != nil || len(eps) != 0 {
		t.Fatalf("got %v, %v", eps, err)
	}
	if _, err := ParseTctl([]byte(`ERROR: access denied`)); err == nil {
		t.Fatal("non-JSON parsed")
	}
}

func TestResolve(t *testing.T) {
	daily := Window{Interval: IntervalDaily}
	life := Window{Interval: IntervalLifetime}
	weekly := Window{Interval: "weekly"}
	over := map[string]uint64{"openai": 1_000_000}
	for _, tc := range []struct {
		name   string
		app    string
		w      Window
		want   uint64
		source string
	}{
		{"daily default", "anthropic", daily, DefaultDailyOutput, SourceDefault},
		{"lifetime default", "anthropic", life, DefaultLifetimeOutput, SourceDefault},
		{"no default for weekly", "anthropic", weekly, 0, ""},
		{"config wins over default", "openai", daily, 1_000_000, SourceConfig},
		{"config covers any interval", "openai", weekly, 1_000_000, SourceConfig},
	} {
		got := Resolve(tc.app, tc.w, over)
		if got.OutputTokens != tc.want || got.Source != tc.source {
			t.Errorf("%s: got %+v, want %d from %q", tc.name, got, tc.want, tc.source)
		}
	}
}

func TestUsedPercent(t *testing.T) {
	w := Window{OutputTokens: 29_989_276}
	pct, ok := UsedPercent(w, Limit{OutputTokens: DefaultLifetimeOutput})
	if !ok || pct < 99.9 || pct > 100 {
		t.Errorf("odd-firefly's lifetime window: %v %v", pct, ok)
	}
	if _, ok := UsedPercent(w, Limit{}); ok {
		t.Error("an unknown limit produced a percentage")
	}
	if pct, _ := UsedPercent(Window{OutputTokens: 6e6}, Limit{OutputTokens: 5e6}); pct != 120 {
		t.Errorf("an overrun is reported as is, got %v", pct)
	}
}

func TestParseLimits(t *testing.T) {
	got, err := ParseLimits(" anthropic=5M, openai=1.5m ,x=500k,y=2000")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint64{"anthropic": 5e6, "openai": 1.5e6, "x": 5e5, "y": 2000}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	for _, bad := range []string{"", "anthropic", "anthropic=", "anthropic=-1", "anthropic=lots", "=5M"} {
		if _, err := ParseLimits(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestCacheRoundTripAndProxyScope(t *testing.T) {
	dir := t.TempDir()
	eps, _ := ParseTctl([]byte(tctlJSON))
	at := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	if err := Save(dir, &Cache{Proxy: "fresh-beans.beams.sh:443", FetchedAt: at, Endpoints: eps}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, FileName)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache file: %v %v", fi, err)
	}
	c, err := Load(dir, "fresh-beans.beams.sh:443")
	if err != nil || c == nil || !c.FetchedAt.Equal(at) {
		t.Fatalf("load: %+v %v", c, err)
	}
	if e, ok := c.Endpoint("anthropic"); !ok || e.Windows[0].OutputTokens != 56067 {
		t.Errorf("anthropic: %+v", e)
	}
	// Another cluster's numbers must never be shown under this one's name.
	if c, err := Load(dir, "odd-firefly.beams.sh:443"); c != nil || err != nil {
		t.Errorf("another proxy read %+v, %v", c, err)
	}
	if c, err := Load(dir, ""); c != nil || err != nil {
		t.Errorf("no proxy read %+v, %v", c, err)
	}
	if c, err := Load(t.TempDir(), "x"); c != nil || err != nil {
		t.Errorf("missing file: %+v, %v", c, err)
	}
}

func TestTenantLabel(t *testing.T) {
	for in, want := range map[string]string{
		"fresh-beans.beams.sh:443": "fresh-beans",
		"fresh-beans.beams.sh":     "fresh-beans",
		"localhost:3080":           "localhost",
	} {
		if got := TenantLabel(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

func TestStatusline(t *testing.T) {
	eps, _ := ParseTctl([]byte(tctlJSON))
	fetched := time.Date(2026, 11, 22, 20, 0, 0, 0, time.UTC)
	c := &Cache{Proxy: "fresh-beans.beams.sh:443", FetchedAt: fetched, Endpoints: eps}
	now := fetched.Add(time.Minute)
	out, err := Statusline([]byte(`{"version":1,"key":"work","session_id":"s"}`), c, c.Proxy, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version    int       `json:"version"`
		Key        string    `json:"key"`
		ObservedAt time.Time `json:"observed_at"`
		Account    struct{ ID, Label string }
		Limits     []struct {
			ID            string     `json:"id"`
			Label         string     `json:"label"`
			UsedPercent   *float64   `json:"used_percent"`
			WindowSeconds *uint64    `json:"window_seconds"`
			ResetsAt      *time.Time `json:"resets_at"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	if got.Version != 1 || got.Key != "work" || !got.ObservedAt.Equal(fetched) {
		t.Errorf("header: %s", out)
	}
	if got.Account.ID != "teleport:fresh-beans.beams.sh:443" || got.Account.Label != "fresh-beans" {
		t.Errorf("account: %+v", got.Account)
	}
	if len(got.Limits) != 2 {
		t.Fatalf("limits: %s", out)
	}
	a, o := got.Limits[0], got.Limits[1]
	if a.ID != "beams_anthropic" || a.Label != "Claude" || a.UsedPercent == nil ||
		*a.WindowSeconds != 86400 || !a.ResetsAt.Equal(time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("anthropic limit: %s", out)
	}
	if o.ID != "beams_openai" || o.WindowSeconds != nil || o.ResetsAt != nil || o.UsedPercent == nil {
		t.Errorf("a lifetime window has no reset but does have a percentage: %s", out)
	}

	// After the daily window resets, its stale count must not be shown.
	out, _ = Statusline([]byte(`{"version":1,"key":"k"}`), c, c.Proxy, nil, time.Date(2026, 11, 23, 0, 1, 0, 0, time.UTC))
	if json.Unmarshal(out, &got); len(got.Limits) != 1 || got.Limits[0].ID != "beams_openai" {
		t.Errorf("after reset: %s", out)
	}
}

func TestStatuslineWithNothingCached(t *testing.T) {
	out, err := Statusline([]byte(`{"version":1,"key":"k"}`), nil, "fresh-beans.beams.sh:443", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if v, ok := got["limits"]; !ok || v != nil {
		t.Errorf("want limits: null, got %s", out)
	}
	if got["key"] != "k" || got["account"] == nil {
		t.Errorf("required fields missing: %s", out)
	}
	if _, err := Statusline([]byte(`{"version":2,"key":"k"}`), nil, "", nil, time.Now()); err == nil {
		t.Error("version 2 accepted")
	}
}

func TestNudgeNeverBlocks(t *testing.T) {
	for range 3 {
		Nudge()
	}
	select {
	case <-Nudged():
	default:
		t.Fatal("no nudge delivered")
	}
	select {
	case <-Nudged():
		t.Fatal("a burst delivered more than one nudge")
	default:
	}
}
