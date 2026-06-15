// Package usage tracks token consumption from proxied API requests.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record represents one API call's token usage.
type Record struct {
	Timestamp    time.Time `json:"ts"`
	Model        string    `json:"model,omitempty"`
	Backend      string    `json:"backend"`
	Proxy        string    `json:"proxy,omitempty"`
	InputTokens  int64     `json:"input_tokens,omitempty"`
	OutputTokens int64     `json:"output_tokens,omitempty"`
	CacheRead    int64     `json:"cache_read,omitempty"`
	CacheCreate  int64     `json:"cache_creation,omitempty"`
}

// Writer appends usage records to monthly JSONL files (usage-YYYY-MM.jsonl).
// It rotates to a new file when the month changes.
type Writer struct {
	mu      sync.Mutex
	dir     string
	current string // current filename
	file    *os.File
	enc     *json.Encoder
}

// NewWriter opens (or creates) the usage log for the current month.
func NewWriter(dir string) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	w := &Writer{dir: dir}
	if err := w.openMonth(time.Now().UTC()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Writer) openMonth(t time.Time) error {
	name := monthFile(t)
	if name == w.current && w.file != nil {
		return nil
	}
	if w.file != nil {
		w.file.Close()
	}
	path := filepath.Join(w.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	w.enc = json.NewEncoder(f)
	w.current = name
	return nil
}

// Write appends a record to the current month's log.
func (w *Writer) Write(r Record) {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.openMonth(r.Timestamp)
	_ = w.enc.Encode(r)
}

// Close closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// Dir returns the usage data directory (~/.config/prism/usage/).
func Dir() (string, error) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "prism", "usage"), nil
}

func monthFile(t time.Time) string {
	return fmt.Sprintf("usage-%04d-%02d.jsonl", t.Year(), t.Month())
}

// FilesForRange returns the monthly filenames that could contain records
// in [since, now]. If since is zero, all files are returned.
func FilesForRange(dir string, since time.Time) ([]string, error) {
	// Always include a legacy usage.jsonl in parent dir if it exists.
	var paths []string
	legacy := filepath.Join(filepath.Dir(dir), "usage.jsonl")
	if _, err := os.Stat(legacy); err == nil {
		paths = append(paths, legacy)
	}

	if since.IsZero() {
		// Return all monthly files.
		matches, err := filepath.Glob(filepath.Join(dir, "usage-*.jsonl"))
		if err != nil {
			return nil, err
		}
		paths = append(paths, matches...)
		return paths, nil
	}

	// Only include months from since to now.
	now := time.Now().UTC()
	y, m, _ := since.Date()
	for {
		t := time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
		if t.After(now.AddDate(0, 1, 0)) {
			break
		}
		p := filepath.Join(dir, monthFile(t))
		if _, err := os.Stat(p); err == nil {
			paths = append(paths, p)
		}
		m++
		if m > 12 {
			m = 1
			y++
		}
	}
	return paths, nil
}
