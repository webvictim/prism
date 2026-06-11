package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/gravitational/prism/internal/config"
	"github.com/gravitational/prism/internal/state"
	"github.com/gravitational/prism/internal/tbot"
	"github.com/gravitational/prism/internal/tshwrap"
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

After bootstrap + configure:
  prism config set identity tbot
  prism config set tbot.dir <dir>
  prism up
  prism install          (Linux: optional systemd user service)
`)
	return nil
}

// --- bootstrap ---

func cmdTbotBootstrap(args []string) error {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown"
	}
	// Sanitise hostname for use in Teleport resource names (lowercase, no dots).
	hostname = strings.ToLower(strings.Split(hostname, ".")[0])

	defaultToken := "prism-bot-token-" + hostname
	defaultRole := "prism-bot-role-" + hostname
	defaultBot := "prism-bot-" + hostname

	fs := flag.NewFlagSet("tbot bootstrap", flag.ExitOnError)
	dir := fs.String("dir", "", "tbot working directory (default ~/.config/prism/tbot)")
	proxy := fs.String("proxy", "", "Teleport proxy host:port (default from `tsh status`)")
	tokenName := fs.String("token-name", defaultToken, "name for the join token resource")
	roleName := fs.String("role-name", defaultRole, "name for the bot's role resource")
	botName := fs.String("bot-name", defaultBot, "name for the bot resource")
	_ = fs.Parse(args)

	resolvedDir, err := resolveTbotDir(*dir)
	if err != nil {
		return err
	}
	resolvedProxy, err := resolveProxy(*proxy)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Join(resolvedDir, "storage"), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", resolvedDir, err)
	}

	// Detect existing sidecar — if present, preserve its registration
	// secret and only update the config files (role.yaml gains openai
	// access, tbot.yaml gets the multi-service format).
	existingSidecar, _ := tbot.LoadSidecar(resolvedDir)
	isUpdate := existingSidecar != nil && existingSidecar.RegistrationSecret != ""

	// If the existing sidecar uses old-style generic names (without hostname),
	// we need to migrate to hostname-scoped names. This requires new cluster
	// resources, so we wipe the registration secret and treat it as a fresh
	// bootstrap (the user will need to tctl create the new resources).
	if isUpdate && !strings.Contains(existingSidecar.TokenName, hostname) {
		fmt.Fprintf(os.Stderr, `prism tbot bootstrap: migrating from generic names to hostname-scoped names:
  token: %s → %s
  role:  %s → %s
  bot:   %s → %s

This requires creating new cluster resources. The old ones can be removed:
  tctl rm token/%s
  tctl rm role/%s
  tctl rm bot/%s

