package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/machineid"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tbot"
	"github.com/webvictim/prism/internal/tshwrap"
)

// cmdTbot dispatches `prism tbot ...` subcommands.
func cmdTbot(args []string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("prism tbot is not supported on Windows in this iteration — use `prism up --tsh` (the default) instead")
	}
	if len(args) == 0 {
		return tbotUsage()
	}
	switch args[0] {
	case "bootstrap":
		return cmdTbotBootstrap(args[1:])
	case "configure":
		return cmdTbotConfigure(args[1:])
	case "status":
		return cmdTbotStatus(args[1:])
	case "reset":
		return cmdTbotReset(args[1:])
	case "help", "--help", "-h":
		return tbotUsage()
	default:
		return fmt.Errorf("unknown `prism tbot` subcommand %q", args[0])
	}
}

func tbotUsage() error {
	fmt.Fprint(os.Stderr, `Usage:
  prism tbot bootstrap [--dir DIR] [--proxy HOST:PORT] [--token-name N] [--role-name N] [--bot-name N]
      Generate role/token/bot/tbot.yaml templates for a Machine ID-based
      identity. Defaults read from `+"`tsh status`"+`. Prints the
      `+"`tctl create -f`"+` commands you need to run next.

  prism tbot configure --registration-secret SECRET [--dir DIR]
      Persists the registration secret (from `+"`tctl get token/...`"+`)
      to the tbot dir's prism sidecar and writes the initial tbot.yaml.

  prism tbot status [--dir DIR]
      Reports validation state, configured proxy, and the apps
      targeted by tbot.yaml's application-tunnel services.

  prism tbot reset [--yes] [--dry-run] [--dir DIR] [--proxy HOST:PORT]
      Rebuild this machine's Machine ID identity from scratch: delete and
      recreate its bot, role and token in the cluster, clear the local
      tbot state, restart prism and check that inference works. Scoped to
      this hostname, so no other machine is affected. Needs tctl and a
      live `+"`tsh login`"+`.

After bootstrap + configure:
  prism config set identity tbot
  prism up
  prism install          (Linux: optional systemd user service)
`)
	return nil
}

// --- bootstrap ---

