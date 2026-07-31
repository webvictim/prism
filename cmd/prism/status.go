package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/gravitational/prism/internal/logfile"
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
	logDir, _ := state.DaemonLogDir()
	statePath, _ := state.Path()

	alive := "DEAD"
	if isServiceManaged() && serviceIsActive() {
		alive = fmt.Sprintf("alive (%s)", serviceManagedLabel())
	} else if s.DaemonPID != 0 && processAlive(s.DaemonPID) {
		alive = fmt.Sprintf("alive (pid %d)", s.DaemonPID)
	} else if portResponds(s.LocalPort) {
		alive = "alive (port responding)"
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
		if msg := tbotStaleTTLWarning(s.TbotDir); msg != "" {
			fmt.Fprintf(os.Stdout, "\n  WARNING: %s\n", msg)
		}
	} else {
		fmt.Fprintf(os.Stdout, "Identity:    %s\n", identityLine())
	}
	fmt.Fprintf(os.Stdout, "Created:     %s (%s ago)\n", s.CreatedAt.Local().Format(time.RFC3339), time.Since(s.CreatedAt).Round(time.Second))
	fmt.Fprintf(os.Stdout, "State file:  %s\n", statePath)
	if isServiceManaged() && serviceManagedLabel() == "systemd-managed" {
		fmt.Fprintln(os.Stdout, "Daemon log:  journalctl --user -u prism")
	} else {
		fmt.Fprintf(os.Stdout, "Daemon log:  %s\n", logfile.LatestPath(logDir))
	}
	return nil
}

func tbotStaleTTLWarning(tbotDir string) string {
	if tbotDir == "" {
		return ""
	}
	yamlPath := tbot.TbotYAMLPath(tbotDir)
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return ""
	}
	contents := string(data)
	if strings.Contains(contents, "renewal_interval: 23h") || strings.Contains(contents, "renewal_interval: 24h") {
		return "tbot.yaml has a renewal_interval that exceeds the server's 12h cert lifetime.\n" +
			"           Certs will expire before renewal. Run `prism down && prism up` to fix."
	}
	return ""
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
	case h.Degraded:
		return fmt.Sprintf("degraded (tunnel ok, but: %s)", h.DegradedReason)
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

func portResponds(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
