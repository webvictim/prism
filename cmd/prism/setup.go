package main

import (
	"fmt"
	"os"
)

// `prism setup <tool>` writes prism into a tool's own configuration, for
// the tools that ignore the base-URL environment variables. It is a verb
// of prism's own rather than `prism <tool> config` because everything
// after `prism <tool>` belongs to the tool: Pi has a `pi config` of its
// own, which `prism pi config` hid for several releases, and any tool
// could add one.
var setupTools = []struct {
	name, summary string
	run           func([]string) error
}{
	{"opencode", "add a \"via prism\" model to OpenCode's config for every model the cluster serves", setupOpencode},
	{"pi", "the same for Pi, plus routing Pi's own models through prism", setupPi},
}

func cmdSetup(args []string) error {
	if len(args) == 0 || wantsHelp(args[:1]) {
		printSetupHelp()
		if len(args) == 0 {
			return fmt.Errorf("setup: name a tool")
		}
		return nil
	}
	for _, t := range setupTools {
		if t.name == args[0] {
			return t.run(args[1:])
		}
	}
	printSetupHelp()
	return fmt.Errorf("setup: unknown tool %q", args[0])
}

func printSetupHelp() {
	fmt.Fprintln(os.Stderr, "prism setup — write prism into a tool's own configuration\n\nUsage:")
	for _, t := range setupTools {
		fmt.Fprintf(os.Stderr, "  prism setup %-9s # %s\n", t.name, t.summary)
	}
	fmt.Fprintln(os.Stderr, "\n`prism setup <tool> --help` lists a tool's flags. `prism opencode` and\n`prism pi` refresh the same entries on every launch.")
}
