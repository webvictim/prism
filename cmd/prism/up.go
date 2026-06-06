package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/gravitational/prism/internal/config"
	"github.com/gravitational/prism/internal/router"
	"github.com/gravitational/prism/internal/state"
	"github.com/gravitational/prism/internal/tbot"
	"github.com/gravitational/prism/internal/tshwrap"
)

const defaultLocalPort = 7331

func cmdUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	port := fs.Int("port", defaultLocalPort, "local router port")
	proxy := fs.String("proxy", "", "Teleport proxy address (host:port); persisted to config")
	useTsh := fs.Bool("tsh", false, "force tsh identity backend for this invocation")
	_ = fs.Parse(args)

	if *proxy != "" {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		cfg.Proxy = *proxy
		if err := config.Save(cfg); err != nil {
			return err
		}
		_ = os.Setenv("TELEPORT_PROXY", *proxy)
		fmt.Fprintf(os.Stderr, "prism: saved proxy=%s to config\n", *proxy)
	}
	if os.Getenv("TELEPORT_PROXY") == "" {
		fmt.Fprintln(os.Stderr, "prism: warning: no Teleport proxy configured. Set one with `prism config set proxy <host:port>` or pass --proxy.")
	}

	// If already running, just reprint env.
	existing, _ := state.Load()
	if isServiceManaged() && serviceIsActive() {
		fmt.Fprintln(os.Stderr, "prism: already running (service-managed); reprinting env")
		return cmdEnv(nil)
	}
	if existing != nil && existing.DaemonPID != 0 && processAlive(existing.DaemonPID) {
		fmt.Fprintln(os.Stderr, "prism: already running; reprinting env (use `prism down` first to restart)")
		return cmdEnv(nil)
	}
	if existing != nil && portResponds(existing.LocalPort) {
		fmt.Fprintln(os.Stderr, "prism: already running (router port responding); reprinting env")
		return cmdEnv(nil)
	}

	// Migrate from legacy beam-based state if present.
	if state.IsLegacyState() {
		fmt.Fprintln(os.Stderr, "prism: detected legacy beam-based state, migrating…")
		migrateLegacyState(existing)
		existing = nil
	}

	debug := os.Getenv("PRISM_DEBUG") == "1"
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "prism: "+format+"\n", args...) }

	// Resolve identity.
	identitySrc, tbotDir, err := resolveIdentitySource(*useTsh, existing)
	if err != nil {
		return err
	}
	logf("using identity: %s", describeIdentity(identitySrc, tbotDir))

	// In tsh mode, login to both cluster apps.
	if identitySrc == state.IdentitySourceTSH {
		logf("logging into %s app…", router.AnthropicAppName)
		if err := tshwrap.AppLogin(router.AnthropicAppName); err != nil {
			return fmt.Errorf("app login %s: %w", router.AnthropicAppName, err)
		}
		logf("logging into %s app…", router.OpenAIAppName)
		if err := tshwrap.AppLogin(router.OpenAIAppName); err != nil {
			logf("warning: could not login to %s app: %v (OpenAI routing will be unavailable)", router.OpenAIAppName, err)
		}
	}

	// Pick ports.
	localPort := *port
	if !portFree(localPort) {
		return fmt.Errorf("local port %d is in use", localPort)
	}
	used := []int{localPort}

	tbotDiagPort := 0
	if identitySrc == state.IdentitySourceTbot {
		tbotDiagPort = pickPortNear(localPort+1, used, existingPort(existing, func(s *state.State) int { return s.TbotDiagPort }))
		used = append(used, tbotDiagPort)
	}

	anthropicPort := pickPortNear(localPort+2, used, existingPort(existing, func(s *state.State) int { return s.AnthropicPort }))
	used = append(used, anthropicPort)

	openAIPort := pickPortNear(localPort+3, used, existingPort(existing, func(s *state.State) int { return s.OpenAIPort }))

	// Save state.
	s := &state.State{
		LocalPort:      localPort,
		AnthropicPort:  anthropicPort,
		OpenAIPort:     openAIPort,
		IdentitySource: identitySrc,
		TbotDir:        tbotDir,
		TbotDiagPort:   tbotDiagPort,
		Debug:          debug,
		CreatedAt:      time.Now().UTC(),
	}
	if err := state.Save(s); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	// Launch daemon — via systemd if installed, otherwise fork-exec.
	if isServiceManaged() {
		fmt.Fprintln(os.Stderr, "prism: starting via service manager…")
		if err := serviceStart(); err != nil {
			return fmt.Errorf("service start: %w", err)
		}
	} else {
		logPath, err := state.DaemonLogPath()
		if err != nil {
			return err
		}
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		defer logFile.Close()

		self, err := os.Executable()
		if err != nil {
			return err
		}
		daemonCmd := exec.Command(self, "__daemon")
		daemonCmd.Stdout = logFile
		daemonCmd.Stderr = logFile
		daemonCmd.Stdin = nil
		setDetachedAttrs(daemonCmd)
		if err := daemonCmd.Start(); err != nil {
			return fmt.Errorf("start daemon: %w", err)
		}

		s.DaemonPID = daemonCmd.Process.Pid
		if err := state.Save(s); err != nil {
			return fmt.Errorf("save state with pid: %w", err)
		}
	}

	// Wait for the router port to come up.
	waitBudget := 5 * time.Second
	if identitySrc == state.IdentitySourceTbot {
		waitBudget = 20 * time.Second
	}
	if !waitForPort(localPort, waitBudget) {
		if isServiceManaged() {
			logHint := "check daemon log"
			if serviceManagedLabel() == "systemd-managed" {
				logHint = "check `journalctl --user -u prism`"
			}
			return fmt.Errorf("daemon did not bind 127.0.0.1:%d within %s — %s", localPort, waitBudget, logHint)
		}
		logPath, _ := state.DaemonLogPath()
		return fmt.Errorf("daemon did not bind 127.0.0.1:%d within %s — check %s", localPort, waitBudget, logPath)
	}

	if isServiceManaged() {
		fmt.Fprintf(os.Stderr, "prism: up (%s). Router on 127.0.0.1:%d\n", serviceManagedLabel(), localPort)
	} else {
		fmt.Fprintf(os.Stderr, "prism: up. Daemon PID %d, router on 127.0.0.1:%d\n", s.DaemonPID, localPort)
	}
	return cmdEnv(nil)
}

