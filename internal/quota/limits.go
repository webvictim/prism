package quota

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Where the limits come from, in order:
//
//  1. `quota_limits` in prism's config, per app: the user's override of
//     the output limit;
//  2. the cluster: the app's own `inference.quota`, read from the apps
//     listing prism already keeps for the model list. That is a Teleport
//     v19 field, so it arrives with a v19 tsh — the one every beam ships,
//     and the one the cluster's managed client tools will move to — and an
//     18.x tsh drops it. Reading it needs only access to the apps, which
//     prism's tbot identity and a `beam-user`-only beam both have;
//  3. prism's defaults, below.
//
// The defaults are what Beams tenants run with (November 2026, confirmed
// live against the cluster's own config):
//
//   - daily: 5M output and 35M input tokens a day, resetting at the
//     window's ends_at — the server supplies that, so the tenant's own
//     reset time is used;
//   - lifetime: 30M output and 90M input tokens, never resetting.
//
// Both kinds count, because the gateway refuses once either runs out, and
// which runs out first depends on the endpoint's accounting. Anthropic's
// input count is uncached input alone, so for Claude Code it is tiny and
// output binds. OpenAI's counts cached input too, and an agent resends its
// whole context every turn: odd-firefly's openai lifetime window stood at
// 12.6M input to 0.6M output, 14% of its input limit against 2% of its
// output one.
const (
	DefaultDailyOutput    = 5_000_000
	DefaultDailyInput     = 35_000_000
	DefaultLifetimeOutput = 30_000_000
	DefaultLifetimeInput  = 90_000_000
)

// Where a limit came from, for saying so.
const (
	SourceDefault = "default"
	SourceConfig  = "config"
	SourceCluster = "cluster"
)

// AppLimits are one app's limits as the cluster configures them.
type AppLimits struct {
	App       string          `json:"app"`
	Intervals []LimitInterval `json:"intervals"`
}

// LimitInterval is one interval's limits; zero is no limit on that kind.
type LimitInterval struct {
	Interval     string `json:"interval"`
	InputTokens  uint64 `json:"input_tokens,omitempty"`
	OutputTokens uint64 `json:"output_tokens,omitempty"`
}

// Limit is what a window is measured against. Either count may be zero,
// meaning that kind of token isn't limited (or the limit isn't known).
type Limit struct {
	OutputTokens uint64
	InputTokens  uint64
	Source       string
}

// Known reports whether there is a limit at all: an interval prism has no
// default for, with nothing configured or read, has none.
func (l Limit) Known() bool { return l.OutputTokens > 0 || l.InputTokens > 0 }

// ClusterLimit is the cluster's limit for app's interval, if it said.
func ClusterLimit(cluster []AppLimits, app, interval string) (LimitInterval, bool) {
	for _, a := range cluster {
		if a.App != app {
			continue
		}
		for _, iv := range a.Intervals {
			if iv.Interval == interval && (iv.InputTokens > 0 || iv.OutputTokens > 0) {
				return iv, true
			}
		}
	}
	return LimitInterval{}, false
}

// Resolve picks the limit for app's window: the configured override (of
// the output limit, which is what it has always meant), else what the
// cluster says, else the default for the window's interval.
func Resolve(app string, w Window, overrides map[string]uint64, cluster []AppLimits) Limit {
	if n, ok := overrides[app]; ok && n > 0 {
		return Limit{OutputTokens: n, Source: SourceConfig}
	}
	if iv, ok := ClusterLimit(cluster, app, w.Interval); ok {
		return Limit{OutputTokens: iv.OutputTokens, InputTokens: iv.InputTokens, Source: SourceCluster}
	}
	switch w.Interval {
	case IntervalDaily:
		return Limit{OutputTokens: DefaultDailyOutput, InputTokens: DefaultDailyInput, Source: SourceDefault}
	case IntervalLifetime:
		return Limit{OutputTokens: DefaultLifetimeOutput, InputTokens: DefaultLifetimeInput, Source: SourceDefault}
	}
	return Limit{}
}

// UsedPercent is how full the window is: the larger of its output and
// input use against their limits, since the gateway refuses once either
// runs out. Unclamped, as a window can overrun by the requests in flight
// when it filled. ok is false when there is no limit to measure against.
func UsedPercent(w Window, l Limit) (pct float64, ok bool) {
	if l.OutputTokens > 0 {
		pct, ok = float64(w.OutputTokens)/float64(l.OutputTokens)*100, true
	}
	if l.InputTokens > 0 {
		pct, ok = max(pct, float64(w.InputTokens)/float64(l.InputTokens)*100), true
	}
	return pct, ok
}

// ParseLimits reads the `quota_limits` config value:
// "anthropic=5M,openai=500k". Each entry is an app name and an output
// token count.
func ParseLimits(s string) (map[string]uint64, error) {
	out := map[string]uint64{}
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		app, value, ok := strings.Cut(entry, "=")
		app = strings.TrimSpace(app)
		if !ok || app == "" {
			return nil, fmt.Errorf("%q is not app=tokens (e.g. anthropic=5M)", entry)
		}
		n, err := ParseTokens(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", app, err)
		}
		out[app] = n
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no limits given (e.g. anthropic=5M,openai=5M)")
	}
	return out, nil
}

// ParseTokens reads a token count with an optional k/M/G suffix: 5M,
// 1.5M, 500k, 5000000.
func ParseTokens(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	mult := 1.0
	if s != "" {
		switch s[len(s)-1] {
		case 'k', 'K':
			mult, s = 1e3, s[:len(s)-1]
		case 'm', 'M':
			mult, s = 1e6, s[:len(s)-1]
		case 'g', 'G', 'b', 'B':
			mult, s = 1e9, s[:len(s)-1]
		}
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, "_", ""), 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%q is not a positive token count (e.g. 5M)", s)
	}
	return uint64(math.Round(f * mult)), nil
}
