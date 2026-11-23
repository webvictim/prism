package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/webvictim/prism/internal/config"
)

// `prism claude` shows the tenant's quota in statusline
// (github.com/ryanclark/statusline) with no setup, when Claude Code's
// status line is statusline: as its native `statusLine` command, or as
// its plugin. statusline only runs under Claude Code, so nothing else
// needs this.
//
// It must not change what statusline shows anywhere else. A usage_source
// in ~/.statusline/settings.json would: statusline reads that file for
// every Claude Code session on the machine, and with an external source
// it stops reading claude.ai usage, so a session not started by prism
// would lose its five-hour and seven-day figures. So the source goes in
// STATUSLINE_USAGE_SOURCE, which statusline reads ahead of the file "for
// that process", in this one Claude Code's environment, which its status
// line inherits. The settings file only gains prism's two usage_limit
// segments, which render nothing without prism's source, and a divider
// statusline drops when nothing follows it.
//
// Left alone: a session whose environment already sets the variable, a
// settings file with someone else's usage_source (statusline takes one),
// or one that isn't valid JSON. `statusline_auto false` switches it off,
// and `prism setup statusline --remove` sets that so a removal sticks.

const statuslineSourceEnv = "STATUSLINE_USAGE_SOURCE"

// statuslinePlugin is statusline's Claude Code plugin, as enabledPlugins
// names it.
const statuslinePlugin = "statusline@ryanclark"

// claudeSettingsPath is Claude Code's user settings file.
func claudeSettingsPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// claudeUsesStatusline reports whether Claude Code's user settings run
// statusline, as the plugin or as a statusLine command that names a
// statusline binary which exists.
func claudeUsesStatusline(settingsJSON []byte, lookPath func(string) (string, error)) bool {
	var s struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
		EnabledPlugins map[string]any `json:"enabledPlugins"`
	}
	if json.Unmarshal(settingsJSON, &s) != nil {
		return false
	}
	if on, _ := s.EnabledPlugins[statuslinePlugin].(bool); on {
		return true
	}
	fields := strings.Fields(s.StatusLine.Command)
	if len(fields) == 0 {
		return false
	}
	bin := strings.Trim(fields[0], `"'`)
	if name := strings.TrimSuffix(filepath.Base(bin), ".exe"); name != "statusline" {
		return false
	}
	_, err := lookPath(bin)
	return err == nil
}

// withStatuslineSource returns env with prism's usage source added for a
// Claude Code launch, adding prism's segments to statusline's settings
// first when they're missing. Every failure leaves env as it was: this is
// a display nicety and never stops a launch.
func withStatuslineSource(cfg *config.Config, env []string, notes io.Writer) []string {
	if !cfg.StatuslineAutoEnabled() || hasEnv(env, statuslineSourceEnv) {
		return env
	}
	claudePath, err := claudeSettingsPath()
	if err != nil {
		return env
	}
	claudeJSON, err := os.ReadFile(claudePath)
	if err != nil || !claudeUsesStatusline(claudeJSON, exec.LookPath) {
		return env
	}
	path, err := statuslineSettingsPath()
	if err != nil {
		return env
	}
	self, err := serviceProgramPath()
	if err != nil {
		return env
	}
	value, added, err := applyStatuslineAuto(path, self, cfg.StatuslineBeamShown() != config.StatuslineBeamOff)
	if err != nil {
		fmt.Fprintf(notes, "prism: statusline: %v\n", err)
		return env
	}
	if added {
		fmt.Fprintf(notes, "prism: added the tenant's token quota to statusline (%s); "+
			"`prism config set statusline_auto false` turns this off\n", path)
	}
	if value == "" {
		return env
	}
	return append(env, statuslineSourceEnv+"="+value)
}

// applyStatuslineAuto adds prism's segments to the statusline settings at
// path if needed and returns the STATUSLINE_USAGE_SOURCE value to set, or
// "" when the environment should be left alone, and whether it wrote the
// file.
func applyStatuslineAuto(path, self string, beam bool) (value string, added bool, err error) {
	settings := map[string]any{}
	orig, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		orig = nil
	case err != nil:
		return "", false, nil
	default:
		if json.Unmarshal(orig, &settings) != nil {
			return "", false, nil // statusline will complain about it itself
		}
	}
	if _, ok := settings["usage_source"]; ok {
		// Someone else's source: theirs to keep. prism's own (from
		// `prism setup statusline`) already applies to every session.
		return "", false, nil
	}
	if addStatuslineSegments(settings, beam) {
		if err := writeStatuslineSettings(path, orig, settings, false, ""); err != nil {
			return "", false, fmt.Errorf("couldn't add prism's segments to %s: %w", path, err)
		}
		added = true
	}
	b, err := json.Marshal(prismUsageSource(self))
	if err != nil {
		return "", false, err
	}
	return string(b), added, nil
}

// syncStatuslineBeam puts statusline's settings in step with
// statusline_beam at once, rather than at the next `prism claude`:
// statusline reads the file on every render, while running sessions keep
// their segments until something changes it. Only a file already holding
// prism's segments is touched, and a failure is a note, never an error:
// the setting itself was saved.
func syncStatuslineBeam(cfg *config.Config, notes io.Writer) {
	path, err := statuslineSettingsPath()
	if err != nil {
		return
	}
	changed, err := syncStatuslineBeamAt(path, cfg.StatuslineBeamShown() != config.StatuslineBeamOff)
	switch {
	case err != nil:
		fmt.Fprintf(notes, "prism: statusline: %v\n", err)
	case changed:
		fmt.Fprintf(notes, "prism: updated statusline's segments (%s)\n", path)
	}
}

func syncStatuslineBeamAt(path string, beam bool) (bool, error) {
	orig, err := os.ReadFile(path)
	if err != nil {
		return false, nil // no statusline settings: nothing to keep in step
	}
	settings := map[string]any{}
	if json.Unmarshal(orig, &settings) != nil {
		return false, nil
	}
	segs, _ := settings["segments"].([]any)
	if !slices.ContainsFunc(segs, isPrismSegment) || !addStatuslineSegments(settings, beam) {
		return false, nil
	}
	if err := writeStatuslineSettings(path, orig, settings, false, ""); err != nil {
		return false, fmt.Errorf("couldn't update %s: %w", path, err)
	}
	return true, nil
}
