package quota

import "time"

// When the tenant's usage can't be read — no login, or roles without the
// editor role's app_server access — prism can still say roughly where the
// quota stands from the requests it carried itself. That is a lower
// bound: the tenant's counter also counts every other user, every beam,
// and anything that reached the apps without prism. So it is only ever
// shown marked as approximate.
//
// An estimate needs a window to count over, and prism only assumes one on
// evidence that the tenant has quotas at all. Otherwise a cluster with no
// quota would be shown measured against one.

// EstimateWindow is the window an estimate for app counts over, from the
// best evidence available:
//
//  1. an earlier reading of the tenant's usage (the cache keeps the last
//     good one): its interval, rolled forward to the window containing
//     now, which keeps the tenant's own reset time;
//  2. the cluster's own limits (Teleport v19, limits.go): a daily one
//     counts from 00:00 UTC — the reset Beams tenants use, as the listing
//     doesn't carry a custom one through — and a lifetime one from the
//     start of this machine's records;
//  3. failing both, if assumeDaily (the user configured `quota_limits`,
//     saying the tenant has quotas), a daily window from 00:00 UTC.
//
// ok is false when there is no evidence, or the earlier window's interval
// can't be rolled forward (months and years have no fixed length).
// Counts in the returned window are zero; the caller fills them.
func EstimateWindow(c *Cache, app string, assumeDaily bool, now time.Time) (Window, bool) {
	if w, ok := evidenceWindow(c, app); ok {
		return rollForward(w, now)
	}
	if c != nil {
		for _, interval := range clusterIntervals(c.Limits, app) {
			switch interval {
			case IntervalDaily:
				return dailyUTC(now), true
			case IntervalLifetime:
				return Window{Interval: IntervalLifetime}, true
			}
		}
	}
	if assumeDaily {
		return dailyUTC(now), true
	}
	return Window{}, false
}

// evidenceWindow is a window from the cache's last reading, the app's own
// if it has one, else another app's (tenants configure one interval for
// both).
func evidenceWindow(c *Cache, app string) (Window, bool) {
	if c == nil {
		return Window{}, false
	}
	if e, ok := c.Endpoint(app); ok && len(e.Windows) > 0 {
		return e.Windows[0], true
	}
	for _, e := range c.Endpoints {
		if len(e.Windows) > 0 {
			return e.Windows[0], true
		}
	}
	return Window{}, false
}

// rollForward moves a window of fixed length forward until it contains
// now. A lifetime window has no bounds and is returned as is.
func rollForward(w Window, now time.Time) (Window, bool) {
	out := Window{Interval: w.Interval}
	if w.StartsAt == nil && w.EndsAt == nil {
		return out, w.Interval == IntervalLifetime
	}
	if w.StartsAt == nil || w.EndsAt == nil {
		return Window{}, false
	}
	period := w.EndsAt.Sub(*w.StartsAt)
	// Only intervals of a fixed length roll forward by addition.
	if period != 24*time.Hour && period != 7*24*time.Hour {
		return Window{}, false
	}
	start, end := *w.StartsAt, *w.EndsAt
	if !now.Before(end) {
		n := now.Sub(end)/period + 1
		start, end = start.Add(n*period), end.Add(n*period)
	}
	if now.Before(start) {
		return Window{}, false
	}
	out.StartsAt, out.EndsAt = &start, &end
	return out, true
}

func dailyUTC(now time.Time) Window {
	u := now.UTC()
	start := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	return Window{Interval: IntervalDaily, StartsAt: &start, EndsAt: &end}
}

// clusterIntervals are the intervals the cluster limits app by, else
// those it limits any app by (tenants configure both alike).
func clusterIntervals(limits []AppLimits, app string) []string {
	var mine, any []string
	for _, a := range limits {
		for _, iv := range a.Intervals {
			if a.App == app {
				mine = append(mine, iv.Interval)
			}
			any = append(any, iv.Interval)
		}
	}
	if len(mine) > 0 {
		return mine
	}
	return any
}