func cmdTbotBootstrap(args []string) error {
	names := tbot.LocalNames()

	fs := flag.NewFlagSet("tbot bootstrap", flag.ExitOnError)
	dir := fs.String("dir", "", "tbot working directory (default ~/.config/prism/tbot)")
	proxy := fs.String("proxy", "", "Teleport proxy host:port (default from `tsh status`)")
	tokenName := fs.String("token-name", names.Token, "name for the join token resource")
	roleName := fs.String("role-name", names.Role, "name for the bot's role resource")
	botName := fs.String("bot-name", names.Bot, "name for the bot resource")
	_ = fs.Parse(args)

	// Which names the user actually chose. A flag's value can't tell
	// you — a default and an explicit flag carrying the same string are
	// identical by then — and the difference decides whether an
	// existing sidecar's names win below.
	chosen := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { chosen[f.Name] = true })

	resolvedDir, err := resolveTbotDir(*dir)
	if err != nil {
		return err
	}

	// Detect existing sidecar — if present, preserve its registration
	// secret and only update the config files (role.yaml gains openai
	// access, tbot.yaml gets the multi-service format).
	existingSidecar, _ := tbot.LoadSidecar(resolvedDir)

	resolvedProxy, proxyFrom, err := resolveProxy(*proxy, existingSidecar)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "prism tbot bootstrap: cluster %s (from %s)\n", resolvedProxy, proxyFrom)
	// Every tsh and tctl call below names that cluster explicitly, so
	// the user's active login context doesn't decide where resources
	// land. See cmd/prism/tctl.go.
	tshwrap.SetProxy(resolvedProxy)

	if err := os.MkdirAll(filepath.Join(resolvedDir, "storage"), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", resolvedDir, err)
	}

	isUpdate := existingSidecar != nil && existingSidecar.RegistrationSecret != ""

	// Three ways an existing identity can disagree with this machine,
	// and they want opposite advice — the reason this used to be one
	// hostname-substring test that printed `tctl rm` either way:
	//
	//   - cloned: another computer is still using these resources, so
	//     removing them would break *that* machine;
	//   - renamed: the same computer under a new name, so the old
	//     resources are this user's to remove;
	//   - neither: an ordinary in-place update.
	currentMachine, _ := machineid.Current()
	cloned := isUpdate && currentMachine != "" &&
		existingSidecar.MachineID != "" && existingSidecar.MachineID != currentMachine
	renamed := isUpdate && !cloned && existingSidecar.TokenName != "" &&
		existingSidecar.TokenName != *tokenName

	if cloned {
		if existingSidecar.TokenName == *tokenName {
			return fmt.Errorf(`this tbot identity came from a different machine, and this machine answers to the same hostname (%s), so prism cannot tell the two apart in the cluster.

Rename this machine first (macOS: System Settings → General → Sharing → Local hostname), then re-run `+"`prism tbot reset`"+`. To keep the name, pass explicit names:

  prism tbot reset --bot-name prism-bot-<something> --role-name prism-bot-role-<something> --token-name prism-bot-token-<something>`,
				names.Host())
		}
		fmt.Fprintf(os.Stderr, `prism tbot bootstrap: this tbot directory was copied from another machine.

Giving this machine its own bot:
  token: %s → %s
  role:  %s → %s
  bot:   %s → %s

Leave the old resources alone — the machine they belong to is still
using them, and removing them would break prism there.

`, existingSidecar.TokenName, *tokenName,
			existingSidecar.RoleName, *roleName,
			existingSidecar.BotName, *botName)
	} else if renamed {
		fmt.Fprintf(os.Stderr, `prism tbot bootstrap: this machine's hostname has changed, so it needs new cluster resources:
  token: %s → %s
  role:  %s → %s
  bot:   %s → %s

The old ones belong to this same machine and can be removed once the
new ones work — but only if no other machine was set up under the old
name:

  tctl bots rm %s
  tctl rm token/%s
  tctl rm role/%s

`, existingSidecar.TokenName, *tokenName,
			existingSidecar.RoleName, *roleName,
			existingSidecar.BotName, *botName,
			existingSidecar.BotName, existingSidecar.TokenName, existingSidecar.RoleName)
	}

	if cloned || renamed {
		// New names mean a new token, and tbot's stored keypair is tied
		// to the old one. Wipe storage so it joins afresh.
		storagePath := tbot.StorageDirPath(resolvedDir)
		_ = os.RemoveAll(storagePath)
		_ = os.MkdirAll(storagePath, 0o700)
		isUpdate = false
	}

	if isUpdate {
		fmt.Fprintln(os.Stderr, "prism tbot bootstrap: existing sidecar detected, updating in-place (preserving registration secret)")
		// Keep the names already in the cluster unless the user asked
		// for different ones.
		if !chosen["token-name"] && existingSidecar.TokenName != "" {
			*tokenName = existingSidecar.TokenName
		}
		if !chosen["role-name"] && existingSidecar.RoleName != "" {
			*roleName = existingSidecar.RoleName
		}
		if !chosen["bot-name"] && existingSidecar.BotName != "" {
			*botName = existingSidecar.BotName
		}
	}

	paths, err := writeResourceYAML(resolvedDir, tbot.Names{Token: *tokenName, Role: *roleName, Bot: *botName})
	if err != nil {
		return err
	}
	rolePath, botPath, tokenPath := paths.Role, paths.Bot, paths.Token

	// Preserve existing secret, update everything else.
	sidecar := &tbot.SidecarConfig{
		ProxyServer: resolvedProxy,
		TokenName:   *tokenName,
		RoleName:    *roleName,
		BotName:     *botName,
		MachineID:   currentMachine,
	}
	if isUpdate {
		sidecar.RegistrationSecret = existingSidecar.RegistrationSecret
	}
	if err := tbot.SaveSidecar(resolvedDir, sidecar); err != nil {
		return fmt.Errorf("save sidecar: %w", err)
	}

	// Persist tbot.dir in config so `prism up` uses it automatically.
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if cfg.TbotDir != resolvedDir {
		cfg.TbotDir = resolvedDir
		if err := config.Save(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
	}

	if isUpdate {
		// Write the updated tbot.yaml with the new multi-service format.
		tbotYAML := tbot.RenderTbotYAML(tbot.TbotYAMLOpts{
			TokenName:          sidecar.TokenName,
			RegistrationSecret: sidecar.RegistrationSecret,
			StoragePath:        tbot.StorageDirPath(resolvedDir),
			Services: []tbot.AppTunnelService{
				{Name: tbot.AppTunnelServiceName, AppName: "anthropic", Port: 7334},
				{Name: tbot.OpenAITunnelServiceName, AppName: "openai", Port: 7335},
			},
			ProxyServer: sidecar.ProxyServer,
		})
		if err := os.WriteFile(tbot.TbotYAMLPath(resolvedDir), tbotYAML, 0o600); err != nil {
			return fmt.Errorf("write tbot.yaml: %w", err)
		}

		fmt.Fprintf(os.Stderr, `prism tbot bootstrap: updated config in %s

The role.yaml has been updated to include openai app access.
Re-apply it to the cluster:

  tctl create -f %s

Then restart prism:

  prism down && prism up

(Registration secret and bot/token resources are unchanged.)

`, resolvedDir, rolePath)
	} else {
		fmt.Fprintf(os.Stderr, `prism tbot bootstrap: wrote bootstrap files to %s

Next steps (one-time, requires a tsh login with tctl admin perms):

  tsh login --proxy=%s
  tctl create -f %s
  tctl create -f %s
  tctl create -f %s
  prism tbot configure --dir %s
  prism config set identity tbot
  prism up

On Linux, optionally install as a systemd user service for persistence:

  prism install

(`+"`prism tbot configure`"+` reads the bound-keypair registration secret
from the cluster via tctl. If you'd rather pass it explicitly, append
`+"`--registration-secret <value>`"+`.)

`, resolvedDir, resolvedProxy, rolePath, botPath, tokenPath, resolvedDir)
	}
	return nil
}

