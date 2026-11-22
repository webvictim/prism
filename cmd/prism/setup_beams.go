package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tshwrap"
)

// setupBeams implements `prism setup beams`: the one-time switch to
// recording every session in Beams. It is what an upgraded tbot machine is
// told to run (see config.BeamUnmigrated); a tsh machine is switched over
// on upgrade, and running it there changes nothing but the skill.
//
// It checks before it switches: once beam_mode is required, inference
// stops without a beam, so a user who can't start one — no login, no
// Beams role, a cluster without Beams — must find out here rather than
// from their agent.
func setupBeams(args []string) error {
	fs := flag.NewFlagSet("setup beams", flag.ContinueOnError)
	autoLogin := fs.Bool("auto-login", false,
		"let the daemon run `tsh login` itself when a beam needs a login, holding requests meanwhile "+
			"(for an identity provider that signs you in without a prompt)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// 1. The user's own login: beams belong to a person, so tbot can't
	// start them.
	if _, ok := loginUsable(); !ok {
		if !isTerminal(os.Stdin) {
			return fmt.Errorf("recording in Beams needs your own Teleport login: run `prism login`, then `prism setup beams` again")
		}
		fmt.Fprintln(os.Stderr, "prism: recording in Beams needs your own Teleport login (alongside tbot, if you use it). Logging in…")
		if err := tshLogin(tshwrap.LoginTTLMinutes, nil); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "prism: [1/3] Teleport login %s\n", describeLogin())

	// 2. That login can use Beams on this cluster.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := tshwrap.BeamsList(ctx); err != nil {
		return fmt.Errorf("[2/3] can't use Beams on %s, so nothing was changed: %w\n"+
			"prism: an account needs a Beams role (beam-user) on a cluster with Beams", cmpProxy(cfg), err)
	}
	fmt.Fprintf(os.Stderr, "prism: [2/3] Beams available on %s\n", cmpProxy(cfg))

	// 3. Switch, and teach the agents to link the recordings.
	cfg.BeamMode = config.BeamRequired
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "auto-login" {
			cfg.BeamAutoLogin = *autoLogin
		}
	})
	if err := config.Save(cfg); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "prism: [3/3] beam_mode=required — %s\n", beamModeMeaning(config.BeamRequired))
	if cfg.BeamAutoLogin {
		fmt.Fprintln(os.Stderr, "prism:       beam_auto_login=true — when a login is needed, prism runs it and holds the request")
	}
	if err := setupSkill(nil); err != nil {
		fmt.Fprintf(os.Stderr, "prism: warning: couldn't install the prism-beams skill: %v (`prism setup skill` retries)\n", err)
	}

	fmt.Fprintln(os.Stderr, `
prism: done. A running daemon applies it within a few seconds: no restart.
  - The first request of a session starts a beam (about 20 seconds); it is
    ended, and summarised in Beams, after two hours unused.
  - When your login runs out, prism stops sending inference and says so:
    run `+"`prism login`"+`. `+"`prism claude`"+` and the other launchers log you in first.`)
	if s, _ := state.Load(); (s != nil && s.Identity() == state.IdentitySourceTbot) || cfg.Identity == state.IdentitySourceTbot {
		fmt.Fprintln(os.Stderr, `  - tbot now only carries prism's own checks. The central Beams tenant won't
    offer tbot, so to be ready for it, switch to your login for those too:
    `+"`prism config set identity tsh && prism down && prism up`")
	}
	return nil
}

func cmpProxy(cfg *config.Config) string {
	if cfg != nil && cfg.Proxy != "" {
		return cfg.Proxy
	}
	return "your cluster"
}

// beamMigrationNotice is the reminder an upgraded tbot machine gets until
// it switches to recording in Beams, or "" when it isn't one.
func beamMigrationNotice(cfg *config.Config) string {
	if !cfg.BeamUnmigrated() {
		return ""
	}
	return "prism: sessions on this machine aren't recorded in Teleport Beams, which AI-assisted PRs need.\n" +
		"prism: switch once with `prism setup beams` (it adds your own Teleport login alongside tbot)."
}

// nagExempt are the commands that never print the migration reminder:
// output that is eval'd or parsed, the daemon itself, and the commands
// that report or fix it themselves.
var nagExempt = map[string]bool{
	"env": true, "__daemon": true, "version": true, "--version": true, "-v": true,
	"help": true, "-h": true, "--help": true, "login": true, "config": true,
	"status": true, "doctor": true, "setup": true,
}

// maybeNagBeamMigration prints the reminder before cmd runs, to a person
// only: a program reading a launcher's stderr must not find it there.
func maybeNagBeamMigration(cmd string) {
	if nagExempt[cmd] || !isTerminal(os.Stderr) {
		return
	}
	cfg, err := config.Load()
	if err != nil {
		return
	}
	if msg := beamMigrationNotice(cfg); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
	}
}
