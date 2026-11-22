package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/webvictim/prism/internal/config"
)

// beamSkill is the prism-beams skill: how an agent gets its session
// recorded in Beams and puts the recording in a pull request.
//
//go:embed skill/prism-beams/SKILL.md
var beamSkill []byte

const (
	skillName = "prism-beams"
	// skillMarker identifies a file prism wrote. Only files carrying it
	// are overwritten or removed, so a user's own edits survive once they
	// delete the marker line.
	skillMarker = "prism-managed:"
)

// skillDirs are the user-level skill directories that, between them, every
// harness prism launches reads — each checked against the installed tool:
//
//	~/.claude/skills  Claude Code; OpenCode also reads it
//	~/.agents/skills  Codex and Pi
//
// None of the four reads both, so the skill is never listed twice.
func skillDirs() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	claude := filepath.Join(home, ".claude")
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		if claude, err = expandHome(d); err != nil {
			return nil, err
		}
	}
	return []string{
		filepath.Join(claude, "skills"),
		filepath.Join(home, ".agents", "skills"),
	}, nil
}

func setupSkill(args []string) error {
	fset := flag.NewFlagSet("setup skill", flag.ContinueOnError)
	remove := fset.Bool("remove", false, "remove the skill instead of installing it")
	if err := fset.Parse(args); err != nil {
		return err
	}
	dirs, err := skillDirs()
	if err != nil {
		return err
	}
	for _, d := range dirs {
		path := filepath.Join(d, skillName, "SKILL.md")
		var msg string
		if *remove {
			msg, err = removeSkill(path)
		} else {
			msg, err = installSkill(path, beamSkill)
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "prism: %s: %s\n", path, msg)
	}
	// Remember the choice, so the launchers' automatic install doesn't
	// undo a removal (and a reinstall turns it back on).
	if cfg, err := config.Load(); err == nil {
		if *remove {
			off := false
			cfg.SkillAuto = &off
		} else {
			cfg.SkillAuto = nil
		}
		if err := config.Save(cfg); err != nil {
			return err
		}
	}
	if *remove {
		fmt.Fprintln(os.Stderr, "prism: the launchers won't reinstall it; `prism setup skill` brings it back.")
	} else {
		fmt.Fprintln(os.Stderr, "prism: Claude Code, Codex, OpenCode and Pi load it in their next session.")
	}
	return nil
}

// keepSkillCurrent is the launchers' automatic `prism setup skill`: while
// sessions are recorded in Beams, it installs the skill where it's missing
// and updates copies prism wrote, so agents know how to link recordings in
// a PR without anyone running a setup command — and the skill follows
// prism across upgrades. It leaves files without the marker alone, never
// fails a launch, and says something only when it changed a file, and
// only to a person.
func keepSkillCurrent(cfg *config.Config) {
	if !cfg.SkillAutoEnabled() || cfg.Beam() == config.BeamOff {
		return
	}
	dirs, err := skillDirs()
	if err != nil {
		return
	}
	var changed []string
	for _, d := range dirs {
		msg, err := installSkill(filepath.Join(d, skillName, "SKILL.md"), beamSkill)
		if err == nil && (msg == "installed" || msg == "updated") {
			changed = append(changed, msg)
		}
	}
	if len(changed) > 0 && isTerminal(os.Stderr) {
		fmt.Fprintf(os.Stderr, "prism: %s the prism-beams skill, which shows agents how to link Beams recordings in PRs "+
			"(`prism setup skill --remove` removes it)\n", changed[0])
	}
}

// installSkill writes content to path unless a file is there that prism
// didn't write.
func installSkill(path string, content []byte) (string, error) {
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", err
	case !bytes.Contains(old, []byte(skillMarker)):
		return "left alone — it has no " + skillMarker + " marker, so it's yours", nil
	case bytes.Equal(old, content):
		return "already up to date", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return "", err
	}
	if old == nil {
		return "installed", nil
	}
	return "updated", nil
}

// removeSkill deletes a skill prism wrote, and its directory if empty.
func removeSkill(path string) (string, error) {
	old, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "not installed", nil
	}
	if err != nil {
		return "", err
	}
	if !bytes.Contains(old, []byte(skillMarker)) {
		return "left alone — it has no " + skillMarker + " marker, so it's yours", nil
	}
	if err := os.Remove(path); err != nil {
		return "", err
	}
	_ = os.Remove(filepath.Dir(path)) // only succeeds when empty
	return "removed", nil
}

// skillInstalled reports whether every skill directory has the skill, for
// prism doctor.
func skillInstalled() (missing []string) {
	dirs, err := skillDirs()
	if err != nil {
		return nil
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, skillName, "SKILL.md")); err != nil {
			missing = append(missing, strings.TrimSuffix(d, string(filepath.Separator)))
		}
	}
	return missing
}
