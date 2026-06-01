// prism: route local AI traffic through Teleport cluster apps.
package main

import (
	"fmt"
	"os"

	"github.com/gravitational/prism/internal/config"
)

var version = "dev"

func applyConfigEnv() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "prism: warning: read config: %v\n", err)
		return
	}
	if cfg.Proxy != "" && os.Getenv("TELEPORT_PROXY") == "" {
		_ = os.Setenv("TELEPORT_PROXY", cfg.Proxy)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `prism %s — route local AI traffic through Teleport cluster apps

Usage:
  prism claude [args...]        # up + exec claude with prism env
  prism codex [args...]         # up + exec codex with prism env
  prism exec <cmd> [args...]    # up + exec arbitrary command with prism env
  prism up [--proxy <addr>] [--port <n>] [--tsh]
  prism down
  prism status
  prism env
  prism logs
  prism test [anthropic|openai]   # exercise the local router end-to-end
  prism config [show|set <k> <v>|unset <k>|clear]
  prism tbot [bootstrap|configure|status]
  prism version

  (internal: prism __daemon)
`, version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	applyConfigEnv()
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "claude":
		err = cmdClaude(args)
	case "codex":
		err = cmdCodex(args)
	case "exec":
		err = cmdExec(args)
	case "up":
		err = cmdUp(args)
	case "down":
		err = cmdDown(args)
	case "status":
		err = cmdStatus(args)
	case "env":
		err = cmdEnv(args)
	case "logs":
		err = cmdLogs(args)
	case "config":
		err = cmdConfig(args)
	case "test":
		err = cmdTest(args)
	case "tbot":
		err = cmdTbot(args)
	case "version", "--version", "-v":
		fmt.Println("prism", version)
	case "__daemon":
		err = cmdDaemon(args)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "prism: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "prism: %v\n", err)
		os.Exit(1)
	}
}
