//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setDetachedAttrs configures cmd so the spawned process survives the parent
// closing — used for the prism daemon. On Unix we put it in its own session.
func setDetachedAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// processAlive returns true if a process with the given PID still exists.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// signalShutdown asks the process to terminate cleanly (SIGTERM on Unix).
func signalShutdown(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// signalKill force-kills the process (SIGKILL on Unix).
func signalKill(p *os.Process) error {
	return p.Signal(syscall.SIGKILL)
}

// forwardedSignals lists the signals cmdClaude forwards to the wrapped
// `claude` child so the TUI sees Ctrl-C / termination requests.
func forwardedSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP}
}
