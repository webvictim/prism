package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/mitm"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tshwrap"
)

// cmdClaude ensures prism is up, then runs `claude` with the prism env set.
func cmdClaude(args []string) error {
	return runToolWithPrism("claude", args)
}

// cmdCodex ensures prism is up, then runs `codex` with the prism env set.
func cmdCodex(args []string) error {
	return runToolWithPrism("codex", args)
}

// cmdExec ensures prism is up, then runs an arbitrary command with the prism env set.
func cmdExec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: prism exec <command> [args...]")
	}
	return runToolWithPrism(args[0], args[1:])
}

func runToolWithPrism(tool string, args []string) error {
	s, _ := state.Load()
	running := false
	if isServiceManaged() && serviceIsActive() {
		running = true
	} else if s != nil && s.DaemonPID != 0 && processAlive(s.DaemonPID) {
		running = true
	} else if s != nil && s.LocalPort != 0 && portResponds(s.LocalPort) {
		running = true
	}
	if !running {
		fmt.Fprintln(os.Stderr, "prism: not running, bringing it up first…")
		if err := cmdUp(nil); err != nil {
			return err
		}
		s, _ = state.Load()
		if s == nil {
			return fmt.Errorf("prism up succeeded but state is missing")
		}
	}

	bin, err := tshwrap.LookPathStrict(tool)
	if err != nil {
		return fmt.Errorf("`%s` not found on PATH: %w", tool, err)
	}

	// When claude_forward_proxy_mode is enabled and we're launching
	// claude, use HTTPS_PROXY instead of ANTHROPIC_BASE_URL so that
	// Remote Control stays available.
	cfg, _ := config.Load()
	forwardProxy := cfg != nil && cfg.ClaudeForwardProxyMode && tool == "claude"

	env := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "ANTHROPIC_API_KEY="),
			strings.HasPrefix(kv, "ANTHROPIC_BASE_URL="),
			strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN="),
			strings.HasPrefix(kv, "OPENAI_BASE_URL="),
			strings.HasPrefix(kv, "OPENAI_API_KEY="):
			continue
		case forwardProxy && (strings.HasPrefix(kv, "HTTPS_PROXY=") ||
			strings.HasPrefix(kv, "https_proxy=") ||
			strings.HasPrefix(kv, "NODE_EXTRA_CA_CERTS=")):
			continue
		}
		env = append(env, kv)
	}

	if forwardProxy {
		configDir, _ := config.Dir()
		caPath := mitm.CACertPath(configDir)
		env = append(env,
			fmt.Sprintf("HTTPS_PROXY=http://127.0.0.1:%d", s.LocalPort),
			fmt.Sprintf("NODE_EXTRA_CA_CERTS=%s", caPath),
			fmt.Sprintf("OPENAI_BASE_URL=http://127.0.0.1:%d/v1", s.LocalPort),
			"OPENAI_API_KEY=teleport",
		)
	} else {
		env = append(env,
			fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", s.LocalPort),
			fmt.Sprintf("OPENAI_BASE_URL=http://127.0.0.1:%d/v1", s.LocalPort),
			"OPENAI_API_KEY=teleport",
		)
	}

	cmd := exec.Command(bin, args...)
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", tool, err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, forwardedSignals()...)
	go func() {
		for sig := range sigCh {
			_ = cmd.Process.Signal(sig)
		}
	}()
	defer signal.Stop(sigCh)

	err = cmd.Wait()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("wait %s: %w", tool, err)
	}
	return nil
}