func resolveIdentitySource(forceTsh bool, existing *state.State) (string, string, error) {
	cfg, err := config.Load()
	if err != nil {
		return "", "", err
	}
	src := state.IdentitySourceTSH
	switch {
	case forceTsh:
		src = state.IdentitySourceTSH
	case existing != nil && existing.IdentitySource != "":
		src = existing.Identity()
	case cfg.Identity != "":
		src = cfg.Identity
	}
	if src != state.IdentitySourceTbot {
		return src, "", nil
	}
	dir := cfg.TbotDir
	if existing != nil && existing.TbotDir != "" {
		dir = existing.TbotDir
	}
	if dir == "" {
		return "", "", fmt.Errorf("identity=tbot but tbot.dir is not set — run `prism tbot bootstrap` and then `prism config set tbot.dir <path>`")
	}
	if problems := tbot.Validate(dir); len(problems) > 0 {
		msg := "tbot dir " + dir + " is not ready:\n"
		for _, p := range problems {
			msg += "  - " + p + "\n"
		}
		return "", "", fmt.Errorf("%s", msg)
	}
	return src, dir, nil
}

func describeIdentity(src, tbotDir string) string {
	switch src {
	case state.IdentitySourceTbot:
		if tbotDir != "" {
			return fmt.Sprintf("tbot (Machine ID, dir=%s)", tbotDir)
		}
		return "tbot (Machine ID)"
	default:
		return "tsh (interactive login)"
	}
}

func existingPort(s *state.State, getter func(*state.State) int) int {
	if s == nil {
		return 0
	}
	return getter(s)
}

func pickPortNear(preferred int, avoid []int, prevPick int) int {
	avoided := make(map[int]bool, len(avoid))
	for _, p := range avoid {
		avoided[p] = true
	}
	if prevPick != 0 && !avoided[prevPick] && portFree(prevPick) {
		return prevPick
	}
	for offset := 0; offset < 32; offset++ {
		p := preferred + offset
		if !avoided[p] && portFree(p) {
			return p
		}
	}
	return 0
}

func portFree(p int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

func waitForPort(p int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// migrateLegacyState kills any running legacy daemon and deletes the
// old state file so a fresh `prism up` can proceed.
func migrateLegacyState(existing *state.State) {
	if existing != nil && existing.DaemonPID != 0 && processAlive(existing.DaemonPID) {
		if p, err := os.FindProcess(existing.DaemonPID); err == nil {
			_ = signalShutdown(p)
			time.Sleep(2 * time.Second)
			if processAlive(existing.DaemonPID) {
				_ = signalKill(p)
			}
		}
		fmt.Fprintln(os.Stderr, "prism: stopped legacy daemon")
	}
	_ = state.Delete()
	fmt.Fprintln(os.Stderr, "prism: legacy state removed. Beam (if any) left running — destroy it manually with `tsh beams rm <id>` if needed.")
}
