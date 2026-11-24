package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestSetupStatuslineAddsAndRemoves(t *testing.T) {
	var settings map[string]any
	_ = json.Unmarshal([]byte(`{"five_hour_reset_threshold":70,
	  "segments":["context_percentage",{"type":"git_branch","dirty":true},"divider","five_hour"]}`), &settings)

	if err := addStatuslineSettings(settings, "/opt/homebrew/bin/prism"); err != nil {
		t.Fatal(err)
	}
	if !isPrismUsageSource(settings["usage_source"]) {
		t.Fatalf("usage_source: %v", settings["usage_source"])
	}
	segs := settings["segments"].([]any)
	if len(segs) != 9 || segs[4] != "divider" || !isAccountSegment(segs[5]) || !isPrismSegment(segs[6]) || segs[7] != "divider" || !isPrismSegment(segs[8]) {
		t.Fatalf("segments: %v", segs)
	}
	if settings["five_hour_reset_threshold"] != 70.0 {
		t.Error("an unrelated key was lost")
	}
	// Again: nothing is added twice.
	_ = addStatuslineSettings(settings, "/opt/homebrew/opt/prism/bin/prism")
	if n := len(settings["segments"].([]any)); n != 9 {
		t.Fatalf("re-running added segments: %d", n)
	}

	if !removeStatuslineSettings(settings) {
		t.Fatal("remove changed nothing")
	}
	if _, ok := settings["usage_source"]; ok {
		t.Error("usage_source left behind")
	}
	if segs := settings["segments"].([]any); len(segs) != 4 || segs[3] != "five_hour" {
		t.Errorf("segments after remove: %v", segs)
	}
	if removeStatuslineSettings(settings) {
		t.Error("a second remove changed something")
	}
}

func TestSetupStatuslineKeepsTheDefaultLayout(t *testing.T) {
	settings := map[string]any{}
	_ = addStatuslineSettings(settings, "/usr/local/bin/prism")
	segs := settings["segments"].([]any)
	if !slices.Equal(segs[:len(statuslineDefaultSegments)], statuslineDefaultSegments) {
		t.Errorf("an absent segments list lost statusline's defaults: %v", segs)
	}
	if settings["five_hour_reset_threshold"] != 70.0 || settings["seven_day_reset_threshold"] != 100.0 {
		t.Errorf("a new file lacks the fields statusline requires: %v", settings)
	}
}

func TestSetupStatuslineLeavesAnotherSourceAlone(t *testing.T) {
	settings := map[string]any{"usage_source": map[string]any{"command": []any{"python3", "/x/usage-file.py"}}}
	err := addStatuslineSettings(settings, "/usr/local/bin/prism")
	if err == nil || !strings.Contains(err.Error(), "different usage_source") {
		t.Fatalf("got %v", err)
	}
	if removeStatuslineSettings(settings) {
		t.Error("remove took out someone else's usage_source")
	}
}

// An install from before the account segment carried the beam gains it,
// once, in front of prism's limits, and a divider between them.
func TestSetupStatuslineAddsTheAccountToAnOlderInstall(t *testing.T) {
	var settings map[string]any
	_ = json.Unmarshal([]byte(`{"segments":["model","divider",
	  {"type":"usage_limit","limit_id":"beams_anthropic"},{"type":"usage_limit","limit_id":"beams_openai"}]}`), &settings)
	if !addStatuslineSegments(settings) {
		t.Fatal("nothing added")
	}
	segs := settings["segments"].([]any)
	if len(segs) != 6 || !isAccountSegment(segs[2]) || !isPrismSegment(segs[3]) || segs[4] != "divider" || !isPrismSegment(segs[5]) {
		t.Fatalf("segments: %v", segs)
	}
	if addStatuslineSegments(settings) {
		t.Error("added again")
	}
	removeStatuslineSettings(settings)
	if segs := settings["segments"].([]any); len(segs) != 1 || segs[0] != "model" {
		t.Errorf("after remove: %v", segs)
	}
}

// A divider separates the two limits, and goes with them on removal;
// a pass over a current install changes nothing.
func TestSetupStatuslineSeparatesTheLimits(t *testing.T) {
	settings := map[string]any{"segments": []any{"model"}}
	addStatuslineSegments(settings)
	segs := settings["segments"].([]any)
	if len(segs) != 6 || segs[1] != "divider" || !isAccountSegment(segs[2]) || !isPrismSegment(segs[3]) || segs[4] != "divider" || !isPrismSegment(segs[5]) {
		t.Fatalf("fresh = %v", segs)
	}
	if addStatuslineSegments(settings) {
		t.Error("a second pass changed something")
	}
	removeStatuslineSettings(settings)
	if segs := settings["segments"].([]any); len(segs) != 1 || segs[0] != "model" {
		t.Errorf("after remove: %v", segs)
	}
}
