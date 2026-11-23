package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/quota"
	"github.com/webvictim/prism/internal/router"
	usagepkg "github.com/webvictim/prism/internal/usage"
)

// quotaRow is one app's current window, ready to display.
type quotaRow struct {
	App    string
	Window quota.Window
	Limit  quota.Limit
	Pct    float64
	HasPct bool
	// Estimated rows count only this machine's requests, because the
	// tenant's usage couldn't be read (internal/quota/estimate.go). They
	// are a lower bound and always displayed marked ≈.
	Estimated bool
	// ReadAt, on an estimate, is when the tenant's count it starts from was
	// read: the row is that reading plus this machine's requests since
	// (staleReadingRows). Zero for an estimate from this machine alone.
	ReadAt time.Time
}

// quotaReadingStale is how old a reading gets before it is brought up to
// date with this machine's requests. It matches the max_age prism gives
// statusline (prismUsageSource), past which statusline marks it (stale).
const quotaReadingStale = 35 * time.Minute

// quotaApps are the LLM apps a quota is shown for, in display order.
var quotaAppNames = []string{router.AnthropicAppName, router.OpenAIAppName}

// quotaRows are the windows to show, one per app: the tenant's, as last
// read, or else an estimate from this machine's own requests.
func quotaRows(cfg *config.Config, c *quota.Cache, now time.Time) []quotaRow {
	if rows := measuredRows(cfg, c, now); len(rows) > 0 {
		if now.Sub(c.FetchedAt) > quotaReadingStale {
			return staleReadingRows(cfg, c, rows)
		}
		return rows
	}
	return estimatedRows(cfg, c, now)
}

// staleReadingRows bring a reading too old to stand on its own (the login
// lapsed, or the daemon isn't running) up to date: the tenant's count as
// read, plus what this machine has sent the proxy since. Still a lower
// bound, since other users and beams kept counting, so the rows are
// estimates, but much closer than this machine's requests alone.
func staleReadingRows(cfg *config.Config, c *quota.Cache, rows []quotaRow) []quotaRow {
	proxy := c.Proxy
	if cfg != nil && cfg.Proxy != "" {
		proxy = cfg.Proxy
	}
	overrides := quotaOverrides(cfg)
	for i, r := range rows {
		since := r.Window
		since.StartsAt = &c.FetchedAt
		in, out, _ := localTokens(proxy, r.App, since)
		r.Window.InputTokens += uint64(in)
		r.Window.OutputTokens += uint64(out)
		r.Limit = quota.Resolve(r.App, r.Window, overrides)
		r.Pct, r.HasPct = quota.UsedPercent(r.Window, r.Limit)
		r.Estimated, r.ReadAt = true, c.FetchedAt
		rows[i] = r
	}
	return rows
}

// rowsReadAt is when an estimate's starting reading was taken, or zero.
func rowsReadAt(rows []quotaRow) time.Time {
	if len(rows) == 0 {
		return time.Time{}
	}
	return rows[0].ReadAt
}

