package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/quota"
	usagepkg "github.com/webvictim/prism/internal/usage"
)

// quotaRow is one app's current window, ready to display.
type quotaRow struct {
	App    string
	Window quota.Window
	Limit  quota.Limit
	Pct    float64
	HasPct bool
}

// quotaRows are the cache's live windows, one per app. A window that has
// reset since it was read is left out: its count describes nothing now.
func quotaRows(cfg *config.Config, c *quota.Cache, now time.Time) []quotaRow {
	if c == nil {
		return nil
	}
	overrides := quotaOverrides(cfg)
	var rows []quotaRow
	for _, e := range c.Endpoints {
		for _, w := range e.Windows {
			if w.Ended(now) {
				continue
			}
			row := quotaRow{App: e.Name, Window: w, Limit: quota.Resolve(e.Name, w, overrides)}
			row.Pct, row.HasPct = quota.UsedPercent(w, row.Limit)
			rows = append(rows, row)
			break
		}
	}
	return rows
}

// describeWindow is "daily, resets 20:00" or "lifetime, never resets".
func describeWindow(w quota.Window, now time.Time) string {
	if w.EndsAt == nil {
		return w.Interval + ", never resets"
	}
	at := w.EndsAt.Local()
	layout := "15:04"
	if at.Sub(now) > 20*time.Hour {
		layout = "Mon 15:04"
	}
	return w.Interval + ", resets " + at.Format(layout)
}

func formatPct(pct float64) string {
	if pct < 10 {
		return fmt.Sprintf("%.1f%%", pct)
	}
	return fmt.Sprintf("%.0f%%", pct)
}

func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// printQuotaSection is `prism usage`'s tenant section. It prints nothing
// at all when there is no proxy or nothing has ever been checked, and one
// line saying why when the quota couldn't be read: never an error.
func printQuotaSection(w io.Writer, cfg *config.Config, now time.Time) {
	c := loadQuota(cfg)
	if c == nil {
		return
	}
	rows := quotaRows(cfg, c, now)
	if len(rows) == 0 {
		if c.Problem != "" {
			fmt.Fprintf(w, "\nTenant quota: not shown: %s\n", c.Problem)
		}
		return
	}
	fmt.Fprintf(w, "\nTenant quota, output tokens (%s, read %s)\n", quota.TenantLabel(c.Proxy), formatAge(now.Sub(c.FetchedAt)))
	fmt.Fprintln(w, repeat("─", 60))
	defaulted := false
	for _, r := range rows {
		used := usagepkg.FormatTokens(int64(r.Window.OutputTokens))
		limit := "?"
		pct := ""
		if r.HasPct {
			limit = usagepkg.FormatTokens(int64(r.Limit.OutputTokens))
			pct = "  " + formatPct(r.Pct)
			if r.Limit.Source == quota.SourceDefault {
				limit += "*"
				defaulted = true
			}
		}
		fmt.Fprintf(w, "%-10s %-30s %8s / %-6s%s\n", r.App, describeWindow(r.Window, now), used, limit, pct)
	}
	// The tenant's count is everyone's: every user, every beam, and
	// anything that reached the apps without prism. This machine's share,
	// over the same window, is what tells those apart.
	if mine := localOutputSince(cfg, rows); mine != "" {
		fmt.Fprintf(w, "this machine, same windows: %s\n", mine)
	}
	if defaulted {
		fmt.Fprintln(w, "* assumed: the cluster doesn't say; `prism config set quota_limits anthropic=5M,openai=5M`")
	}
	if c.Problem != "" {
		fmt.Fprintf(w, "last check: %s\n", c.Problem)
	}
}

// localOutputSince sums this machine's output, per app, over each row's
// window (all of it, for a lifetime window), for the configured proxy.
func localOutputSince(cfg *config.Config, rows []quotaRow) string {
	dir, err := usagepkg.Dir()
	if err != nil {
		return ""
	}
	var parts []string
	for _, r := range rows {
		var since time.Time
		if r.Window.StartsAt != nil {
			since = *r.Window.StartsAt
		}
		records, err := usagepkg.Load(dir, since)
		if err != nil {
			return ""
		}
		var out int64
		for _, rec := range records {
			if rec.Backend == r.App && rec.Proxy == cfg.Proxy && !rec.Timestamp.Before(since) {
				out += rec.OutputTokens
			}
		}
		parts = append(parts, fmt.Sprintf("%s %s", r.App, usagepkg.FormatTokens(out)))
	}
	return strings.Join(parts, "  ")
}

