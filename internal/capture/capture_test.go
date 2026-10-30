package capture

import (
	"net/http"
	"net/http/httptest"
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

// A substituted model is the one documented exception to the fixed
// field count: the served name keeps the `model=` key it always had,
// the requested name arrives in front of it, and `[!]` closes the pair.
func TestSummaryMarksSubstitution(t *testing.T) {
	got := Summary(usage.Record{
		RequestedModel: "claude-opus-5-5",
		Model:          "claude-sonnet-5",
		InputTokens:    2,
		OutputTokens:   89,
	})
	want := "requested=claude-opus-5-5 model=claude-sonnet-5 [!] in=2 out=89 cache_read=0 cache_write=0"
	if got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
}

// And a record that names the same model twice is not a substitution,
// however it got that way — the marker has to mean something.
func TestSummaryIgnoresNonSubstitution(t *testing.T) {
	for _, rec := range []usage.Record{
		{RequestedModel: "claude-sonnet-5", Model: "claude-sonnet-5"},
		{RequestedModel: "Claude-Sonnet-5", Model: "claude-sonnet-5"}, // the gateway folds case
		{RequestedModel: "claude-opus-5-5"},                           // nothing served
	} {
		if got := Summary(rec); strings.Contains(got, "[!]") || strings.Contains(got, "requested=") {
			t.Errorf("Summary(%+v) = %q, want no substitution marker", rec, got)
		}
	}
}

// End to end on the non-streaming path: the request names one model,
// the response another, and the record keeps both. Summary can only
// mark a substitution the Writer noticed, so this is the part that
// actually has to work.
func TestWriterRecordsSubstitution(t *testing.T) {
	rec := captureResponse(t, BackendAnthropic, "claude-opus-5-5",
		"application/json",
		`{"model":"claude-sonnet-5","usage":{"input_tokens":2,"output_tokens":5}}`)

	if rec.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the served claude-sonnet-5", rec.Model)
	}
	if rec.RequestedModel != "claude-opus-5-5" {
		t.Errorf("RequestedModel = %q, want claude-opus-5-5", rec.RequestedModel)
	}
}

// The streaming path is the one with the trap: message_start overwrites
// the writer's model with the served name, so the requested name has to
// be kept somewhere that overwrite can't reach.
func TestWriterRecordsSubstitutionStreaming(t *testing.T) {
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-sonnet-5","usage":{"input_tokens":2,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n"

	rec := captureResponse(t, BackendAnthropic, "claude-opus-5-5", "text/event-stream", body)

	if rec.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the served claude-sonnet-5", rec.Model)
	}
	if rec.RequestedModel != "claude-opus-5-5" {
		t.Errorf("RequestedModel = %q, want claude-opus-5-5", rec.RequestedModel)
	}
	if !Substituted(rec) {
		t.Error("Substituted() = false, want true")
	}
}

// A request the gateway honoured records no requested model at all:
// storing it twice is what makes the field mean "substituted".
func TestWriterOmitsRequestedModelWhenHonoured(t *testing.T) {
	rec := captureResponse(t, BackendAnthropic, "claude-sonnet-5",
		"application/json",
		`{"model":"claude-sonnet-5","usage":{"input_tokens":2,"output_tokens":5}}`)

	if rec.RequestedModel != "" {
		t.Errorf("RequestedModel = %q, want it empty", rec.RequestedModel)
	}
	if Substituted(rec) {
		t.Error("Substituted() = true, want false")
	}
}

// captureResponse drives a Writer through one response and returns the
// finalized record.
func captureResponse(t *testing.T, backend, requested, contentType, body string) usage.Record {
	t.Helper()

	uw, err := usage.NewWriter(t.TempDir())
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer uw.Close()

	rr := httptest.NewRecorder()
	cw := New(rr, Options{Backend: backend, Model: requested, UsageWriter: uw})
	cw.Header().Set("Content-Type", contentType)
	cw.WriteHeader(http.StatusOK)
	if _, err := cw.Write([]byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	rec, ok := cw.Finalize()
	if !ok {
		t.Fatal("Finalize() reported no usage")
	}
	return rec
}
