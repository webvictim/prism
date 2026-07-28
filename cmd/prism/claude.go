package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"

	"github.com/gravitational/prism/internal/state"
	"github.com/gravitational/prism/internal/tshwrap"
)

// cmdClaude ensures prism is up, then runs `claude` with the prism env set.
func cmdClaude(args []string) error {
	return runToolWithPrism("claude", args, true)
}

// cmdCodex ensures prism is up, then runs `codex` with the prism env set.
func cmdCodex(args []string) error {
	return runToolWithPrism("codex", args, false)
}

// cmdExec ensures prism is up, then runs an arbitrary command with the prism env set.
func cmdExec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: prism exec <command> [args...]")
	}
	return runToolWithPrism(args[0], args[1:], false)
}

func runToolWithPrism(tool string, args []string, unsetAnthropicKey bool) error {
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

	env := make([]string, 0, len(os.Environ())+8)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "ANTHROPIC_API_KEY="),
			strings.HasPrefix(kv, "ANTHROPIC_API_URL="),
			strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN="),
			strings.HasPrefix(kv, "ANTHROPIC_BASE_URL="),
			strings.HasPrefix(kv, "OPENAI_API_BASE="),
			strings.HasPrefix(kv, "OPENAI_API_KEY="),
			strings.HasPrefix(kv, "OPENAI_BASE_URL="):
			continue
		}
		env = append(env, kv)
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", s.LocalPort)
	if unsetAnthropicKey {
		env = append(env,
			fmt.Sprintf("ANTHROPIC_BASE_URL=%s", base),
			fmt.Sprintf("OPENAI_API_BASE=%s/v1", base),
			"OPENAI_API_KEY=teleport",
			fmt.Sprintf("OPENAI_BASE_URL=%s/v1", base),
		)
	} else {
		env = append(env,
			"ANTHROPIC_API_KEY=teleport",
			fmt.Sprintf("ANTHROPIC_API_URL=%s", base),
			"ANTHROPIC_AUTH_TOKEN=teleport",
			fmt.Sprintf("ANTHROPIC_BASE_URL=%s", base),
			fmt.Sprintf("OPENAI_API_BASE=%s/v1", base),
			"OPENAI_API_KEY=teleport",
			fmt.Sprintf("OPENAI_BASE_URL=%s/v1", base),
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
