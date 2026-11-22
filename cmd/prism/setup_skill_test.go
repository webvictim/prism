package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/beam"
	"github.com/webvictim/prism/internal/config"
)

func TestSkillIsWellFormed(t *testing.T) {
	s := string(beamSkill)
	if !strings.HasPrefix(s, "---\nname: "+skillName+"\n") {
		t.Errorf("skill must open with frontmatter naming %q (the directory name); got %q", skillName, s[:min(len(s), 40)])
	}
	if !strings.Contains(s, "\ndescription: ") {
		t.Error("skill frontmatter has no description, which is what triggers it")
	}
	if !strings.Contains(s, skillMarker) {
		t.Error("skill lacks the marker that lets prism update it")
	}
	for _, cmd := range []string{"prism beam status", "prism login", "prism beam end", "prism beam link --pr"} {
		if !strings.Contains(s, cmd) {
			t.Errorf("skill doesn't mention %q", cmd)
		}
	}
	// Agents must never be steered into sending inference unrecorded.
	if strings.Contains(s, "beam_mode") {
		t.Error("skill mentions beam_mode: an agent must not suggest turning recording off")
	}
}

func TestInstallSkillRespectsUserFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, skillName, "SKILL.md")

	if msg, err := installSkill(path, beamSkill); err != nil || msg != "installed" {
		t.Fatalf("install = %q, %v", msg, err)
	}
	if msg, _ := installSkill(path, beamSkill); msg != "already up to date" {
		t.Errorf("reinstall = %q", msg)
	}
	newer := append([]byte(nil), beamSkill...)
	newer = append(newer, []byte("\nmore\n")...)
	if msg, _ := installSkill(path, newer); msg != "updated" {
		t.Errorf("update = %q", msg)
	}

	// A user who removes the marker owns the file from then on.
	if err := os.WriteFile(path, []byte("my own notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if msg, _ := installSkill(path, beamSkill); !strings.Contains(msg, "left alone") {
		t.Errorf("overwrote a user file: %q", msg)
	}
	if msg, _ := removeSkill(path); !strings.Contains(msg, "left alone") {
		t.Errorf("removed a user file: %q", msg)
	}
	if b, _ := os.ReadFile(path); string(b) != "my own notes" {
		t.Errorf("user file changed: %q", b)
	}

	// prism's own file goes, directory and all.
	if err := os.WriteFile(path, beamSkill, 0o644); err != nil {
		t.Fatal(err)
	}
	if msg, err := removeSkill(path); err != nil || msg != "removed" {
		t.Fatalf("remove = %q, %v", msg, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Error("empty skill directory left behind")
	}
	if msg, _ := removeSkill(path); msg != "not installed" {
		t.Errorf("second remove = %q", msg)
	}
}

func TestSkillDirsCoverBothLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	dirs, err := skillDirs()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(home, ".claude", "skills"), filepath.Join(home, ".agents", "skills")}
	if strings.Join(dirs, "|") != strings.Join(want, "|") {
		t.Errorf("dirs = %v, want %v", dirs, want)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "alt"))
	dirs, _ = skillDirs()
	if dirs[0] != filepath.Join(home, "alt", "skills") {
		t.Errorf("CLAUDE_CONFIG_DIR ignored: %v", dirs)
	}
}

func TestKeepSkillCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	claudeSkill := filepath.Join(home, ".claude", "skills", skillName, "SKILL.md")
	agentsSkill := filepath.Join(home, ".agents", "skills", skillName, "SKILL.md")

	// Recording off (an unswitched tbot machine): nothing written.
	keepSkillCurrent(&config.Config{Identity: "tbot"})
	if _, err := os.Stat(claudeSkill); err == nil {
		t.Fatal("installed the skill with recording off")
	}
	// The default: installed in both places.
	keepSkillCurrent(&config.Config{})
	for _, p := range []string{claudeSkill, agentsSkill} {
		if b, err := os.ReadFile(p); err != nil || string(b) != string(beamSkill) {
			t.Errorf("%s not installed: %v", p, err)
		}
	}
	// A stale prism copy is updated; a file the user owns is left alone.
	_ = os.WriteFile(claudeSkill, []byte("old "+skillMarker), 0o644)
	_ = os.WriteFile(agentsSkill, []byte("mine"), 0o644)
	keepSkillCurrent(&config.Config{})
	if b, _ := os.ReadFile(claudeSkill); string(b) != string(beamSkill) {
		t.Error("a stale prism copy wasn't updated")
	}
	if b, _ := os.ReadFile(agentsSkill); string(b) != "mine" {
		t.Error("the user's own file was overwritten")
	}
	// After `prism setup skill --remove`, it stays removed.
	off := false
	_ = os.Remove(claudeSkill)
	keepSkillCurrent(&config.Config{SkillAuto: &off})
	if _, err := os.Stat(claudeSkill); err == nil {
		t.Error("reinstalled after the user removed it")
	}
}

func TestRestartIfOutdatedLeavesCurrentOrBusyDaemons(t *testing.T) {
	for _, st := range []beam.Status{
		{Version: version},           // same build
		{},                           // too old to say
		{Version: "v0.0.1", Busy: 2}, // older, but carrying requests
	} {
		if restarted, err := restartIfOutdated(st); restarted || err != nil {
			t.Errorf("restartIfOutdated(%+v) = %v, %v; want left alone", st, restarted, err)
		}
	}
}
