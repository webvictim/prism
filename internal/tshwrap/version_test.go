//go:build !windows

package tshwrap

import (
	"os"
	"path/filepath"
	"testing"
)

// A fake tsh that behaves like one under managed updates: 18.8.1 runs,
// unless TELEPORT_TOOLS_VERSION=off lets the installed 18.11.9 answer.
func TestClientVersionsSeesManagedUpdates(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in *"version --client --format=json"*) ;; *) exit 2 ;; esac
if [ "$TELEPORT_TOOLS_VERSION" = off ]; then v=18.11.9; else v=18.8.1; fi
printf '{"version":"%s","gitref":"v%s","runtime":"go"}\n' "$v" "$v"
`
	if err := os.WriteFile(filepath.Join(dir, "tsh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TELEPORT_TOOLS_VERSION", "")
	SetProxy("proxy.example:443")
	defer SetProxy("")

	v, err := ClientVersions()
	if err != nil {
		t.Fatal(err)
	}
	if v.Ran != "18.8.1" || v.Installed != "18.11.9" || !v.Reexecuted() {
		t.Errorf("got %+v", v)
	}
}
