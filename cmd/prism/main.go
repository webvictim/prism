// prism: route local AI traffic through Teleport cluster apps.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/tshwrap"
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
	// Aim every tsh call prism makes at the configured cluster, rather
	// than at whichever profile the user's shell last logged into. The
	// env var above only applies when the user hasn't set one, so on
	// its own it lets a stray TELEPORT_PROXY decide where prism looks.
	tshwrap.SetProxy(cfg.Proxy)
}

func usage() { writeUsage(os.Stderr) }

func writeUsage(w io.Writer) {
	fmt.Fprintf(w, `prism %s — route local AI traffic through Teleport cluster apps

Usage:
  prism claude [args...]        # up + exec claude with prism env
  prism codex [args...]         # up + exec codex with prism env
  prism opencode [args...]      # up + add "via prism" models + exec opencode
  prism pi [args...]            # up + configure Pi's models + exec pi
  prism exec <cmd> [args...]    # up + exec arbitrary command with prism env
  prism setup opencode [--overwrite]
                                  # add "via prism" models to OpenCode's config
  prism setup pi [--overwrite] [--anthropic-model <m>] [--openai-model <m>]
                                  # configure Pi's models without launching it
  prism up [--proxy <addr>] [--port <n>] [--tsh]
  prism down
  prism status [--json]
  prism env
  prism logs
  prism test [anthropic|openai|all] [--model <m>] [--format <f>] [--stream]
                                  # exercise the local router end-to-end
  prism models [--cached] [--json]
                                  # the models the cluster serves, and the probe model
  prism usage [--week|--all|--json]
  prism doctor [--no-request] [--model <m>]
                                  # check everything and say what is wrong
  prism config [show|set <k> <v>|unset <k>|clear]
  prism tbot [bootstrap|configure|status|reset]
  prism install                   # install as system service (Linux/macOS)
  prism uninstall                 # remove system service
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
	tshwrap.SetStdio(childStdio())
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "claude":
		err = cmdClaude(args)
	case "codex":
		err = cmdCodex(args)
	case "opencode":
		err = cmdOpencode(args)
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
	case "usage":
		err = cmdUsage(args)
	case "models":
		err = cmdModels(args)
	case "doctor":
		err = cmdDoctor(args)
	case "tbot":
		err = cmdTbot(args)
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "pi":
		err = cmdPi(args)
	case "setup":
		err = cmdSetup(args)
	case "version", "--version", "-v":
		fmt.Println("prism", version)
	case daemonArg:
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
