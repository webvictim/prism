package main

import (
	"fmt"
	"os"
	"time"

	"github.com/gravitational/prism/internal/router"
	"github.com/gravitational/prism/internal/state"
	"github.com/gravitational/prism/internal/tshwrap"
)

func cmdDown(_ []string) error {
	s, err := state.Load()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Fprintln(os.Stderr, "prism: no active session")
		return nil
	}

	if s.DaemonPID != 0 {
		if p, err := os.FindProcess(s.DaemonPID); err == nil {
			if perr := signalShutdown(p); perr == nil {
				fmt.Fprintf(os.Stderr, "prism: stopping daemon pid %d…\n", s.DaemonPID)
				waitForExit(s.DaemonPID, 3*time.Second)
			}
		}
	}

	_ = tshwrap.AppLogout(router.AnthropicAppName)
	_ = tshwrap.AppLogout(router.OpenAIAppName)

	if err := state.Delete(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "prism: down.")
	return nil
}

func waitForExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = signalKill(p)
	}
}
