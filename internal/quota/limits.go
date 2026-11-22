package quota

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// The limits prism assumes, per app, because nothing a client holds can
// read the real ones (see the package comment). They are what Beams
// tenants were seen to run with in November 2026:
//
//   - daily: 5M output tokens a day, resetting at the window's ends_at —
//     the server supplies that, so the tenant's own reset time is used;
//   - lifetime: 30M output tokens, never resetting.
//
// Only output is limited by default. The Anthropic endpoint counts
// uncached input alone, so for Claude Code its input count is tiny, and
// an input limit would be a number nobody approaches. `quota_limits` in
// prism's config replaces these per app without a release.
const (
	DefaultDailyOutput    = 5_000_000
	DefaultLifetimeOutput = 30_000_000
)

// Where a limit came from, for saying so.
const (
	SourceDefault = "default"
	SourceConfig  = "config"
)

// Limit is the output-token limit a window is measured against.
type Limit struct {
	OutputTokens uint64
	Source       string
}

// Known reports whether there is a limit at all: an interval prism has no
// default for, with nothing configured, has none.
func (l Limit) Known() bool { return l.OutputTokens > 0 }

// Resolve picks the limit for app's window: the configured override for
// the app, else the default for the window's interval.
func Resolve(app string, w Window, overrides map[string]uint64) Limit {
	if n, ok := overrides[app]; ok && n > 0 {
		return Limit{OutputTokens: n, Source: SourceConfig}
	}
	switch w.Interval {
	case IntervalDaily:
		return Limit{OutputTokens: DefaultDailyOutput, Source: SourceDefault}
	case IntervalLifetime:
		return Limit{OutputTokens: DefaultLifetimeOutput, Source: SourceDefault}
	}
	return Limit{}
}

// UsedPercent is the window's output against the limit, unclamped (a
// window can overrun by the requests in flight when it filled). ok is
// false when there is no limit to measure against.
func UsedPercent(w Window, l Limit) (pct float64, ok bool) {
	if !l.Known() {
		return 0, false
	}
	return float64(w.OutputTokens) / float64(l.OutputTokens) * 100, true
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
