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
	Version   int    `json:"version"`
	Key       string `json:"key"`
	SessionID string `json:"session_id"`
}

// StatuslineSessionID is the conversation a statusline request is for:
// Claude Code's session id, the one it sends as X-Claude-Code-Session-Id.
// "" when the request carries none or can't be read.
func StatuslineSessionID(req []byte) string {
	var r statuslineRequest
	if json.Unmarshal(req, &r) != nil {
		return ""
	}
	return r.SessionID
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
	// Approximate is prism's own field, which statusline tolerates and
	// ignores today: the percentage is an estimate from this machine's
	// requests (estimate.go). The label carries the mark too ("Claude ≈"),
	// since that is what statusline displays.
	Approximate bool `json:"approximate,omitempty"`
}

// StatuslineEntry is one app's window as statusline should show it.
type StatuslineEntry struct {
	App         string
	Window      Window
	Limit       Limit
	Approximate bool
	// Note follows the label in brackets, ahead of any ≈: why the figure
	// isn't current, as a hint at the fix ("Claude (tsh login expired) ≈").
	Note string
}

// approximateMark follows the label of an estimated limit.
const approximateMark = " ≈"

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

// Statusline answers a statusline request with entries, one per app,
// and beam, which replaces the tenant as the account label that
// statusline's `account` segment shows ("beam: quiet-otter"),
// as measured at observed. No entries is an answer with no limits rather
// than an error, so statusline hides the segments instead of reporting a
// failure. now is when the answer is made: a window that has reset since
// is left out, and so is an observation from the future, which the
// protocol refuses outright.
func Statusline(req []byte, proxy, beam string, observed time.Time, entries []StatuslineEntry, now time.Time) ([]byte, error) {
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
	if beam != "" {
		// Most people use one cluster, so the beam says more than its name.
		resp.Account.Label = beam
	}
	if observed.IsZero() || observed.After(now) {
		return json.Marshal(resp)
	}
	resp.ObservedAt = observed.UTC()
	seen := map[string]bool{}
	for _, e := range entries {
		// One window per app: the protocol wants unique ids.
		if e.Window.Ended(now) || seen[e.App] {
			continue
		}
		seen[e.App] = true
		lim := statuslineLimit{ID: StatuslineLimitID(e.App), Label: statuslineLabels[e.App], Approximate: e.Approximate}
		if lim.Label == "" {
			lim.Label = e.App
		}
		if e.Note != "" {
			lim.Label += " (" + e.Note + ")"
		}
		if e.Approximate {
			lim.Label += approximateMark
		}
		if pct, ok := UsedPercent(e.Window, e.Limit); ok {
			lim.UsedPercent = &pct
		}
		w := e.Window
		if w.StartsAt != nil && w.EndsAt != nil && w.EndsAt.After(*w.StartsAt) {
			secs := uint64(w.EndsAt.Sub(*w.StartsAt) / time.Second)
			lim.WindowSeconds = &secs
			ends := w.EndsAt.UTC()
			lim.ResetsAt = &ends
		}
		resp.Limits = append(resp.Limits, lim)
	}
	return json.Marshal(resp)
}