// --- configure ---

func cmdTbotConfigure(args []string) error {
	fs := flag.NewFlagSet("tbot configure", flag.ExitOnError)
	dir := fs.String("dir", "", "tbot working directory (default ~/.config/prism/tbot, or the value used at bootstrap)")
	secret := fs.String("registration-secret", "", "the bound-keypair registration secret; if omitted, prism shells out to tctl to fetch it")
	_ = fs.Parse(args)

	resolvedDir, err := resolveTbotDir(*dir)
	if err != nil {
		return err
	}

	sidecar, err := tbot.LoadSidecar(resolvedDir)
	if err != nil {
		return fmt.Errorf("%w (run `prism tbot bootstrap --dir %s` first)", err, resolvedDir)
	}

	// Auto-fetch from the cluster when the user didn't pass an explicit
	// secret. Faster, no jq needed, and removes the inline-$()-expansion
	// risk of silently setting an empty secret if tctl fails.
	resolvedSecret := *secret
	if resolvedSecret == "" {
		if sidecar.TokenName == "" {
			return fmt.Errorf("cannot auto-fetch registration secret: sidecar has no token_name (re-run `prism tbot bootstrap` or pass --registration-secret explicitly)")
		}
		fmt.Fprintf(os.Stderr, "prism tbot configure: fetching registration secret from cluster via `tctl get token/%s`…\n", sidecar.TokenName)
		fetched, err := fetchRegistrationSecret(sidecar.ProxyServer, sidecar.TokenName)
		if err != nil {
			return fmt.Errorf("fetch registration secret: %w (or pass --registration-secret explicitly)", err)
		}
		resolvedSecret = fetched
	}
	if resolvedSecret == "" || resolvedSecret == "null" {
		return fmt.Errorf("refusing to configure with an empty or null registration secret")
	}

	// If the user is rotating the registration secret (token recreated,
	// rebootstrap, etc.), tbot's on-disk bound-keypair state in
	// <dir>/storage is tied to the OLD secret and tbot will refuse to
	// join. Warn loudly so the user knows to wipe storage before the
	// next daemon start.
	//
	// We warn in two cases:
	//  - sidecar recorded a previous secret that differs from the new
	//    one (the simple "rotated the token" case);
	//  - the sidecar's previous secret is empty (e.g. blanked by hand
	//    or by a linter) but the storage dir has prior bot state (the
	//    bkp_state file). We can't tell whether the secret changed
	//    or not, so we err on the side of warning.
	storage := tbot.StorageDirPath(resolvedDir)
	suspectStale := sidecar.RegistrationSecret != "" && sidecar.RegistrationSecret != resolvedSecret
	if !suspectStale && sidecar.RegistrationSecret == "" {
		if _, err := os.Stat(filepath.Join(storage, "bkp_state")); err == nil {
			suspectStale = true
		}
	}
	if suspectStale {
		fmt.Fprintf(os.Stderr, `
prism tbot configure: WARNING: previous tbot identity may not match the new registration secret.

tbot's stored bound-keypair state in
    %s
is tied to whichever secret produced it. If the token was recreated since
then, tbot will fail to join (the cluster will reject the new keypair tbot
generates because the prior secret was already redeemed). To fully recover:

    tctl rm token/%s
    tctl create -f %s/token.yaml
    prism tbot configure --dir %s
    rm -rf %s

The recreated token gets a fresh registration_secret which prism will
auto-fetch on the next configure run.

(If you're certain the secret hasn't actually changed, you can ignore this.)

`, storage, sidecar.TokenName, resolvedDir, resolvedDir, storage)
	}
	sidecar.RegistrationSecret = resolvedSecret
	if err := tbot.SaveSidecar(resolvedDir, sidecar); err != nil {
		return fmt.Errorf("save sidecar: %w", err)
	}

	if err := writePlaceholderTbotYAML(resolvedDir, sidecar); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "prism tbot configure: wrote sidecar and initial tbot.yaml in %s\n", resolvedDir)
	printConfigureNextSteps(resolvedDir)
	return nil
}

