package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/webvictim/prism/internal/quota"
)

// `prism setup statusline` points statusline (github.com/ryanclark/statusline)
// at `prism usage --statusline` as its external usage source, and adds a
// `usage_limit` segment per LLM app, so the tenant's quota is on screen in
// every Claude Code session.
//
// One trade-off has to be said out loud: while an external source is
// configured, statusline stops fetching claude.ai account usage, so its
// five_hour / seven_day segments go quiet. Sessions through prism's router
// have no claude.ai usage to show anyway; forward-proxy sessions do.

// statuslineApps are the apps a segment is added for, in display order.
var statuslineApps = []string{"anthropic", "openai"}

// statuslineDefaultSegments is statusline's layout when settings.json has
// no `segments` (its README). Adding to an absent list would replace that
// layout with prism's two segments alone, so it is written out first.
var statuslineDefaultSegments = []any{
	"context_percentage", "total_input_tokens", "output_tokens", "divider",
	"five_hour", "seven_day", "divider", "extra_usage",
}

func statuslineSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".statusline", "settings.json"), nil
}

func setupStatusline(args []string) error {
	fs := flag.NewFlagSet("setup statusline", flag.ContinueOnError)
	remove := fs.Bool("remove", false, "take prism's usage source and segments back out")
	dryRun := fs.Bool("dry-run", false, "print the settings that would be written, and change nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := statuslineSettingsPath()
	if err != nil {
		return err
	}
	settings := map[string]any{}
	orig, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(orig, &settings); err != nil {
			return fmt.Errorf("%s isn't valid JSON (%v); fix or move it first", path, err)
		}
	}

	self, err := serviceProgramPath()
	if err != nil {
		return err
	}
	if *remove {
		changed := removeStatuslineSettings(settings)
		if !changed {
			fmt.Fprintf(os.Stderr, "prism: %s has nothing of prism's in it\n", path)
			return nil
		}
		return writeStatuslineSettings(path, orig, settings, *dryRun, "removed prism's usage source and segments")
	}
	if err := addStatuslineSettings(settings, self); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := writeStatuslineSettings(path, orig, settings, *dryRun, "statusline now shows the tenant's quota"); err != nil {
		return err
	}
	if !*dryRun {
		fmt.Fprintln(os.Stderr, `
  Segments: "Claude" and "OpenAI" (usage_limit, limit_id beams_anthropic /
  beams_openai). They stay empty until the daemon has read the quota, which
  needs a live tsh login to the tenant: `+"`prism usage`"+` says why if it can't.

  Note: with an external usage source, statusline stops fetching claude.ai
  account usage, so five_hour / seven_day go quiet. Undo with
  `+"`prism setup statusline --remove`"+`.`)
	}
	return nil
}

// isPrismUsageSource reports whether a usage_source is the one prism
// writes, by its arguments (the binary's path may have moved).
func isPrismUsageSource(v any) bool {
	src, ok := v.(map[string]any)
	if !ok {
		return false
	}
	cmd, ok := src["command"].([]any)
	if !ok || len(cmd) != 3 {
		return false
	}
	return cmd[1] == "usage" && cmd[2] == "--statusline" && filepath.Base(fmt.Sprint(cmd[0])) == "prism"
}

func isPrismSegment(v any) bool {
	seg, ok := v.(map[string]any)
	if !ok || seg["type"] != "usage_limit" {
		return false
	}
	for _, app := range statuslineApps {
		if seg["limit_id"] == quota.StatuslineLimitID(app) {
			return true
		}
	}
	return false
}

func addStatuslineSettings(settings map[string]any, self string) error {
	if src, ok := settings["usage_source"]; ok && !isPrismUsageSource(src) {
		return errors.New("it already has a different usage_source; statusline takes one, so prism won't replace it")
	}
	// statusline refuses a settings file without these two, so a file
	// prism creates carries statusline's own defaults (settings.rs).
	for key, def := range map[string]float64{"five_hour_reset_threshold": 70, "seven_day_reset_threshold": 100} {
		if _, ok := settings[key]; !ok {
			settings[key] = def
		}
	}
	settings["usage_source"] = map[string]any{
		"command":          []any{self, "usage", "--statusline"},
		"refresh_interval": "60s",
		// The daemon reads every 2 minutes while busy and every 30 when
		// idle; older than this and statusline dims it as stale.
		"max_age": "35m",
		"timeout": "2s",
	}
	segs, ok := settings["segments"].([]any)
	if !ok {
		segs = slices.Clone(statuslineDefaultSegments)
	}
	if slices.ContainsFunc(segs, isPrismSegment) {
		settings["segments"] = segs
		return nil
	}
	segs = append(segs, "divider")
	for _, app := range statuslineApps {
		segs = append(segs, map[string]any{
			"type":           "usage_limit",
			"limit_id":       quota.StatuslineLimitID(app),
			"show_countdown": true,
		})
	}
	settings["segments"] = segs
	return nil
}

func removeStatuslineSettings(settings map[string]any) bool {
	changed := false
	if isPrismUsageSource(settings["usage_source"]) {
		delete(settings, "usage_source")
		changed = true
	}
	if segs, ok := settings["segments"].([]any); ok {
		kept := slices.DeleteFunc(slices.Clone(segs), isPrismSegment)
		if len(kept) != len(segs) {
			// The divider prism put in front of its segments.
			if n := len(kept); n > 0 && kept[n-1] == "divider" {
				kept = kept[:n-1]
			}
			settings["segments"] = kept
			changed = true
		}
	}
	return changed
}

func writeStatuslineSettings(path string, orig []byte, settings map[string]any, dryRun bool, done string) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(settings); err != nil {
		return err
	}
	if dryRun {
		os.Stdout.Write(buf.Bytes())
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if orig != nil {
		if err := os.WriteFile(path+".bak", orig, 0o600); err != nil {
			return err
		}
	}
	tmp := path + ".prism.tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	backup := ""
	if orig != nil {
		backup = fmt.Sprintf(" (previous copy: %s.bak)", path)
	}
	fmt.Fprintf(os.Stderr, "prism: %s — wrote %s%s\n", done, path, backup)
	return nil
}
