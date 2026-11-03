package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/machineid"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tbot"
	"github.com/webvictim/prism/internal/tshwrap"
)

// `prism tbot reset` exists because a broken Machine ID setup is the one
// prism failure a user cannot talk their way out of: the local keypair,
// the cluster's token, role and bot, and prism's own config all have to
// agree, and when they don't the symptoms (a tunnel that never becomes
// ready, a token whose secret was already redeemed, an identity copied
// from another laptop) read identically from the outside. Debugging that
// remotely means asking someone who has never heard of a bound keypair
// to run tctl by hand.
//
// So this does the whole sequence, in order, and says what it did:
// stop prism, delete this machine's cluster resources, delete the local
// tbot state, recreate all of it, and prove the result works by sending
// a request through the router. Everything is scoped to the local
// hostname — `prism-bot-<host>` and friends — so it can only ever
// reset the machine it runs on.

// resetStepCount is the number of numbered steps the user sees, kept
// here so the progress lines can't drift out of step with each other.
const resetStepCount = 8

func cmdTbotReset(args []string) error {
	names := tbot.LocalNames()

	fs := flag.NewFlagSet("tbot reset", flag.ExitOnError)
	dir := fs.String("dir", "", "tbot working directory (default ~/.config/prism/tbot, or the configured tbot.dir)")
	proxy := fs.String("proxy", "", "Teleport proxy host:port (default from `tsh status`, else the current sidecar)")
	tokenName := fs.String("token-name", names.Token, "name for the join token resource")
	roleName := fs.String("role-name", names.Role, "name for the bot's role resource")
	botName := fs.String("bot-name", names.Bot, "name for the bot resource")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	dryRun := fs.Bool("dry-run", false, "print what would happen and change nothing")
	_ = fs.Parse(args)

	want := tbot.Names{Token: *tokenName, Role: *roleName, Bot: *botName}

	resolvedDir, err := resolveTbotDir(*dir)
	if err != nil {
		return err
	}
	existing, _ := tbot.LoadSidecar(resolvedDir)

	resolvedProxy, proxyFrom, err := resolveProxy(*proxy, existing)
	if err != nil {
		return err
	}
	// Point tsh and tctl at that cluster for the rest of the command,
	// so nothing here depends on which profile the user's shell last
	// logged into. See cmd/prism/tctl.go.
	tshwrap.SetProxy(resolvedProxy)

	// Preflight. Everything that can be checked without changing
	// anything is checked first and reported together: a reset that
	// stops the daemon and then discovers tctl is missing has left the
	// user worse off than when they started. It runs after the proxy is
	// resolved, because "are you logged in" means "to that cluster".
	problems := resetPreflight(resolvedProxy, want, existing)
	if len(problems) > 0 {
		return fmt.Errorf("cannot reset tbot yet:\n%s", strings.Join(problems, "\n"))
	}

	currentMachine, machineErr := machineid.Current()

	fmt.Fprintln(os.Stderr, "prism tbot reset: tctl, tbot and your Teleport login all check out.")
	fmt.Fprint(os.Stderr, resetPlan(resolvedDir, resolvedProxy, proxyFrom, want, existing))
	if *dryRun {
		fmt.Fprintln(os.Stderr, "\nprism tbot reset: --dry-run, nothing was changed.")
		return nil
	}
	if !*yes {
		if err := confirmReset(os.Stdin, os.Stderr, interactive(os.Stdin)); err != nil {
			return err
		}
	}

	step := resetStepper{}

	// 1. Stop prism. tbot holds the storage directory and keeps
	// renewing while it runs, so nothing below is reliable until it is
	// gone.
	step.begin("stopping prism")
	// Remember the router port so the restart in step 7 puts it back
	// where it was: anything already pointed at prism keeps working,
	// and a reset is no moment to move it.
	routerPort := 0
	if s, _ := state.Load(); s == nil {
		step.skip("prism was not running")
	} else {
		routerPort = s.LocalPort
		if err := cmdDown(nil); err != nil {
			return step.fail(err)
		}
		step.done("")
	}

	// 2. Remove this machine's cluster resources. Best effort: a
	// resource that is already gone, or that was never created, is the
	// state we are trying to reach anyway. A permissions problem shows
	// up as a hard failure at step 4 instead, where it is unambiguous.
	step.begin("removing cluster resources for " + names.Host())
	// tctl prints its own ERROR line for a resource that isn't there,
	// which on a first run is all three of them. Say so before it
	// happens rather than leaving someone to read red text as a
	// failure.
	fmt.Fprintln(os.Stderr, "      (an ERROR from tctl here means the resource wasn't there, which is fine)")
	for _, rm := range [][]string{
		{"bots", "rm", want.Bot},
		{"rm", "token/" + want.Token},
		{"rm", "role/" + want.Role},
	} {
		label := strings.Join(rm[len(rm)-1:], "")
		if _, err := runTctl(resolvedProxy, rm...); err != nil {
			fmt.Fprintf(os.Stderr, "      %s: not removed (%v) — carrying on\n", label, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "      %s: removed\n", label)
	}
	step.done("")

	// 3. Remove the local state. Only the files prism itself writes:
	// the directory may be somewhere the user chose, and a stray
	// RemoveAll there would be unforgivable.
	step.begin("clearing local tbot state in " + resolvedDir)
	removed, err := clearLocalTbotState(resolvedDir)
	if err != nil {
		return step.fail(err)
	}
	step.done(strings.Join(removed, ", "))

	// 4. Recreate the cluster resources from prism's own templates.
	step.begin("creating role, bot and token in the cluster")
	paths, err := writeResourceYAML(resolvedDir, want)
	if err != nil {
		return step.fail(err)
	}
	for _, p := range []string{paths.Role, paths.Bot, paths.Token} {
		if out, err := runTctl(resolvedProxy, "create", "-f", p); err != nil {
			return step.fail(fmt.Errorf("tctl create -f %s: %w\n%s", p, err, out))
		}
	}
	step.done("")

	// 5. Read back the registration secret the cluster generated for
	// the new token. Without it tbot cannot join, and it only exists
	// until it is redeemed.
	step.begin("reading the new join secret")
	secret, err := fetchRegistrationSecret(resolvedProxy, want.Token)
	if err != nil {
		return step.fail(err)
	}
	step.done("")

	// 6. Write the local config that points at all of it.
	step.begin("writing local config")
	sidecar := &tbot.SidecarConfig{
		ProxyServer:        resolvedProxy,
		TokenName:          want.Token,
		RoleName:           want.Role,
		BotName:            want.Bot,
		RegistrationSecret: secret,
		MachineID:          currentMachine,
	}
	if err := tbot.SaveSidecar(resolvedDir, sidecar); err != nil {
		return step.fail(err)
	}
	if err := writePlaceholderTbotYAML(resolvedDir, sidecar); err != nil {
		return step.fail(err)
	}
	cfg, err := config.Load()
	if err != nil {
		return step.fail(err)
	}
	cfg.Identity = state.IdentitySourceTbot
	cfg.TbotDir = resolvedDir
	if cfg.Proxy == "" {
		// Without this `prism up` warns that no proxy is configured,
		// and a later switch to tsh mode has nothing to log in to. The
		// address is already in hand, so leaving it unset is just a
		// loose end for the user to trip over.
		cfg.Proxy = resolvedProxy
	}
	if err := config.Save(cfg); err != nil {
		return step.fail(err)
	}
	note := "identity=tbot, proxy=" + cfg.Proxy
	if machineErr != nil {
		note += fmt.Sprintf(" (no machine id recorded: %v)", machineErr)
	}
	step.done(note)

	// 7. Start prism on the new identity.
	step.begin("starting prism")
	var upArgs []string
	if routerPort != 0 {
		upArgs = []string{"--port", strconv.Itoa(routerPort)}
	}
	if err := cmdUp(upArgs); err != nil {
		return step.fail(err)
	}
	step.done("")

	// 8. Prove it works, because "it should work now" is what left the
	// user stuck in the first place. A live request through the router
	// is the only check that covers the whole path: tbot joined, the
	// tunnel is up, the app is reachable and the gateway answers.
	step.begin("checking that inference works")
	s, err := state.Load()
	if err != nil || s == nil {
		return step.fail(fmt.Errorf("prism started but wrote no state file"))
	}
	// Retried, not a single shot: the tunnel comes up seconds after the
	// router does, and a reset that reported failure for being a second
	// early would send the user back round the loop for nothing.
	model, err := waitForInference(s.LocalPort, 90*time.Second)
	if err != nil {
		return step.fail(fmt.Errorf("%w\n\nThe identity was rebuilt, but a request still fails. Run `prism doctor` for the detail", err))
	}
	step.done("the gateway answered as " + model)

	fmt.Fprintf(os.Stderr, `
prism tbot reset: done. This machine now has its own bot:

  bot:    %s
  role:   %s
  token:  %s
  proxy:  %s
  dir:    %s

Nothing else needs doing — prism is running. `+"`prism doctor`"+` re-checks it
at any time.
`, want.Bot, want.Role, want.Token, resolvedProxy, resolvedDir)
	return nil
}

// resetPreflight collects everything that would stop a reset from
// finishing, so the user gets one list instead of discovering them one
// failed step at a time. proxy is the cluster the reset will act on,
// already resolved, because every question here is about that cluster
// rather than about the user's current login context.
func resetPreflight(proxy string, want tbot.Names, existing *tbot.SidecarConfig) []string {
	var problems []string

	if _, err := tctlBin(); err != nil {
		problems = append(problems, "  - tctl is not on PATH. It ships with Teleport: `brew install teleport`.")
	}
	if _, err := tshwrap.LookPathStrict("tbot"); err != nil {
		problems = append(problems, "  - tbot is not on PATH. It ships with Teleport: `brew install teleport`.")
	}

	// Creating cluster resources needs a live login *for this cluster*,
	// with rights to manage bots. tsh is already aimed at it, so this
	// asks about the right profile however the shell is logged in.
	login := "`tsh login --proxy=" + withDefaultPort(proxy) + "`"
	st, err := tshwrap.StatusJSON()
	switch {
	case err != nil:
		problems = append(problems, fmt.Sprintf("  - could not read your Teleport login (%v). Try %s.", err, login))
	case !st.LoggedIn():
		problems = append(problems, fmt.Sprintf("  - you are not logged in to %s. Run %s.", proxy, login))
	case st.IsExpired():
		problems = append(problems, fmt.Sprintf("  - your login to %s expired at %s. Run %s.",
			st.Cluster, st.ValidUntil.Local().Format(time.RFC3339), login))
	}

	// A directory copied from another machine, on a machine that
	// answers to the same hostname, is the one case where resetting
	// would delete resources another computer is still using.
	current, _ := machineid.Current()
	if existing != nil && current != "" && existing.MachineID != "" && existing.MachineID != current &&
		existing.TokenName == want.Token {
		problems = append(problems, fmt.Sprintf(
			"  - this tbot directory came from another machine, and this machine answers to the same hostname (%s),\n"+
				"    so a reset would delete the resources that machine is using. Rename this machine (macOS:\n"+
				"    System Settings → General → Sharing → Local hostname) and try again, or pass explicit\n"+
				"    --bot-name/--role-name/--token-name.", want.Host()))
	}
	return problems
}

// resetPlan is the "here is what I am about to do" block. It is spelled
// out in full because the destructive half of a reset happens in
// someone else's cluster, and they should be able to read it back to
// whoever told them to run this.
func resetPlan(dir, proxy string, from proxySource, want tbot.Names, existing *tbot.SidecarConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
prism tbot reset will rebuild this machine's Machine ID identity from scratch.

In the cluster %s (from %s), which every tsh and tctl call below names
explicitly — your current login context is not consulted:
  delete and recreate  bot/%s
  delete and recreate  role/%s
  delete and recreate  token/%s

On this machine, in %s:
  delete  storage/ (the joined identity), .prism-tbot.json, tbot.yaml,
          role.yaml, bot.yaml, token.yaml
  write   a fresh set, and set identity=tbot

Then it restarts prism and sends one request through it to check.
`, proxy, from, want.Bot, want.Role, want.Token, dir)

	// Changing clusters is a different operation from repairing one,
	// and the only visible difference is this line.
	if existing != nil && existing.ProxyServer != "" && existing.ProxyServer != proxy {
		fmt.Fprintf(&b, `
Note: this machine's tbot currently uses %s. This moves it to %s, and
leaves whatever it has in %s alone — delete those by hand if nothing
else needs them.
`, existing.ProxyServer, proxy, existing.ProxyServer)
	}

	if existing != nil && existing.BotName != "" && existing.BotName != want.Bot {
		fmt.Fprintf(&b, `
Note: this directory currently names bot/%s, which is NOT in the list
above and will be left alone. Only %s is touched.
`, existing.BotName, want.Bot)
	}
	b.WriteString("\nAny other machine's resources are untouched — everything above is scoped to this hostname.\n")
	return b.String()
}

// interactive reports whether stdin is a terminal, i.e. whether there
// is anybody there to answer a prompt.
func interactive(in *os.File) bool {
	fi, err := in.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// confirmReset asks before anything is deleted. A reset is irreversible
// in the cluster, so a non-interactive run must pass --yes rather than
// having the prompt silently answered for it.
func confirmReset(in io.Reader, out io.Writer, interactive bool) error {
	if !interactive {
		return errors.New("refusing to reset without a confirmation: re-run with --yes (or --dry-run to see the plan)")
	}
	fmt.Fprint(out, "Type `reset` to continue: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "reset" {
		return errors.New("not confirmed — nothing was changed")
	}
	return nil
}

// clearLocalTbotState removes the files prism writes into the tbot
// directory, and nothing else. Returns what it actually removed.
func clearLocalTbotState(dir string) ([]string, error) {
	var removed []string
	for _, name := range []string{
		"storage", tbot.SidecarFilename, "tbot.yaml", "role.yaml", "bot.yaml", "token.yaml",
	} {
		p := filepath.Join(dir, name)
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return removed, fmt.Errorf("remove %s: %w", p, err)
		}
		removed = append(removed, name)
	}
	if err := os.MkdirAll(tbot.StorageDirPath(dir), 0o700); err != nil {
		return removed, fmt.Errorf("recreate storage dir: %w", err)
	}
	if len(removed) == 0 {
		removed = append(removed, "nothing to remove")
	}
	return removed, nil
}

// resetStepper prints the numbered progress lines.
type resetStepper struct {
	n int
}

func (s *resetStepper) begin(what string) {
	s.n++
	fmt.Fprintf(os.Stderr, "\n[%d/%d] %s…\n", s.n, resetStepCount, what)
}

func (s *resetStepper) done(detail string) {
	if detail == "" {
		fmt.Fprintln(os.Stderr, "      ok")
		return
	}
	fmt.Fprintf(os.Stderr, "      ok — %s\n", detail)
}

func (s *resetStepper) skip(why string) {
	fmt.Fprintf(os.Stderr, "      skipped — %s\n", why)
}

// fail annotates an error with the step it happened in, so a pasted
// transcript says how far the reset got.
func (s *resetStepper) fail(err error) error {
	return fmt.Errorf("step %d/%d failed: %w", s.n, resetStepCount, err)
}
