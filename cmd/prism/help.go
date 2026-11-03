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
