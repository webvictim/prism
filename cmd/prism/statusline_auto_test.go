package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/config"
)

func TestClaudeUsesStatusline(t *testing.T) {
	found := func(string) (string, error) { return "/opt/homebrew/bin/statusline", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for _, tc := range []struct {
		settings string
		look     func(string) (string, error)
		want     bool
	}{
		{`{"statusLine":{"type":"command","command":"statusline"}}`, found, true},
		{`{"statusLine":{"command":"/opt/homebrew/bin/statusline --flag"}}`, found, true},
		{`{"statusLine":{"command":"statusline"}}`, missing, false},
		{`{"statusLine":{"command":"~/bin/my-status.sh"}}`, found, false},
		{`{"enabledPlugins":{"statusline@ryanclark":true}}`, missing, true},
		{`{"enabledPlugins":{"statusline@ryanclark":false}}`, found, false},
		{`{}`, found, false},
		{`not json`, found, false},
	} {
		if got := claudeUsesStatusline([]byte(tc.settings), tc.look); got != tc.want {
			t.Errorf("%s: got %v", tc.settings, got)
		}
	}
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestStatuslineAutoAddsSegmentsButNoSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	value, added, err := applyStatuslineAuto(path, "/opt/homebrew/bin/prism")
	if err != nil || !added {
		t.Fatalf("first launch: %v %v", added, err)
	}
	var src map[string]any
	if err := json.Unmarshal([]byte(value), &src); err != nil || !isPrismUsageSource(src) {
		t.Fatalf("env value %q isn't prism's source: %v", value, err)
	}
	s := readSettings(t, path)
	if _, ok := s["usage_source"]; ok {
		// The whole point: other sessions keep their claude.ai usage.
		t.Fatal("the settings file must not gain a usage_source")
	}
	segs, _ := s["segments"].([]any)
	if !slices.ContainsFunc(segs, isPrismSegment) || !slices.Contains(segs, any("five_hour")) {
		t.Fatalf("segments: %v", segs)
	}
	if _, ok := s["five_hour_reset_threshold"]; !ok {
		t.Fatal("a new file needs statusline's required thresholds")
	}

	before, _ := os.ReadFile(path)
	if _, added, err := applyStatuslineAuto(path, "/opt/homebrew/bin/prism"); err != nil || added {
		t.Fatalf("second launch rewrote it: %v %v", added, err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatal("second launch changed the file")
	}
}

func TestStatuslineAutoLeavesSourcesAndBadFilesAlone(t *testing.T) {
	for name, body := range map[string]string{
		"foreign source": `{"usage_source":{"command":["beast","usage"]},"segments":["model"]}`,
		"prism's source": `{"usage_source":{"command":["/x/prism","usage","--statusline"]},"segments":["model"]}`,
		"invalid json":   `{"segments":[`,
	} {
		path := filepath.Join(t.TempDir(), "settings.json")
		os.WriteFile(path, []byte(body), 0o644)
		value, added, err := applyStatuslineAuto(path, "/opt/homebrew/bin/prism")
		if value != "" || added || err != nil {
			t.Errorf("%s: %q %v %v", name, value, added, err)
		}
		if b, _ := os.ReadFile(path); string(b) != body {
			t.Errorf("%s: file changed to %s", name, b)
		}
	}
}

func TestWithStatuslineSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	claudeDir := filepath.Join(home, ".claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	os.MkdirAll(claudeDir, 0o755)
	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"enabledPlugins":{"statusline@ryanclark":true}}`), 0o644)

	env := withStatuslineSource(nil, []string{"A=1"}, io.Discard)
	if len(env) != 2 || !strings.HasPrefix(env[1], statuslineSourceEnv+"=") {
		t.Fatalf("on by default: %q", env)
	}

	off := false
	if env := withStatuslineSource(&config.Config{StatuslineAuto: &off}, []string{"A=1"}, io.Discard); len(env) != 1 {
		t.Errorf("statusline_auto false: %q", env)
	}
	mine := []string{statuslineSourceEnv + `={"command":["mine"]}`}
	if env := withStatuslineSource(nil, mine, io.Discard); !slices.Equal(env, mine) {
		t.Errorf("the user's own value must win: %q", env)
	}

	os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte(`{"statusLine":{"command":"ccstatusline"}}`), 0o644)
	if env := withStatuslineSource(nil, []string{"A=1"}, io.Discard); len(env) != 1 {
		t.Errorf("another status line: %q", env)
	}
}

func TestSetupStatuslineRemoveSticks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	stderr := os.Stderr
	os.Stderr = devnull
	defer func() { os.Stderr = stderr }()

	if err := setupStatusline([]string{"--remove"}); err != nil {
		t.Fatal(err)
	}
	c, _ := config.Load()
	if c.StatuslineAutoEnabled() {
		t.Fatal("--remove must turn statusline_auto off, or the next prism claude puts the segments back")
	}
	if err := setupStatusline(nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := config.Load(); c.StatuslineAuto != nil {
		t.Fatalf("setup must restore the default: %v", *c.StatuslineAuto)
	}
}
