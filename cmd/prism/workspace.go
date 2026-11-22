package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// currentWorkspace is the repository the current directory is in — its
// git top level — or the directory itself outside one. The launchers tag
// every request with it (egress.WorkspaceHeader), and the beam commands
// name it, so `prism beam end` and `prism beam link --pr` act on this
// repository's beams.
func currentWorkspace() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	if out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return filepath.Clean(top)
		}
	}
	return filepath.Clean(dir)
}
