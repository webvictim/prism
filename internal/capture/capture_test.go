package capture

import (
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/usage"
)

// Every field is always present so a log line can be parsed without
// first working out which fields it carries.
func TestSummaryAlwaysEmitsEveryField(t *testing.T) {
	got := Summary(usage.Record{
		Model:        "claude-opus-5",
		InputTokens:  2,
		OutputTokens: 89,
		CacheRead:    18807,
		CacheCreate:  209741,
	})
	want := "model=claude-opus-5 in=2 out=89 cache_read=18807 cache_write=209741"
	if got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
}

func TestSummaryKeepsZeroFields(t *testing.T) {
	got := Summary(usage.Record{Model: "gpt-5", InputTokens: 11, OutputTokens: 9})
	want := "model=gpt-5 in=11 out=9 cache_read=0 cache_write=0"
	if got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
}

// A model the gateway did not report is spelled "?" rather than left
// blank, so the field never collapses into the next one.
func TestSummaryUnknownModel(t *testing.T) {
	got := Summary(usage.Record{InputTokens: 1, OutputTokens: 1})
	if !strings.HasPrefix(got, "model=? ") {
		t.Errorf("Summary() = %q, want it to start with model=?", got)
	}
}

// Field count is fixed, which is the property that makes the line
// parseable; guard it so a future field addition is a deliberate act.
func TestSummaryFieldCount(t *testing.T) {
	fields := strings.Fields(Summary(usage.Record{Model: "m"}))
	if len(fields) != 5 {
		t.Errorf("Summary() has %d fields (%v), want 5", len(fields), fields)
	}
	for _, f := range fields {
		if !strings.Contains(f, "=") {
			t.Errorf("field %q is not key=value", f)
		}
	}
}
