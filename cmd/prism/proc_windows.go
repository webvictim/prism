//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// Windows process-creation flags (from the Win32 API). Defined here to
// avoid pulling in golang.org/x/sys/windows just for two constants.
const (
	detachedProcess         = 0x00000008
	createNewProcessGroup   = 0x00000200
	createBreakawayFromJob  = 0x01000000
)

// setDetachedAttrs configures cmd so the spawned process survives the parent.
// On Windows: detach from any console, start a new process group so Ctrl-C
// in the parent's console doesn't propagate, and break away from any job
// the parent might belong to (so the daemon outlives e.g. a CI runner job).
func setDetachedAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob,
		HideWindow:    true,
	}
}

// stillActive is the Win32 STILL_ACTIVE exit-code sentinel returned by
// GetExitCodeProcess for a process that hasn't exited yet.
const stillActive = 259

// processAlive returns true if a process with the given PID still exists.
// On Windows we can't reuse the Unix Signal(0) trick — Process.Signal on
// Windows returns "not supported" for any signal that isn't Interrupt /
// Kill, so it would always report "dead." Use the Win32 native path:
// open a handle with QUERY_INFORMATION and check the exit code; STILL_ACTIVE
// (259) means it's still running.
func processAlive(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// signalShutdown asks the process to terminate cleanly. Windows has no
// SIGTERM, so we use Process.Kill — there's no graceful shutdown signal
// short of injecting a Ctrl-C into the process group, which is out of
// scope for prism's daemon. The daemon doesn't need to flush any state
// that isn't already on disk.
func signalShutdown(p *os.Process) error {
	return p.Kill()
}

// signalKill force-terminates the process.
func signalKill(p *os.Process) error {
	return p.Kill()
}

// forwardedSignals lists the signals cmdClaude forwards to the wrapped
// `claude` child. On Windows os.Interrupt is the moral equivalent of
// SIGINT — it's what Ctrl-C delivers when the console runtime translates
// CTRL_C_EVENT into a Go signal.
func forwardedSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
