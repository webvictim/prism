package main

import (
	"fmt"
	"strings"

	"github.com/webvictim/prism/internal/egress"
)

// `prism --no-beam <tool> …` and `prism --beam <tool> …` override
// beam_mode for one launch. --no-beam is for a quick request that
// shouldn't wait for a beam to start and needn't be recorded: it goes
// straight to the tunnels. --beam is the opposite, for a launch that must
// be recorded when beam_mode is off or preferred: it goes only through a
// beam, refused when none can be had, exactly as in required mode.
//
// They come before the tool because everything after `prism <tool>`
// belongs to the tool. They work by a header on every request
// (egress.BeamHeader), so only launchers that can attach one take them:
// Claude Code (ANTHROPIC_CUSTOM_HEADERS) and Codex (its provider's
// http_headers). OpenCode and Pi read their providers from config files,
// and `prism exec` runs an arbitrary program, so for those the flag is
// refused rather than quietly ignored.
//
// Like beam_mode off, --no-beam is documented for power users and never
// suggested by a refusal, a fix line or the skill.

// beamOverride is the launch's override: "", egress.BeamOff or
// egress.BeamRequired.
var beamOverride string

// beamOverrideTools are the launchers that can carry the override.
var beamOverrideTools = []string{"claude", "codex"}

// takeBeamOverride reads the flags in front of the command.
func takeBeamOverride(args []string) (override string, rest []string, err error) {
	for len(args) > 0 {
		var v string
		switch args[0] {
		case "--no-beam", "-no-beam":
			v = egress.BeamOff
		case "--beam", "-beam":
			v = egress.BeamRequired
		default:
			return override, args, nil
		}
		if override != "" && override != v {
			return "", nil, fmt.Errorf("--beam and --no-beam contradict each other; give one")
		}
		override, args = v, args[1:]
	}
	return override, args, nil
}

// checkBeamOverride refuses the override for a command that can't carry it.
func checkBeamOverride(override, command string) error {
	if override == "" {
		return nil
	}
	for _, t := range beamOverrideTools {
		if command == t {
			return nil
		}
	}
	flag := "--no-beam"
	if override == egress.BeamRequired {
		flag = "--beam"
	}
	return fmt.Errorf("%s works only with `prism %s`: it rides on a header prism adds to that tool's requests, "+
		"and `prism %s` can't add one", flag, strings.Join(beamOverrideTools, "` and `prism "), command)
}

// claudeBeamHeaderLine is the ANTHROPIC_CUSTOM_HEADERS line for the
// override, or "".
func beamHeaderLine(override string) string {
	if override == "" {
		return ""
	}
	return egress.BeamHeader + ": " + override
}
