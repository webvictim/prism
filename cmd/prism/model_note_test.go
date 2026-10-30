package main

import (
	"strings"
	"testing"

	usagepkg "github.com/webvictim/prism/internal/usage"
)

func TestRecentSubstitutionNote(t *testing.T) {
	note := recentSubstitutionNote([]usagepkg.Record{
		{Model: "claude-sonnet-5", RequestedModel: "claude-opus-5-5"},
		{Model: "claude-sonnet-5", RequestedModel: "claude-opus-5-5"},
		{Model: "openai.gpt-5.6-luna", RequestedModel: "openai.gpt-6-astra"},
	})

	for _, want := range []string{
		"3 request(s)",
		"claude-opus-5-5 → claude-sonnet-5 (2)",
		"openai.gpt-6-astra → openai.gpt-5.6-luna (1)",
		"prism usage",
	} {
		if !strings.Contains(note, want) {
			t.Errorf("note %q is missing %q", note, want)
		}
	}
}

// Nothing swapped means nothing said: the note has to stay rare enough
// that it is still read when it appears.
func TestRecentSubstitutionNoteSilentWhenHonoured(t *testing.T) {
	for _, records := range [][]usagepkg.Record{
		nil,
		{{Model: "claude-sonnet-5"}},
		{{Model: "claude-sonnet-5", RequestedModel: "claude-sonnet-5"}},
	} {
		if got := recentSubstitutionNote(records); got != "" {
			t.Errorf("recentSubstitutionNote(%+v) = %q, want empty", records, got)
		}
	}
}

// Past two pairs the note counts the rest rather than listing them.
func TestRecentSubstitutionNoteCapsNamedPairs(t *testing.T) {
	records := []usagepkg.Record{
		{Model: "a", RequestedModel: "w"},
		{Model: "b", RequestedModel: "x"},
		{Model: "c", RequestedModel: "y"},
		{Model: "d", RequestedModel: "z"},
	}
	note := recentSubstitutionNote(records)
	if !strings.Contains(note, "and 2 more pair(s)") {
		t.Errorf("note %q should count the unnamed pairs", note)
	}
	if strings.Count(note, "→") != 2 {
		t.Errorf("note %q should name exactly two pairs", note)
	}
}
