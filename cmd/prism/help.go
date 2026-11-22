package main

import (
	"fmt"
	"os"
)

// Commands with a flag set of their own get `--help` handling for free
// from the flag package, before they do anything. The ones below take
// no flags and used to ignore their arguments entirely — so
// `prism down --help` stopped the daemon, and `prism uninstall --help`
// removed the service and told you afterwards. A help flag must never
// be the thing that changes the machine.
var commandHelp = map[string]string{
	"login": `prism login [--ttl <minutes>] — log in to Teleport for Beams

Runs ` + "`tsh login`" + ` to prism's cluster, asking for 30 hours; your roles may
cap it (Teleport shortens a longer request rather than refusing it).
Beams belong to a person, so recording sessions in Beams needs this login
even when tbot carries prism's tunnels.`,

	"beam": `prism beam — record agent sessions in Teleport Beams

  prism beam status [--json]   the beams running, and your Teleport login
  prism beam link [--pr] [--since 24h] [--all]
                               this repository's beams, or a PR section
  prism beam end [--all]       end this repository's beams (all with --all)
  prism beam test              start a beam of its own and prove inference
                               works through it, then remove it
  prism beam start             warm a beam for the next session (with
                               beam_mode off: start the one beam)

prism sends inference only through a beam (beam_mode required, the
default), so every agent session is recorded, summarised and classified in
Beams as if it had run there. The agent, the code and the tools stay on
this machine; only the LLM traffic passes through the beam.

Each session gets a beam of its own when it first sends a request, ended
after two hours unused; at most beam_max (10) run at once, and at the limit
sessions from the same repository share. A beam lives at most 11 hours and
is replaced before it expires. Its summary appears in Beams about 15
minutes after it ends.

Beams belong to a person, so this needs your own Teleport login, even
when prism runs on tbot: ` + "`prism login`" + `. On a tbot machine that hasn't
switched yet, ` + "`prism setup beams`" + ` does it once.`,

	"down": `prism down — stop the daemon and clear the session

Stops the service if prism is service-managed, otherwise signals the
daemon, logs out of the cluster apps and removes state.json. Takes no
flags. Start again with ` + "`prism up`" + `.`,

	"install": `prism install — run prism as a system service

macOS: writes ~/Library/LaunchAgents/com.prism.daemon.plist, which
starts prism at login and restarts it if it crashes.
Linux: writes ~/.config/systemd/user/prism.service, enables it, and
turns on lingering so it survives logout.

Takes no flags. Re-running it rewrites the definition for the prism
binary you ran it with, which is also how a stale one is repaired.`,

	"uninstall": `prism uninstall — remove the system service

Stops the service and deletes its definition. prism keeps working;
` + "`prism up`" + ` just starts the daemon directly from then on.
Takes no flags.`,
}

// wantsHelp reports whether args ask for help rather than action.
func wantsHelp(args []string) bool {
	for _, a := range args {
		switch a {
		case "-h", "--help", "help":
			return true
		}
	}
	return false
}

// printCommandHelp writes the help for one command, or falls back to
// the whole usage text if a command was added without any.
func printCommandHelp(name string) error {
	text, ok := commandHelp[name]
	if !ok {
		usage()
		return nil
	}
	fmt.Fprintln(os.Stderr, text)
	return nil
}