// printConfigureNextSteps prints only the follow-on commands the user
// hasn't already run. The full sequence is laid out by `prism tbot
// bootstrap`; configure is in the middle of that sequence, so repeating
// every line each time is just noise. We compare the loaded config
// against what tbot mode requires and emit only the missing bits.
func printConfigureNextSteps(resolvedDir string) {
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	var steps []string
	if cfg.Identity != state.IdentitySourceTbot {
		steps = append(steps, "prism config set identity tbot")
	}
	if cfg.TbotDir != resolvedDir {
		steps = append(steps, "prism config set tbot.dir "+resolvedDir)
	}
	steps = append(steps, "prism up")
	fmt.Fprintf(os.Stderr, "Next: %s\n", strings.Join(steps, " && "))
}

// --- status ---

func cmdTbotStatus(args []string) error {
	fs := flag.NewFlagSet("tbot status", flag.ExitOnError)
	dir := fs.String("dir", "", "tbot working directory (default ~/.config/prism/tbot, or the configured tbot.dir)")
	_ = fs.Parse(args)
	resolvedDir, err := resolveTbotDir(*dir)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "tbot dir:   %s\n", resolvedDir)

	problems := tbot.Validate(resolvedDir)
	if len(problems) > 0 {
		fmt.Fprintln(os.Stdout, "validation:")
		for _, p := range problems {
			fmt.Fprintf(os.Stdout, "  - %s\n", p)
		}
		return nil
	}
	fmt.Fprintln(os.Stdout, "validation: ok")

	sidecar, err := tbot.LoadSidecar(resolvedDir)
	if err != nil {
		return err
	}
	hasSecret := "yes"
	if sidecar.RegistrationSecret == "" {
		hasSecret = "NO — run `prism tbot configure`"
	}
	fmt.Fprintf(os.Stdout, "proxy:      %s\ntoken:      %s\nrole:       %s\nbot:        %s\nsecret set: %s\n",
		sidecar.ProxyServer, sidecar.TokenName, sidecar.RoleName, sidecar.BotName, hasSecret)

	s, _ := state.Load()
	switch {
	case s == nil:
		fmt.Fprintln(os.Stdout, "session:    none (run `prism up`)")
	case s.Identity() == state.IdentitySourceTbot && s.TbotDir == resolvedDir:
		fmt.Fprintf(os.Stdout, "session:    active (router=127.0.0.1:%d)\n", s.LocalPort)
		fmt.Fprintf(os.Stdout, "health:     %s\n", tbotHealthLine(s.TbotDiagPort))
	case s.Identity() == state.IdentitySourceTbot:
		fmt.Fprintf(os.Stdout, "session:    active but pointed at a different tbot dir (%s)\n", s.TbotDir)
	default:
		fmt.Fprintf(os.Stdout, "session:    active in %s mode (this tbot dir is not currently in use)\n", s.Identity())
	}
	return nil
}

