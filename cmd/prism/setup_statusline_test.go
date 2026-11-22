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
	if len(segs) != 7 || segs[4] != "divider" || !isPrismSegment(segs[5]) || !isPrismSegment(segs[6]) {
		t.Fatalf("segments: %v", segs)
	}
	if settings["five_hour_reset_threshold"] != 70.0 {
		t.Error("an unrelated key was lost")
	}
	// Again: nothing is added twice.
	_ = addStatuslineSettings(settings, "/opt/homebrew/opt/prism/bin/prism")
	if n := len(settings["segments"].([]any)); n != 7 {
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
