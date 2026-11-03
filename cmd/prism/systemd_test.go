//go:build linux

package main

import "testing"

// The unit renderer and the reader serviceStale uses have to agree: if
// they drift, prism either rewrites a current unit on every `prism up`
// or never notices a stale one.
func TestUnitFileProgramRoundTrip(t *testing.T) {
	const program = "/home/linuxbrew/.linuxbrew/opt/prism/bin/prism"
	unit := unitFile(program)
	if got := execStartProgram(unit); got != program {
		t.Errorf("execStartProgram(unitFile(...)) = %q, want %q", got, program)
	}
	if s := programPathStale(execStartProgram(unit), program); s.stale() {
		t.Errorf("freshly rendered unit reads as stale: %s", s.Reason)
	}
}