`, existingSidecar.TokenName, *tokenName,
			existingSidecar.RoleName, *roleName,
			existingSidecar.BotName, *botName,
			existingSidecar.TokenName, existingSidecar.RoleName, existingSidecar.BotName)
		// Wipe storage so tbot re-joins with the new token.
		storagePath := tbot.StorageDirPath(resolvedDir)
		_ = os.RemoveAll(storagePath)
		_ = os.MkdirAll(storagePath, 0o700)
		isUpdate = false
	}

	if isUpdate {
		fmt.Fprintln(os.Stderr, "prism tbot bootstrap: existing sidecar detected, updating in-place (preserving registration secret)")
		if *tokenName == "prism-bot-token" && existingSidecar.TokenName != "" {
			*tokenName = existingSidecar.TokenName
		}
		if *roleName == "prism-bot-role" && existingSidecar.RoleName != "" {
			*roleName = existingSidecar.RoleName
		}
		if *botName == "prism-bot" && existingSidecar.BotName != "" {
			*botName = existingSidecar.BotName
		}
	}

	roleBody := tbot.RenderRoleYAML(tbot.RoleYAMLOpts{RoleName: *roleName})
	botBody := tbot.RenderBotYAML(tbot.BotYAMLOpts{BotName: *botName, RoleName: *roleName})
	tokenBody := tbot.RenderTokenYAML(tbot.TokenYAMLOpts{TokenName: *tokenName, BotName: *botName})

	rolePath := filepath.Join(resolvedDir, "role.yaml")
	botPath := filepath.Join(resolvedDir, "bot.yaml")
	tokenPath := filepath.Join(resolvedDir, "token.yaml")
	if err := os.WriteFile(rolePath, roleBody, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", rolePath, err)
	}
	if err := os.WriteFile(botPath, botBody, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", botPath, err)
	}
	if err := os.WriteFile(tokenPath, tokenBody, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tokenPath, err)
	}

	// Preserve existing secret, update everything else.
	sidecar := &tbot.SidecarConfig{
		ProxyServer: resolvedProxy,
		TokenName:   *tokenName,
		RoleName:    *roleName,
		BotName:     *botName,
	}
	if isUpdate {
		sidecar.RegistrationSecret = existingSidecar.RegistrationSecret
	}
	if err := tbot.SaveSidecar(resolvedDir, sidecar); err != nil {
		return fmt.Errorf("save sidecar: %w", err)
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
  prism config set tbot.dir %s
  prism up

On Linux, optionally install as a systemd user service for persistence:

  prism install

(`+"`prism tbot configure`"+` reads the bound-keypair registration secret
from the cluster via tctl. If you'd rather pass it explicitly, append
`+"`--registration-secret <value>`"+`.)

`, resolvedDir, resolvedProxy, rolePath, botPath, tokenPath, resolvedDir, resolvedDir)
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
		fetched, err := fetchRegistrationSecret(sidecar.TokenName)
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

	// Write an initial tbot.yaml so the dir validates before the daemon
	// rewrites it on first Runtime.Prepare. Until then, listing tbot.yaml
	// with the placeholder app gives users something concrete to inspect.
	placeholder := tbot.RenderTbotYAML(tbot.TbotYAMLOpts{
		TokenName:          sidecar.TokenName,
		RegistrationSecret: sidecar.RegistrationSecret,
		StoragePath:        tbot.StorageDirPath(resolvedDir),
		Services: []tbot.AppTunnelService{
			{Name: tbot.AppTunnelServiceName, AppName: "prism-placeholder", Port: 7331},
		},
		ProxyServer: sidecar.ProxyServer,
	})
	if err := os.WriteFile(tbot.TbotYAMLPath(resolvedDir), placeholder, 0o600); err != nil {
		return fmt.Errorf("write tbot.yaml: %w", err)
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

// resolveTbotDir picks the tbot working directory in this order:
// explicit flag, persistent config (`tbot.dir`), `~/.config/prism/tbot`.
func resolveTbotDir(flagVal string) (string, error) {
	if flagVal != "" {
		return flagVal, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	if cfg.TbotDir != "" {
		return cfg.TbotDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "prism", "tbot"), nil
}

// fetchRegistrationSecret shells out to
// `tctl get token/<name> --format=json` and extracts the cluster-
// generated bound-keypair registration secret.
//
// We parse the JSON in Go rather than asking the user to install jq
// — the layout is stable enough that we can navigate it directly, and
// not requiring an external dep simplifies bootstrap UX significantly
// (jq isn't installed by default on macOS or stock Debian).
func fetchRegistrationSecret(tokenName string) (string, error) {
	tctlBin, err := tshwrap.LookPathStrict("tctl")
	if err != nil {
		return "", errors.New("tctl not on PATH — install Teleport's admin CLI, or pass --registration-secret explicitly")
	}
	cmd := exec.Command(tctlBin, "get", "token/"+tokenName, "--format=json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("tctl get token/%s: %s", tokenName, msg)
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
	if err := json.Unmarshal(stdout.Bytes(), &resources); err != nil {
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

// resolveProxy fills in the Teleport proxy address from `tsh status`
// when the caller didn't supply --proxy explicitly.
func resolveProxy(flagProxy string) (string, error) {
	if flagProxy != "" {
		return flagProxy, nil
	}
	st, err := tshwrap.StatusJSON()
	if err != nil {
		return "", fmt.Errorf("could not read `tsh status` for defaults (pass --proxy explicitly to skip): %w", err)
	}
	if st.ProfileURL != "" {
		u, err := url.Parse(st.ProfileURL)
		if err == nil && u.Host != "" {
			return u.Host, nil
		}
	}
	if st.Cluster == "" {
		return "", fmt.Errorf("`tsh status` returned no proxy — pass --proxy explicitly")
	}
	return st.Cluster + ":443", nil
}

