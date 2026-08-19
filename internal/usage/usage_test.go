package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriterAndLoad(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	w.Write(Record{Timestamp: now, Model: "claude-opus-4-6", Backend: "anthropic", InputTokens: 100, OutputTokens: 20})
	w.Write(Record{Timestamp: now, Model: "gpt-4o", Backend: "openai", InputTokens: 50, OutputTokens: 5})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// The current month's file should exist.
	if _, err := os.Stat(filepath.Join(dir, monthFile(now))); err != nil {
		t.Fatalf("monthly file not written: %v", err)
	}

	records, err := Load(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].Model != "claude-opus-4-6" || records[0].InputTokens != 100 {
		t.Errorf("first record = %+v", records[0])
	}
}

func TestWriterFillsTimestamp(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(Record{Model: "m", Backend: "anthropic", InputTokens: 1})
	w.Close()

	records, err := Load(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Timestamp.IsZero() {
		t.Fatalf("timestamp not filled in: %+v", records)
	}
}

func TestLoadSinceFilter(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	w.Write(Record{Timestamp: now.Add(-2 * time.Hour), Model: "old", Backend: "anthropic", InputTokens: 1})
	w.Write(Record{Timestamp: now, Model: "new", Backend: "anthropic", InputTokens: 2})
	w.Close()

	records, err := Load(dir, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Model != "new" {
		t.Fatalf("since filter failed: %+v", records)
	}
}

func TestFilesForRangeIncludesLegacyFile(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(parent, "usage.jsonl")
	if err := os.WriteFile(legacy, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	paths, err := FilesForRange(dir, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != legacy {
		t.Fatalf("paths = %v, want just the legacy file", paths)
	}
}

func TestSummary(t *testing.T) {
	records := []Record{
		{Model: "a", Backend: "anthropic", InputTokens: 10, OutputTokens: 1, CacheRead: 5, CacheCreate: 2},
		{Model: "a", Backend: "anthropic", InputTokens: 20, OutputTokens: 2},
		{Model: "b", Backend: "openai", InputTokens: 1000, OutputTokens: 100},
	}
	sums := Summary(records)
	if len(sums) != 2 {
		t.Fatalf("got %d summaries, want 2", len(sums))
	}
	// Sorted by total tokens descending — "b" first.
	if sums[0].Model != "b" || sums[0].Requests != 1 {
		t.Errorf("first summary = %+v, want model b", sums[0])
	}
	a := sums[1]
	if a.Model != "a" || a.Requests != 2 || a.InputTokens != 30 || a.OutputTokens != 3 || a.CacheRead != 5 || a.CacheCreate != 2 {
		t.Errorf("summary for a = %+v", a)
	}
}

func TestSummaryByProxy(t *testing.T) {
	records := []Record{
		{Proxy: "p1", InputTokens: 10, OutputTokens: 1},
		{Proxy: "p1", InputTokens: 20, OutputTokens: 2},
		{Proxy: "", InputTokens: 5},
	}
	sums := SummaryByProxy(records)
	if len(sums) != 2 {
		t.Fatalf("got %d summaries, want 2", len(sums))
	}
	if sums[0].Proxy != "p1" || sums[0].Requests != 2 || sums[0].InputTokens != 30 {
		t.Errorf("first summary = %+v", sums[0])
	}
	if sums[1].Proxy != "(unknown)" {
		t.Errorf("empty proxy = %q, want (unknown)", sums[1].Proxy)
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{890, "890"},
		{45_300, "45.3k"},
		{1_200_000, "1.2M"},
		{0, "0"},
	}
	for _, tt := range tests {
		if got := FormatTokens(tt.n); got != tt.want {
			t.Errorf("FormatTokens(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