// measuredRows are the cache's live windows. A window that has reset
// since it was read is left out: its count describes nothing now.
func measuredRows(cfg *config.Config, c *quota.Cache, now time.Time) []quotaRow {
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

// estimatedRows stand in for the tenant's usage when it can't be read:
// this machine's own requests to the configured proxy, over the window the
// tenant's quota would count. Only when there is reason to think the
// tenant has a quota (quota.EstimateWindow), and only for an app this
// machine actually used in that window — where prism carried no traffic,
// a confident 0% would be worse than nothing.
func estimatedRows(cfg *config.Config, c *quota.Cache, now time.Time) []quotaRow {
	if cfg == nil || cfg.Proxy == "" {
		return nil
	}
	// A read that worked and found no windows means the cluster has no
	// quota; there is nothing to estimate.
	if c != nil && c.Problem == "" && !c.FetchedAt.IsZero() {
		return nil
	}
	overrides := quotaOverrides(cfg)
	var rows []quotaRow
	for _, app := range quotaAppNames {
		w, ok := quota.EstimateWindow(c, app, len(overrides) > 0, now)
		if !ok {
			continue
		}
		in, out, n := localTokens(cfg.Proxy, app, w)
		if n == 0 {
			continue
		}
		w.InputTokens, w.OutputTokens = uint64(in), uint64(out)
		row := quotaRow{App: app, Window: w, Limit: quota.Resolve(app, w, overrides), Estimated: true}
		row.Pct, row.HasPct = quota.UsedPercent(w, row.Limit)
		rows = append(rows, row)
	}
	return rows
}

// localTokens sums this machine's recorded tokens for app on proxy within
// w (all of them, for a lifetime window), and how many requests that was.
func localTokens(proxy, app string, w quota.Window) (in, out int64, n int) {
	dir, err := usagepkg.Dir()
	if err != nil {
		return 0, 0, 0
	}
	var since time.Time
	if w.StartsAt != nil {
		since = *w.StartsAt
	}
	records, err := usagepkg.Load(dir, since)
	if err != nil {
		return 0, 0, 0
	}
	for _, rec := range records {
		if rec.Backend != app || rec.Proxy != proxy || rec.Timestamp.Before(since) {
			continue
		}
		if w.EndsAt != nil && !rec.Timestamp.Before(*w.EndsAt) {
			continue
		}
		in, out, n = in+rec.InputTokens, out+rec.OutputTokens, n+1
	}
	return in, out, n
}

// rowsEstimated reports whether rows are an estimate (they all are or
// none is).
func rowsEstimated(rows []quotaRow) bool { return len(rows) > 0 && rows[0].Estimated }

// approx is the mark an estimated figure carries.
func approx(rows []quotaRow) string {
	if rowsEstimated(rows) {
		return "≈"
	}
	return ""
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
	rows := quotaRows(cfg, c, now)
	if len(rows) == 0 {
		if c != nil && c.Problem != "" {
			fmt.Fprintf(w, "\nTenant quota: not shown: %s\n", c.Problem)
		}
		return
	}
	estimated := rowsEstimated(rows)
	readAt := rowsReadAt(rows)
	switch {
	case !readAt.IsZero():
		fmt.Fprintf(w, "\nTenant quota, output tokens (%s, ≈ read %s, plus this machine since)\n", quota.TenantLabel(cfg.Proxy), formatAge(now.Sub(readAt)))
	case estimated:
		fmt.Fprintf(w, "\nTenant quota, output tokens (%s, ≈ estimated from this machine)\n", quota.TenantLabel(cfg.Proxy))
	default:
		fmt.Fprintf(w, "\nTenant quota, output tokens (%s, read %s)\n", quota.TenantLabel(c.Proxy), formatAge(now.Sub(c.FetchedAt)))
	}
	fmt.Fprintln(w, repeat("─", 60))
	defaulted := false
	for _, r := range rows {
		used := approx(rows) + usagepkg.FormatTokens(int64(r.Window.OutputTokens))
		limit := "?"
		pct := ""
		if r.HasPct {
			limit = usagepkg.FormatTokens(int64(r.Limit.OutputTokens))
			pct = "  " + approx(rows) + formatPct(r.Pct)
			if r.Limit.Source == quota.SourceDefault {
				limit += "*"
				defaulted = true
			}
			// The window is full when either kind runs out — for OpenAI,
			// whose input count includes cached tokens, usually input — so
			// say so when it's the input that is closer.
			if r.Limit.InputTokens > 0 && (r.Limit.OutputTokens == 0 ||
				r.Window.InputTokens*r.Limit.OutputTokens > r.Window.OutputTokens*r.Limit.InputTokens) {
				pct += fmt.Sprintf(" (input %s%s / %s)", approx(rows), usagepkg.FormatTokens(int64(r.Window.InputTokens)), usagepkg.FormatTokens(int64(r.Limit.InputTokens)))
			}
		}
		fmt.Fprintf(w, "%-10s %-30s %8s / %-6s%s\n", r.App, describeWindow(r.Window, now), used, limit, pct)
	}
	// The tenant's count is everyone's: every user, every beam, and
	// anything that reached the apps without prism. This machine's share,
	// over the same window, is what tells those apart.
	switch {
	case !readAt.IsZero():
		fmt.Fprintf(w, "≈ the tenant's count as read %s, plus this machine's requests since: other users and beams count too, so the tenant's figure may be higher.\n", formatAge(now.Sub(readAt)))
	case estimated:
		fmt.Fprintln(w, "≈ this machine's requests only: other users and beams count too, so the tenant's figure is usually higher.")
	default:
		if mine := localOutputSince(cfg, rows); mine != "" {
			fmt.Fprintf(w, "this machine, same windows: %s\n", mine)
		}
	}
	if defaulted {
		fmt.Fprintln(w, "* assumed (5M output / 35M input daily, 30M / 90M lifetime): the cluster doesn't say; `prism config set quota_limits anthropic=5M,openai=5M` sets the output limit")
	}
	if c != nil && c.Problem != "" {
		label := "last check"
		if estimated && readAt.IsZero() {
			label = "the tenant's usage isn't read here because"
		}
		fmt.Fprintf(w, "%s: %s\n", label, c.Problem)
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
		v := approx(rows) + usagepkg.FormatTokens(int64(r.Window.OutputTokens)) + " out"
		if r.HasPct {
			v = approx(rows) + formatPct(r.Pct)
		}
		parts = append(parts, fmt.Sprintf("%s %s (%s)", r.App, v, r.Window.Interval))
	}
	if readAt := rowsReadAt(rows); !readAt.IsZero() {
		return fmt.Sprintf("%s · estimated: read %s, plus this machine's requests since", strings.Join(parts, " · "), formatAge(now.Sub(readAt)))
	}
	if rowsEstimated(rows) {
		return strings.Join(parts, " · ") + " · estimated from this machine's requests"
	}
	return fmt.Sprintf("%s · as of %s", strings.Join(parts, " · "), formatAge(now.Sub(c.FetchedAt)))
}

// quotaJSON is `prism status --json`'s `quota` key: null when unknown.
type quotaJSON struct {
	Proxy     string         `json:"proxy"`
	FetchedAt *time.Time     `json:"fetched_at"` // null for an estimate from this machine alone
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
	// Approximate: the counts are this machine's own, because the
	// tenant's couldn't be read — a lower bound.
	Approximate bool `json:"approximate"`
}

func buildQuotaJSON(cfg *config.Config, now time.Time) *quotaJSON {
	c := loadQuota(cfg)
	rows := quotaRows(cfg, c, now)
	if len(rows) == 0 {
		return nil
	}
	q := &quotaJSON{Proxy: cfg.Proxy}
	// When the reading was taken, also for an estimate built on one.
	if c != nil && (!rowsEstimated(rows) || !rowsReadAt(rows).IsZero()) {
		fetched := c.FetchedAt
		q.FetchedAt = &fetched
	}
	for _, r := range rows {
		a := quotaJSONApp{
			App: r.App, Interval: r.Window.Interval,
			OutputTokens: r.Window.OutputTokens, InputTokens: r.Window.InputTokens,
			ResetsAt: r.Window.EndsAt, Approximate: r.Estimated,
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
	req, err := readStatuslineRequest(in, statuslineStdinWait)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	now := time.Now()
	c := loadQuota(cfg)
	rows := quotaRows(cfg, c, now)
	// An estimate from this machine alone is measured now; anything built
	// on a reading keeps the reading's time, so statusline says (stale)
	// once it is old: the cue to log in again.
	var observed time.Time
	switch {
	case rowsEstimated(rows) && rowsReadAt(rows).IsZero():
		observed = now
	case c != nil:
		observed = c.FetchedAt
	}
	note := statuslineLoginNote(c)
	var entries []quota.StatuslineEntry
	for _, r := range rows {
		entries = append(entries, quota.StatuslineEntry{App: r.App, Window: r.Window, Limit: r.Limit, Approximate: r.Estimated, Note: note})
	}
	resp, err := quota.Statusline(req, cfg.Proxy, observed, entries, now)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prism:", err)
		return err
	}
	_, err = out.Write(append(resp, '\n'))
	return err
}

// statuslineLoginNote is the hint statusline's labels carry when the
// daemon can't read the quota for want of a login, the one cause the user
// fixes with a command: "tsh login expired" after an earlier reading,
// else "no tsh login". The reading's own time is kept, so statusline also
// marks it (stale) once it passes max_age; together they say what to do.
func statuslineLoginNote(c *quota.Cache) string {
	if c == nil || !strings.HasPrefix(c.Problem, quotaNoLoginPrefix) {
		return ""
	}
	if c.FetchedAt.IsZero() {
		return "no tsh login"
	}
	return "tsh login expired"
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
	if rowsEstimated(rows) {
		line += " (a lower bound)"
	}
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

// statuslineStdinWait bounds the wait for statusline's request. It writes
// one small JSON object and closes stdin at once; a caller that holds
// stdin open without writing must not cost it the answer. Well inside
// statusline's own subprocess timeout.
const statuslineStdinWait = time.Second

// readStatuslineRequest reads the request on in, giving up after wait
// with whatever has arrived. The request only supplies the key echoed
// back, so answering without it is better than not answering.
func readStatuslineRequest(in io.Reader, wait time.Duration) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(in, 1<<20))
		done <- result{b, err}
	}()
	select {
	case r := <-done:
		if r.err == nil && len(bytes.TrimSpace(r.b)) == 0 {
			return []byte(statuslineEmptyRequest), nil
		}
		return r.b, r.err
	case <-time.After(wait):
		return []byte(statuslineEmptyRequest), nil
	}
}

// statuslineEmptyRequest stands in for a request that never came.
const statuslineEmptyRequest = `{"version":1}`