// --- helpers ---

// resourcePaths are the three YAML files prism writes for the user (or
// for itself, in `prism tbot reset`) to apply to the cluster.
type resourcePaths struct{ Role, Bot, Token string }

// writeResourceYAML renders prism's role/bot/token templates for names
// into dir. Shared by bootstrap and reset: both have to produce exactly
// the same resources, or a reset would quietly change what a machine
// is allowed to reach.
func writeResourceYAML(dir string, names tbot.Names) (resourcePaths, error) {
	paths := resourcePaths{
		Role:  filepath.Join(dir, "role.yaml"),
		Bot:   filepath.Join(dir, "bot.yaml"),
		Token: filepath.Join(dir, "token.yaml"),
	}
	for path, body := range map[string][]byte{
		paths.Role:  tbot.RenderRoleYAML(tbot.RoleYAMLOpts{RoleName: names.Role}),
		paths.Bot:   tbot.RenderBotYAML(tbot.BotYAMLOpts{BotName: names.Bot, RoleName: names.Role}),
		paths.Token: tbot.RenderTokenYAML(tbot.TokenYAMLOpts{TokenName: names.Token, BotName: names.Bot}),
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return paths, fmt.Errorf("write %s: %w", path, err)
		}
	}
	return paths, nil
}

// writePlaceholderTbotYAML writes an initial tbot.yaml so the directory
// validates before the daemon rewrites it on first Runtime.Prepare.
// Until then, a tbot.yaml naming the placeholder app gives the user
// something concrete to inspect.
func writePlaceholderTbotYAML(dir string, sidecar *tbot.SidecarConfig) error {
	body := tbot.RenderTbotYAML(tbot.TbotYAMLOpts{
		TokenName:          sidecar.TokenName,
		RegistrationSecret: sidecar.RegistrationSecret,
		StoragePath:        tbot.StorageDirPath(dir),
		Services: []tbot.AppTunnelService{
			{Name: tbot.AppTunnelServiceName, AppName: "prism-placeholder", Port: 7331},
		},
		ProxyServer: sidecar.ProxyServer,
	})
	if err := os.WriteFile(tbot.TbotYAMLPath(dir), body, 0o600); err != nil {
		return fmt.Errorf("write tbot.yaml: %w", err)
	}
	return nil
}