// quotaStatusLine is `prism status`'s one line, or "" with nothing to say.
func quotaStatusLine(cfg *config.Config, now time.Time) string {
	c := loadQuota(cfg)
	rows := quotaRows(cfg, c, now)
	if len(rows) == 0 {
		return ""
	}
	var parts []string
	for _, r := range rows {
		v := usagepkg.FormatTokens(int64(r.Window.OutputTokens)) + " out"
		if r.HasPct {
			v = formatPct(r.Pct)
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", r.App, v, r.Window.Interval))
	}
	return fmt.Sprintf("%s · as of %s", strings.Join(parts, " · "), formatAge(now.Sub(c.FetchedAt)))
}

// quotaJSON is `prism status --json`'s `quota` key: null when unknown.
type quotaJSON struct {
	Proxy     string         `json:"proxy"`
	FetchedAt time.Time      `json:"fetched_at"`
	Apps      []quotaJSONApp `json:"apps"`
}

type quotaJSONApp struct {
	App          string     `json:"app"`
	Interval     string     `json:"interval"`
	OutputTokens uint64     `json:"output_tokens"`
	InputTokens  uint64     `json:"input_tokens"`
	OutputLimit  *uint64    `json:"output_limit"`
	LimitSource  string     `json:"limit_source,omitempty"`
	UsedPercent  *float64   `json:"used_percent"`
	ResetsAt     *time.Time `json:"resets_at"`
}

func buildQuotaJSON(cfg *config.Config, now time.Time) *quotaJSON {
	c := loadQuota(cfg)
	rows := quotaRows(cfg, c, now)
	if len(rows) == 0 {
		return nil
	}
	q := &quotaJSON{Proxy: c.Proxy, FetchedAt: c.FetchedAt}
	for _, r := range rows {
		a := quotaJSONApp{
			App: r.App, Interval: r.Window.Interval,
			OutputTokens: r.Window.OutputTokens, InputTokens: r.Window.InputTokens,
			ResetsAt: r.Window.EndsAt,
		}
		if r.HasPct {
			lim, pct := r.Limit.OutputTokens, r.Pct
			a.OutputLimit, a.UsedPercent, a.LimitSource = &lim, &pct, r.Limit.Source
		}
		q.Apps = append(q.Apps, a)
	}
	return q
}

// cmdUsageStatusline is `prism usage --statusline`: statusline's
// external usage command (internal/quota/statusline.go). It reads only
// the daemon's cache, so it answers in milliseconds and never touches the
// cluster or a login.
func cmdUsageStatusline(in io.Reader, out io.Writer) error {
	req, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	resp, err := quota.Statusline(req, loadQuota(cfg), cfg.Proxy, quotaOverrides(cfg), time.Now())
	if err != nil {
		fmt.Fprintln(os.Stderr, "prism:", err)
		return err
	}
	_, err = out.Write(append(resp, '\n'))
	return err
}

// checkQuota is doctor's view of the tenant's quota, from the daemon's
// cache: read-only like the rest of doctor. Nearly full is a warning; a
// full lifetime window is a failure, because nothing but an
// administrator will ever empty it and every request is refused meanwhile.
func checkQuota(r *report, cfg *config.Config, now time.Time) {
	c := loadQuota(cfg)
	rows := quotaRows(cfg, c, now)
	if len(rows) == 0 {
		if c != nil && c.Problem != "" {
			r.info("quota", "not shown: "+c.Problem)
		}
		return
	}
	worst := rows[0]
	for _, row := range rows[1:] {
		if row.Pct > worst.Pct {
			worst = row
		}
	}
	line := quotaStatusLine(cfg, now)
	switch {
	case !worst.HasPct || worst.Pct < 80:
		r.ok("quota", line)
	case worst.Pct >= 100 && worst.Window.EndsAt == nil:
		r.fail("quota", fmt.Sprintf("%s has used its whole lifetime quota: the gateway refuses every request (`tokens quota exceeded`). %s", worst.App, line),
			"ask the tenant's administrator to raise or reset it")
	case worst.Pct >= 100:
		r.warn("quota", fmt.Sprintf("%s's quota is used up until it %s. %s", worst.App, strings.TrimPrefix(describeWindow(worst.Window, now), worst.Window.Interval+", "), line),
			"wait for the reset, or ask the tenant's administrator")
	default:
		r.warn("quota", fmt.Sprintf("%s is at %s of its quota. %s", worst.App, formatPct(worst.Pct), line),
			"`prism usage` shows the details")
	}
}
