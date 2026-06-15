// Package usage tracks token consumption from proxied API requests.
package usage

import (
	"encoding/json"
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

// Writer appends usage records to a JSONL file.
type Writer struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// NewWriter opens (or creates) the usage log file for appending.
func NewWriter(path string) (*Writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Writer{file: f, enc: json.NewEncoder(f)}, nil
}

// Write appends a record to the log.
func (w *Writer) Write(r Record) {
	if r.Timestamp.IsZero() {
		r.Timestamp = time.Now().UTC()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.enc.Encode(r)
}

// Close closes the underlying file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}

// Path returns the default usage log path (~/.config/prism/usage.jsonl).
func Path() (string, error) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "prism", "usage.jsonl"), nil
}