// resolveTbotDir picks the tbot working directory in this order:
// explicit flag, persistent config (`tbot.dir`), `~/.config/prism/tbot`.
func resolveTbotDir(flagVal string) (string, error) {
	if flagVal != "" {
		return expandHome(flagVal)
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	if cfg.TbotDir != "" {
		return expandHome(cfg.TbotDir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "prism", "tbot"), nil
}

// expandHome replaces a leading ~/ with the user's home directory.
func expandHome(path string) (string, error) {
	if !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, path[2:]), nil
}

// fetchRegistrationSecret shells out to
// `tctl get token/<name> --format=json` and extracts the cluster-
// generated bound-keypair registration secret.
//
// We parse the JSON in Go rather than asking the user to install jq
// — the layout is stable enough that we can navigate it directly, and
// not requiring an external dep simplifies bootstrap UX significantly
// (jq isn't installed by default on macOS or stock Debian).
func fetchRegistrationSecret(proxy, tokenName string) (string, error) {
	stdout, err := captureTctl(proxy, "get", "token/"+tokenName, "--format=json")
	if err != nil {
		return "", fmt.Errorf("tctl get token/%s: %w", tokenName, err)
	}

	// `tctl get` returns a JSON array of resources even for a single
	// resource lookup. Walk to .[0].status.bound_keypair.registration_secret.
	var resources []struct {
		Status struct {
			BoundKeypair struct {
				RegistrationSecret string `json:"registration_secret"`
			} `json:"bound_keypair"`
		} `json:"status"`
	}
	if err := json.Unmarshal(stdout, &resources); err != nil {
		return "", fmt.Errorf("parse tctl output: %w", err)
	}
	if len(resources) == 0 {
		return "", fmt.Errorf("tctl returned no resources for token/%s", tokenName)
	}
	secret := strings.TrimSpace(resources[0].Status.BoundKeypair.RegistrationSecret)
	if secret == "" {
		return "", fmt.Errorf("registration_secret is empty for token/%s — the token may have already been redeemed; recreate it with `tctl rm token/%s && tctl create -f token.yaml`", tokenName, tokenName)
	}
	return secret, nil
}

// proxySource says where a resolved proxy came from, so a command can
// tell the user which cluster it is about to act on and why.
type proxySource string

const (
	proxyFromFlag    proxySource = "--proxy"
	proxyFromConfig  proxySource = "prism's config"
	proxyFromSidecar proxySource = "this machine's current tbot config"
	proxyFromLogin   proxySource = "your tsh login"
)

// resolveProxy picks the cluster a tbot command acts on.
//
// **prism's configured proxy is the authority**, not whichever profile
// the user's shell happens to be logged into: a machine configured for
// one cluster must not quietly move to another because somebody ran
// `tsh login` elsewhere, and moving clusters on purpose means setting
// `prism config set proxy <addr>` first and then bootstrapping or
// resetting. The login is consulted only when nothing is configured at
// all, which is a first bootstrap.
func resolveProxy(flagProxy string, existing *tbot.SidecarConfig) (string, proxySource, error) {
	if flagProxy != "" {
		return flagProxy, proxyFromFlag, nil
	}
	if cfg, err := config.Load(); err == nil && cfg.Proxy != "" {
		return cfg.Proxy, proxyFromConfig, nil
	}
	if existing != nil && existing.ProxyServer != "" {
		return existing.ProxyServer, proxyFromSidecar, nil
	}
	st, err := tshwrap.StatusJSON()
	if err != nil {
		return "", "", fmt.Errorf("no proxy configured and `tsh status` failed — run `prism config set proxy <host:port>`, or pass --proxy: %w", err)
	}
	if st.ProfileURL != "" {
		if u, err := url.Parse(st.ProfileURL); err == nil && u.Host != "" {
			return u.Host, proxyFromLogin, nil
		}
	}
	if st.Cluster == "" {
		return "", "", fmt.Errorf("no proxy configured and you are not logged in to Teleport — run `prism config set proxy <host:port>`, or pass --proxy")
	}
	return st.Cluster + ":443", proxyFromLogin, nil
}
