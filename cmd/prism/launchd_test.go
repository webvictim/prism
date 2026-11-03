//go:build darwin

package main

import "testing"

// The plist renderer and the reader serviceStale uses have to agree: if
// they drift, prism either rewrites a current plist on every `prism up`
// or never notices a stale one.
func TestPlistFileProgramRoundTrip(t *testing.T) {
	const program = "/opt/homebrew/opt/prism/bin/prism"
	plist := plistFile(program, "/usr/bin:/bin", "/tmp/crash.log")
	if got := plistProgram(plist); got != program {
		t.Errorf("plistProgram(plistFile(...)) = %q, want %q", got, program)
	}
	if s := programPathStale(plistProgram(plist), program); s.stale() {
		t.Errorf("freshly rendered plist reads as stale: %s", s.Reason)
	}
}
