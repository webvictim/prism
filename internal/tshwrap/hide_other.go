//go:build !windows

package tshwrap

import "os/exec"

// HideWindow is a no-op on non-Windows platforms.
func HideWindow(_ *exec.Cmd) {}
