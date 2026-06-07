//go:build windows

package tshwrap

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000

// HideWindow sets the CREATE_NO_WINDOW flag so the subprocess doesn't
// allocate a visible console window. Call this on any exec.Cmd before
// Start() to prevent flash-windows on Windows.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
	cmd.SysProcAttr.HideWindow = true
}
