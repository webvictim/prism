package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

	// Preflight. Everything that can be checked without changing
	// anything is checked first and reported together: a reset that
	// stops the daemon and then discovers tctl is missing has left the
	// user worse off than when they started.
	problems, err := resetPreflight(resolvedDir, want, existing)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return fmt.Errorf("cannot reset tbot yet:\n%s", strings.Join(problems, "\n"))
	}

	resolvedProxy, err := resolveProxy(*proxy, existing)
	if err != nil {
		return err
	}

	currentMachine, machineErr := machineid.Current()

	fmt.Fprintln(os.Stderr, "prism tbot reset: tctl, tbot and your Teleport login all check out.")
	fmt.Fprint(os.Stderr, resetPlan(resolvedDir, resolvedProxy, want, existing))
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
	if s, _ := state.Load(); s == nil {
		step.skip("prism was not running")
	} else if err := cmdDown(nil); err != nil {
		return step.fail(err)
	} else {
		step.done("")
	}

	// 2. Remove this machine's cluster resources. Best effort: a
	// resource that is already gone, or that was never created, is the
	// state we are trying to reach anyway. A permissions problem shows
	// up as a hard failure at step 4 instead, where it is unambiguous.
	step.begin("removing cluster resources for " + names.Host())
	for _, rm := range [][]string{
		{"bots", "rm", want.Bot},
		{"rm", "token/" + want.Token},
		{"rm", "role/" + want.Role},
	} {
		label := strings.Join(rm[len(rm)-1:], "")
		if _, err := runTctl(rm...); err != nil {
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
		if out, err := runTctl("create", "-f", p); err != nil {
			return step.fail(fmt.Errorf("tctl create -f %s: %w\n%s", p, err, out))
		}
	}
	step.done("")

	// 5. Read back the registration secret the cluster generated for
	// the new token. Without it tbot cannot join, and it only exists
	// until it is redeemed.
	step.begin("reading the new join secret")
	secret, err := fetchRegistrationSecret(want.Token)
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
	if err := config.Save(cfg); err != nil {
		return step.fail(err)
	}
	note := "identity=tbot"
	if machineErr != nil {
		note += fmt.Sprintf(" (no machine id recorded: %v)", machineErr)
	}
	step.done(note)

	// 7. Start prism on the new identity.
	step.begin("starting prism")
	if err := cmdUp(nil); err != nil {
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
	model, err := probeInference(s.LocalPort, 90*time.Second)
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
// failed step at a time. The error return is for problems reading the
// machine's own state, not for a failed check.
func resetPreflight(dir string, want tbot.Names, existing *tbot.SidecarConfig) ([]string, error) {
	var problems []string

	if _, err := tshwrap.LookPathStrict("tctl"); err != nil {
		problems = append(problems, "  - tctl is not on PATH. It ships with Teleport: `brew install teleport`.")
	}
	if _, err := tshwrap.LookPathStrict("tbot"); err != nil {
		problems = append(problems, "  - tbot is not on PATH. It ships with Teleport: `brew install teleport`.")
	}

	// Creating cluster resources needs a live login with admin rights.
	// Checking the login here turns the most common failure — an
	// expired session — into one line instead of a tctl stack trace.
	st, err := tshwrap.StatusJSON()
	switch {
	case err != nil:
		hint := "tsh login"
		if existing != nil && existing.ProxyServer != "" {
			hint += " --proxy=" + existing.ProxyServer
		}
		problems = append(problems, fmt.Sprintf("  - you are not logged in to Teleport (`%s`): %v", hint, err))
	case st.IsExpired():
		problems = append(problems, fmt.Sprintf("  - your Teleport login expired at %s — run `tsh login --proxy=%s`",
			st.ValidUntil.Local().Format(time.RFC3339), proxyFromStatus(st, existing)))
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
	return problems, nil
}

func proxyFromStatus(st *tshwrap.Status, existing *tbot.SidecarConfig) string {
	if st != nil && st.Cluster != "" {
		return st.Cluster
	}
	if existing != nil && existing.ProxyServer != "" {
		return existing.ProxyServer
	}
	return "<proxy>"
}

// resetPlan is the "here is what I am about to do" block. It is spelled
// out in full because the destructive half of a reset happens in
// someone else's cluster, and they should be able to read it back to
// whoever told them to run this.
func resetPlan(dir, proxy string, want tbot.Names, existing *tbot.SidecarConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, `
prism tbot reset will rebuild this machine's Machine ID identity from scratch.

In the cluster %s:
  delete and recreate  bot/%s
  delete and recreate  role/%s
  delete and recreate  token/%s

On this machine, in %s:
  delete  storage/ (the joined identity), .prism-tbot.json, tbot.yaml,
          role.yaml, bot.yaml, token.yaml
  write   a fresh set, and set identity=tbot

Then it restarts prism and sends one request through it to check.
`, proxy, want.Bot, want.Role, want.Token, dir)

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

// runTctl runs tctl with the user's terminal attached so a per-session
// MFA prompt can be answered rather than hanging, while still returning
// the output for a message of prism's own.
func runTctl(args ...string) (string, error) {
	bin, err := tshwrap.LookPathStrict("tctl")
	if err != nil {
		return "", errors.New("tctl not on PATH")
	}
	cmd := exec.Command(bin, args...)
	var buf bytes.Buffer
	cmd.Stdin = os.Stdin
	cmd.Stdout = &buf
	// Tee'd rather than captured: an MFA prompt is written here, and a
	// prompt nobody can see is a hang.
	cmd.Stderr = io.MultiWriter(&buf, os.Stderr)
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(buf.String()), err
	}
	return strings.TrimSpace(buf.String()), nil
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
