package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/gravitational/prism/internal/state"
	"github.com/gravitational/prism/internal/tbot"
	"github.com/gravitational/prism/internal/tshwrap"
)

func cmdStatus(_ []string) error {
	s, err := state.Load()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Println("prism: no active session")
		return nil
	}
	logPath, _ := state.DaemonLogPath()
	statePath, _ := state.Path()

	alive := "DEAD"
	if s.DaemonPID != 0 && processAlive(s.DaemonPID) {
		alive = fmt.Sprintf("alive (pid %d)", s.DaemonPID)
	}

	fmt.Fprintln(os.Stdout, "prism status")
	fmt.Fprintln(os.Stdout, "------------")
	fmt.Fprintf(os.Stdout, "Router:      127.0.0.1:%d  [%s]\n", s.LocalPort, alive)
	fmt.Fprintf(os.Stdout, "Health:      http://127.0.0.1:%d/_prism/health\n", s.LocalPort)
	fmt.Fprintf(os.Stdout, "Anthropic:   127.0.0.1:%d (app=%s)\n", s.AnthropicPort, "anthropic")
	fmt.Fprintf(os.Stdout, "OpenAI:      127.0.0.1:%d (app=%s)\n", s.OpenAIPort, "openai")
	if s.TbotDiagPort != 0 {
		fmt.Fprintf(os.Stdout, "Bot diag:    127.0.0.1:%d\n", s.TbotDiagPort)
	}
	if s.Identity() == state.IdentitySourceTbot {
		fmt.Fprintf(os.Stdout, "Identity:    tbot (managed, dir=%s)\n", s.TbotDir)
		fmt.Fprintf(os.Stdout, "Bot health:  %s\n", tbotHealthLine(s.TbotDiagPort))
	} else {
		fmt.Fprintf(os.Stdout, "Identity:    %s\n", identityLine())
	}
	fmt.Fprintf(os.Stdout, "Created:     %s (%s ago)\n", s.CreatedAt.Local().Format(time.RFC3339), time.Since(s.CreatedAt).Round(time.Second))
	fmt.Fprintf(os.Stdout, "State file:  %s\n", statePath)
	fmt.Fprintf(os.Stdout, "Daemon log:  %s\n", logPath)
	return nil
}

func tbotHealthLine(diagPort int) string {
	if diagPort == 0 {
		return "unknown (no --diag-addr)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h := tbot.ProbeDiag(ctx, diagPort)
	switch {
	case !h.Reachable:
		return fmt.Sprintf("unreachable on 127.0.0.1:%d (%v)", diagPort, h.Err)
	case !h.Live:
		return fmt.Sprintf("LIVE FAIL on 127.0.0.1:%d/livez", diagPort)
	case !h.Ready:
		return fmt.Sprintf("not ready: %s", h.ReadyBody)
	default:
		return fmt.Sprintf("ok (live + ready on 127.0.0.1:%d)", diagPort)
	}
}

func identityLine() string {
	st, err := tshwrap.StatusJSON()
	if err != nil {
		return fmt.Sprintf("unknown (tsh status failed: %v)", err)
	}
	if st.IsExpired() {
		return fmt.Sprintf("EXPIRED at %s — run `tsh login`", st.ValidUntil.Local().Format(time.RFC3339))
	}
	remaining := time.Until(st.ValidUntil).Round(time.Minute)
	return fmt.Sprintf("ok as %s on %s (expires in %s)", st.Username, st.Cluster, remaining)
}
