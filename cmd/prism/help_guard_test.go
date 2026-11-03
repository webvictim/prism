package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWantsHelp(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"help"}, {"--dir", "x", "--help"}} {
		if !wantsHelp(args) {
			t.Errorf("wantsHelp(%v) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {}, {"--dir", "x"}, {"--helpful"}, {"nohelp"}} {
		if wantsHelp(args) {
			t.Errorf("wantsHelp(%v) = true, want false", args)
		}
	}
}

// Every command that guards on wantsHelp needs text to print, or the
// guard silently dumps the whole usage instead.
func TestCommandHelpCoversGuardedCommands(t *testing.T) {
	for _, name := range []string{"down", "install", "uninstall"} {
		text, ok := commandHelp[name]
		if !ok {
			t.Errorf("no help text for %q", name)
			continue
		}
		if len(text) < 40 {
			t.Errorf("help for %q is too thin to be useful: %q", name, text)
		}
	}
}

// The bug this guards: `prism install --help` used to install, and
// `prism uninstall --help` used to uninstall. Asking for help must
// leave the machine alone.
//
// install is the safe one to exercise — on macOS it only writes a
// file, where uninstall would talk to the real launchd and could stop
// the daemon running this test.
func TestInstallHelpWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := cmdInstall([]string{"--help"}); err != nil {
		t.Fatalf("cmdInstall --help = %v, want it to print help and return", err)
	}

	for _, definition := range []string{
		filepath.Join(home, "Library", "LaunchAgents", "com.prism.daemon.plist"),
		filepath.Join(home, ".config", "systemd", "user", "prism.service"),
	} {
		if _, err := os.Stat(definition); err == nil {
			t.Errorf("asking for help installed %s", definition)
		}
	}
}
