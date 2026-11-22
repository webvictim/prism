package quota

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Statusline speaks version 1 of statusline's external usage protocol
// (github.com/ryanclark/statusline, docs/external-usage.md): it is handed
// {"version":1,"key":…} and answers with the account's limits. statusline
// runs the command in the background and caches the answer, so this only
// ever reads prism's own cache — never the cluster.

// StatuslineLimitID names the limit statusline's `usage_limit` segment
// selects (`limit_id`) for an app.
func StatuslineLimitID(app string) string { return "beams_" + app }

type statuslineRequest struct {
	Version int    `json:"version"`
	Key     string `json:"key"`
}

type statuslineAccount struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type statuslineLimit struct {
	ID            string     `json:"id"`
	Label         string     `json:"label,omitempty"`
	UsedPercent   *float64   `json:"used_percent,omitempty"`
	WindowSeconds *uint64    `json:"window_seconds,omitempty"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
}

type statuslineResponse struct {
	Version    int               `json:"version"`
	Key        string            `json:"key"`
	ObservedAt time.Time         `json:"observed_at"`
	Account    statuslineAccount `json:"account"`
	// Limits is null, not [], when there is nothing to show: the protocol
	// reads both as "no limits", and null says it was never measured.
	Limits []statuslineLimit `json:"limits"`
}

// statuslineLabels are the apps' display names, for the segment's label.
var statuslineLabels = map[string]string{"anthropic": "Claude", "openai": "OpenAI"}

// TenantLabel is a short name for the cluster: its first DNS label, so
// fresh-beans.beams.sh:443 is "fresh-beans".
func TenantLabel(proxy string) string {
	host := proxy
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i]
	}
	return host
}

// Statusline answers a statusline request from the cache. c may be nil
// (nothing cached, or another cluster's), which is an answer with no
// limits rather than an error, so statusline hides the segments instead of
// reporting a failure. now is when the answer is made: windows that have
// reset since the read are left out, and so is an observation from the
// future, which the protocol refuses outright.
func Statusline(req []byte, c *Cache, proxy string, overrides map[string]uint64, now time.Time) ([]byte, error) {
	var r statuslineRequest
	if err := json.Unmarshal(req, &r); err != nil {
		return nil, fmt.Errorf("reading statusline's request: %w", err)
	}
	if r.Version != 1 {
		return nil, fmt.Errorf("statusline asked for protocol version %d; prism speaks 1", r.Version)
	}
	if proxy == "" {
		proxy = "unconfigured"
	}
	resp := statuslineResponse{
		Version:    1,
		Key:        r.Key,
		ObservedAt: now.UTC().Truncate(time.Second),
		Account:    statuslineAccount{ID: "teleport:" + proxy, Label: TenantLabel(proxy)},
	}
	if c != nil && !c.FetchedAt.IsZero() && !c.FetchedAt.After(now) {
		resp.ObservedAt = c.FetchedAt.UTC()
		for _, e := range c.Endpoints {
			for _, w := range e.Windows {
				if w.Ended(now) {
					continue
				}
				lim := statuslineLimit{ID: StatuslineLimitID(e.Name), Label: statuslineLabels[e.Name]}
				if pct, ok := UsedPercent(w, Resolve(e.Name, w, overrides)); ok {
					lim.UsedPercent = &pct
				}
				if w.StartsAt != nil && w.EndsAt != nil && w.EndsAt.After(*w.StartsAt) {
					secs := uint64(w.EndsAt.Sub(*w.StartsAt) / time.Second)
					lim.WindowSeconds = &secs
					ends := w.EndsAt.UTC()
					lim.ResetsAt = &ends
				}
				resp.Limits = append(resp.Limits, lim)
				// One window per app: the protocol wants unique ids, and
				// tenants run a single interval.
				break
			}
		}
	}
	return json.Marshal(resp)
}
