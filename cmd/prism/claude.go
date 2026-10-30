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

// cmdOpencode ensures prism is up, then runs `opencode` with the prism env set.
func cmdOpencode(args []string) error {
	return runToolWithPrism("opencode", args)
}

// cmdExec ensures prism is up, then runs an arbitrary command with the prism env set.
func cmdExec(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: prism exec <command> [args...]")
	}
	return runToolWithPrism(args[0], args[1:])
}

func runToolWithPrism(tool string, args []string) error {
	return runToolWithPrismSetup(tool, args, nil)
}

// runToolWithPrismSetup is runToolWithPrism with an optional setup step after
// prism is running and the tool has been resolved, but before it is started.
func runToolWithPrismSetup(tool string, args []string, setup func(bin string, port int, env []string) error) error {
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

	var caPath string
	if forwardProxy {
		configDir, _ := config.Dir()
		caPath = mitm.CACertPath(configDir)
	}

	// Claude Code needs a credential of its own before it will send
	// anything, even though prism strips it; see claude_auth.go.
	auth := claudeAuthPlan{}
	if tool == "claude" {
		mode := claudeAuthAuto
		if cfg != nil && cfg.ClaudeAuth != "" {
			mode = cfg.ClaudeAuth
		}
		auth = planClaudeAuth(mode, forwardProxy, claudeLoginPinned(), func() (claudeLogin, error) {
			return claudeLoginStatus(bin, os.Environ())
		})
		if auth.Note != "" {
			fmt.Fprintln(os.Stderr, auth.Note)
		}
	}

	// Codex no longer reads OPENAI_BASE_URL, so it has to be pointed at
	// prism on the command line; see codex.go.
	if tool == "codex" {
		args = codexConfigArgs(s.LocalPort, args)
	}

	env := toolEnv(tool, s.LocalPort, forwardProxy, caPath, auth, os.Environ())
	if setup != nil {
		if err := setup(bin, s.LocalPort, env); err != nil {
			return err
		}
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

// toolEnv builds the environment for a tool launched through prism:
// environ with any inherited endpoint/credential vars removed, then
// prism's own values appended.
//
// caPath is only consulted when forwardProxy is set; auth is resolved by
// planClaudeAuth and only ever acted on for claude.
func toolEnv(tool string, port int, forwardProxy bool, caPath string, auth claudeAuthPlan, environ []string) []string {
	env := make([]string, 0, len(environ)+8)
	for _, kv := range environ {
		switch {
		case strings.HasPrefix(kv, "ANTHROPIC_API_KEY="),
			strings.HasPrefix(kv, "ANTHROPIC_BASE_URL="),
			strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN="),
			strings.HasPrefix(kv, "OPENAI_BASE_URL="),
			strings.HasPrefix(kv, "OPENAI_API_KEY="):
			continue
		case strings.HasPrefix(kv, claudePlatformEnv+"="),
			strings.HasPrefix(kv, claudePlatformSkipAuthEnv+"="),
			strings.HasPrefix(kv, claudePlatformBaseURLEnv+"="):
			// Set below when prism chose platform mode, and dropped
			// otherwise so an inherited one can't aim Claude Code at a
			// stale port or a provider prism didn't pick.
			continue
		case forwardProxy && (strings.HasPrefix(kv, "HTTPS_PROXY=") ||
			strings.HasPrefix(kv, "https_proxy=") ||
			strings.HasPrefix(kv, "NODE_EXTRA_CA_CERTS=")):
			continue
		}
		env = append(env, kv)
	}

	if forwardProxy {
		env = append(env,
			fmt.Sprintf("HTTPS_PROXY=http://127.0.0.1:%d", port),
			fmt.Sprintf("NODE_EXTRA_CA_CERTS=%s", caPath),
		)
	} else {
		env = append(env, fmt.Sprintf("ANTHROPIC_BASE_URL=http://127.0.0.1:%d", port))
	}

	env = append(env,
		fmt.Sprintf("OPENAI_BASE_URL=http://127.0.0.1:%d/v1", port),
		"OPENAI_API_KEY=teleport",
	)

	// OpenCode and Pi register a provider only when credentials are
	// available. A dummy Anthropic key makes the provider visible on a
	// fresh install; Pi also needs it when bootstrapping models-store.json.
	// The value is never used: the router strips X-Api-Key before
	// forwarding, the same reason OPENAI_API_KEY above can be a dummy.
	// Claude Code is deliberately excluded, since a key in the environment
	// changes which auth it picks — and an env ANTHROPIC_API_KEY makes it
	// raise its "Detected a custom API key … use this API key?"
	// onboarding step, whose pre-selected answer is "No (recommended)"
	// and lands the user in the browser login flow. Claude Code gets
	// ANTHROPIC_AUTH_TOKEN below instead, which has no such gate.
	if tool == "opencode" || tool == "pi" {
		env = append(env, "ANTHROPIC_API_KEY=teleport")
	}

	// A dummy gateway token so Claude Code stops short of
	// "Not logged in · Please run /login" without a claude.ai
	// subscription; see claude_auth.go for when this is set.
	if auth.GatewayToken {
		env = append(env, "ANTHROPIC_AUTH_TOKEN="+claudeGatewayToken)
	}

	// Or no Anthropic credential at all, with Claude Code told it is a
	// third-party platform client. That is the only shape that works on
	// a machine whose managed settings pin sign-ins, and it is what a
	// Teleport beam does against the very same gateway.
	if auth.Platform {
		env = append(env, claudePlatformEnv+"=1",
			claudePlatformSkipAuthEnv+"=1",
			fmt.Sprintf("%s=http://127.0.0.1:%d", claudePlatformBaseURLEnv, port))
	}

	// Newer Claude Code builds ask the API to run auto mode's safety
	// classifier server-side (a `safeguards` request field, answered by
	// `safeguard_results`). The Bedrock-backed gateway never answers, and
	// when Claude Code believes it's talking to the first-party API — as
	// it does in forward-proxy mode — it has no local fallback, so every
	// Bash call in auto mode is denied with "The server-side auto mode
	// classifier gave no verdict". Turning the server classifier off
	// makes Claude Code run its local classifier, an ordinary
	// /v1/messages request. An explicit value from the user wins.
	if tool == "claude" && !hasEnv(environ, autoModeServerEnv) {
		env = append(env, autoModeServerEnv+"=0")
	}

	return env
}

// autoModeServerEnv switches Claude Code's server-side auto mode
// classifier; see toolEnv.
const autoModeServerEnv = "CLAUDE_CODE_AUTO_MODE_SERVER"

// The variables that put Claude Code on a third-party platform provider
// pointed at prism's router, with no Anthropic credential involved.
//
// This is the configuration a Teleport beam ships with — the same
// provider against the same cluster gateway — which is why prism uses
// it rather than inventing one. On the wire it is the plain Messages API
// (`POST /v1/messages?beta=true`, `model` in the body), so prism's
// router, scrubbing and usage capture need no special case. The one
// thing that matters is that the provider isn't first-party: a sign-in
// pin is only ever checked against first-party OAuth.
//
// The skip-auth switch comes along because the beam sets it and the
// gateway authenticates cluster-side anyway. Without it a request never
// came back at all (no answer in five minutes against a gateway that
// otherwise replies in seconds), so treat it as required rather than
// decorative.
const (
	claudePlatformEnv         = "CLAUDE_CODE_USE_MANTLE"
	claudePlatformSkipAuthEnv = "CLAUDE_CODE_SKIP_MANTLE_AUTH"
	claudePlatformBaseURLEnv  = "ANTHROPIC_BEDROCK_MANTLE_BASE_URL"
)

func hasEnv(environ []string, key string) bool {
	for _, kv := range environ {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}
