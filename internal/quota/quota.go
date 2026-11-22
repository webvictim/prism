// Package quota reads a Teleport tenant's LLM token quotas: the
// `llm_endpoint_quota` resources the cluster keeps per LLM app, which
// count the tokens every user and beam has spent in the current window.
//
// The counters are all the cluster will tell anyone. The limits they are
// measured against live in the app's config (`inference.quota`), a v19
// field that the 18.x tsh and tctl clusters pin drop on the floor, so the
// limits here are prism's own defaults unless configured (limits.go).
//
// Everything in this package is pure: fetching (tctl, behind a login
// check) lives in cmd/prism/quota.go, so the parsing and the arithmetic
// can be tested without a cluster.
package quota

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Intervals the quota windows are named by. Only these two have
// defaults; Teleport also knows weekly, monthly and yearly.
const (
	IntervalDaily    = "daily"
	IntervalLifetime = "lifetime"
)

// Window is one active quota window of an app. StartsAt and EndsAt are
// nil for a lifetime window, which never resets.
type Window struct {
	Interval     string     `json:"interval"`
	StartsAt     *time.Time `json:"starts_at,omitempty"`
	EndsAt       *time.Time `json:"ends_at,omitempty"`
	InputTokens  uint64     `json:"input_tokens"`
	OutputTokens uint64     `json:"output_tokens"`
}

// Ended reports whether the window has reset since it was read, in which
// case its counts no longer describe anything.
func (w Window) Ended(now time.Time) bool {
	return w.EndsAt != nil && !now.Before(*w.EndsAt)
}

// Endpoint is one LLM app's quota, named as the app is (anthropic, openai).
type Endpoint struct {
	Name    string   `json:"name"`
	Windows []Window `json:"windows"`
}

// tokens decodes a uint64 that protojson writes as a string ("6752"),
// accepting a bare number too.
type tokens uint64

func (t *tokens) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		*t = 0
		return nil
	}
	n, err := strconv.ParseUint(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("token count %q: %w", b, err)
	}
	*t = tokens(n)
	return nil
}

// ParseTctl reads `tctl get llm_endpoint_quota --format=json`: an array of
// resources whose status carries the active windows.
func ParseTctl(b []byte) ([]Endpoint, error) {
	var raw []struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			ActiveWindows []struct {
				IntervalName string     `json:"interval_name"`
				StartsAt     *time.Time `json:"starts_at"`
				EndsAt       *time.Time `json:"ends_at"`
				InputTokens  tokens     `json:"input_tokens"`
				OutputTokens tokens     `json:"output_tokens"`
			} `json:"active_windows"`
		} `json:"status"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parsing tctl's quota listing: %w", err)
	}
	var out []Endpoint
	for _, r := range raw {
		if r.Kind != "" && r.Kind != "llm_endpoint_quota" {
			continue
		}
		e := Endpoint{Name: r.Metadata.Name}
		for _, w := range r.Status.ActiveWindows {
			e.Windows = append(e.Windows, Window{
				Interval:     w.IntervalName,
				StartsAt:     w.StartsAt,
				EndsAt:       w.EndsAt,
				InputTokens:  uint64(w.InputTokens),
				OutputTokens: uint64(w.OutputTokens),
			})
		}
		out = append(out, e)
	}
	return out, nil
}

// Cache is <config dir>/quota.json, written by the daemon's refresher and
// read by everything that displays a quota, so no display ever waits on
// the cluster.
type Cache struct {
	// Proxy is the cluster the numbers belong to: config.Proxy at the
	// time. A reader configured for another cluster ignores the file.
	Proxy string `json:"proxy"`
	// FetchedAt is when Endpoints were read; zero if they never were.
	FetchedAt time.Time  `json:"fetched_at,omitzero"`
	Endpoints []Endpoint `json:"endpoints,omitempty"`
	// CheckedAt is the last attempt, successful or not, and Problem why
	// it didn't read (no login, no tctl, not permitted), if it didn't.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	Problem   string    `json:"problem,omitempty"`
	// Limits are the apps' configured limits, when the cluster said
	// (limits.go). They are read separately from the usage, with less
	// permission, so a cache can hold limits and no usage at all.
	Limits []AppLimits `json:"limits,omitempty"`
}

// Endpoint returns the named app's quota, if the cache has it.
func (c *Cache) Endpoint(name string) (Endpoint, bool) {
	if c == nil {
		return Endpoint{}, false
	}
	for _, e := range c.Endpoints {
		if e.Name == name {
			return e, true
		}
	}
	return Endpoint{}, false
}

// FileName is the cache's name in prism's config directory.
const FileName = "quota.json"

// Load reads the cache in dir for proxy. A missing file, or one written
// for a different cluster, is (nil, nil): there is simply nothing to show.
func Load(dir, proxy string) (*Cache, error) {
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var c Cache
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", FileName, err)
	}
	if proxy == "" || c.Proxy != proxy {
		return nil, nil
	}
	return &c, nil
}

// Save writes the cache atomically, 0600.
func Save(dir string, c *Cache) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".quota-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, FileName)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// nudge carries "a request was just refused for quota" from the request
// path to the daemon's refresher, so the displays catch up at once rather
// than at the next scheduled read. Buffered by one: a burst of refusals is
// one refresh.
var nudge = make(chan struct{}, 1)

// Nudge asks the refresher for a prompt refresh. It never blocks.
func Nudge() {
	select {
	case nudge <- struct{}{}:
	default:
	}
}

// Nudged is what the refresher selects on.
func Nudged() <-chan struct{} { return nudge }
