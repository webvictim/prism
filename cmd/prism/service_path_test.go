package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The breakage this guards against: Homebrew's Cellar path carries the
// version, so the path a service definition records stops existing at
// the next upgrade.
func TestBrewOptPath(t *testing.T) {
	for _, tt := range []struct {
		name     string
		resolved string
		want     string
	}{
		{
			name:     "linuxbrew",
			resolved: "/home/linuxbrew/.linuxbrew/Cellar/prism/0.1.21/bin/prism",
			want:     "/home/linuxbrew/.linuxbrew/opt/prism/bin/prism",
		},
		{
			name:     "apple silicon",
			resolved: "/opt/homebrew/Cellar/prism/0.1.28/bin/prism",
			want:     "/opt/homebrew/opt/prism/bin/prism",
		},
		{
			name:     "intel mac",
			resolved: "/usr/local/Cellar/prism/0.1.28/bin/prism",
			want:     "/usr/local/opt/prism/bin/prism",
		},
		{
			name:     "not homebrew",
			resolved: "/usr/local/bin/prism",
			want:     "",
		},
		{
			name:     "cellar with nothing under the version",
			resolved: "/opt/homebrew/Cellar/prism/0.1.28",
			want:     "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := brewOptPath(tt.resolved); got != tt.want {
				t.Errorf("brewOptPath(%q) = %q, want %q", tt.resolved, got, tt.want)
			}
		})
	}
}

func TestVersionedPath(t *testing.T) {
	versioned := []string{
		"/home/linuxbrew/.linuxbrew/Cellar/prism/0.1.21/bin/prism",
		"/opt/homebrew/Cellar/prism/0.1.28/bin/prism",
		"/home/gus/.local/share/mise/installs/prism/1.2.3/bin/prism",
		"/opt/prism/v2.0/bin/prism",
	}
	for _, p := range versioned {
		if !versionedPath(p) {
			t.Errorf("versionedPath(%q) = false, want true", p)
		}
	}
	stable := []string{
		"/usr/local/bin/prism",
		"/opt/homebrew/opt/prism/bin/prism",
		"/home/linuxbrew/.linuxbrew/bin/prism",
		"/home/gus/git/prism/bin/prism",
	}
	for _, p := range stable {
		if versionedPath(p) {
			t.Errorf("versionedPath(%q) = true, want false", p)
		}
	}
}

// writeBinary creates an executable file, and link creates a symlink to
// it, standing in for Homebrew's keg and its bin/ or opt/ symlink.
func writeBinary(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func link(t *testing.T, target, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStableProgramPath(t *testing.T) {
	dir := t.TempDir()
	keg := writeBinary(t, filepath.Join(dir, "Cellar", "prism", "0.1.21", "bin", "prism"))
	opt := link(t, keg, filepath.Join(dir, "opt", "prism", "bin", "prism"))
	binLink := link(t, keg, filepath.Join(dir, "bin", "prism"))
	other := writeBinary(t, filepath.Join(dir, "elsewhere", "prism"))

	// A versioned candidate is never the answer, even though it is the
	// one that really holds the binary.
	if got := stableProgramPath(keg, []string{keg, opt}); got != opt {
		t.Errorf("stableProgramPath = %q, want the opt link %q", got, opt)
	}
	// First match wins, so the candidate order is the preference order.
	if got := stableProgramPath(keg, []string{opt, binLink}); got != opt {
		t.Errorf("stableProgramPath = %q, want %q", got, opt)
	}
	if got := stableProgramPath(keg, []string{binLink, opt}); got != binLink {
		t.Errorf("stableProgramPath = %q, want %q", got, binLink)
	}
	// A different prism on PATH must not be mistaken for this one, and
	// with nothing pointing at the keg there is no stable path at all.
	if got := stableProgramPath(keg, []string{other, filepath.Join(dir, "absent", "prism"), ""}); got != "" {
		t.Errorf("stableProgramPath = %q, want \"\" (no stable path)", got)
	}
}

// serviceProgramPath must prefer a stable spelling of the running
// binary, and must be deterministic: a path that changed between
// invocations would make `prism up` rewrite the service definition
// every time.
func TestProgramPathCandidatesAreDeterministic(t *testing.T) {
	const resolved = "/opt/homebrew/Cellar/prism/0.1.28/bin/prism"
	first := programPathCandidates(resolved)
	if len(first) == 0 || first[0] != "/opt/homebrew/opt/prism/bin/prism" {
		t.Fatalf("candidates = %v, want the opt path first", first)
	}
	t.Setenv("PATH", "/nonexistent")
	second := programPathCandidates(resolved)
	// Only the PATH lookup may differ, and it is last.
	if len(second) == 0 {
		t.Fatal("no candidates")
	}
	n := min(len(first), len(second))
	for i := range n - 1 {
		if first[i] != second[i] {
			t.Errorf("candidate %d differs between calls: %q vs %q", i, first[i], second[i])
		}
	}
}

func TestProgramPathStale(t *testing.T) {
	dir := t.TempDir()
	keg := writeBinary(t, filepath.Join(dir, "Cellar", "prism", "0.1.21", "bin", "prism"))
	opt := link(t, keg, filepath.Join(dir, "opt", "prism", "bin", "prism"))
	binLink := link(t, keg, filepath.Join(dir, "bin", "prism"))
	other := writeBinary(t, filepath.Join(dir, "elsewhere", "prism"))
	gone := filepath.Join(dir, "Cellar", "prism", "0.1.20", "bin", "prism")

	for _, tt := range []struct {
		name      string
		installed string
		want      string
		contains  string
		breaks    bool
	}{
		{name: "current", installed: opt, want: opt},
		{
			// The paste that started this: ExecStart points into a keg
			// a `brew upgrade` deleted.
			name: "upgraded away", installed: gone, want: opt,
			contains: "no longer exists", breaks: true,
		},
		{
			name: "empty", installed: "", want: opt,
			contains: "no program path", breaks: true,
		},
		{
			// Works today, will not survive the next upgrade.
			name: "versioned but working", installed: keg, want: opt,
			contains: "names a version", breaks: true,
		},
		{
			// Nothing stable to move to: leaving it alone beats
			// rewriting the same path on every invocation.
			name: "versioned with no stable alternative", installed: keg, want: keg,
		},
		{
			// Another spelling of the same binary, neither versioned.
			name: "equivalent path", installed: binLink, want: opt,
		},
		{
			// A real mismatch, but one that keeps working, so it is not
			// repaired behind the user's back mid-session.
			name: "different binary", installed: other, want: opt,
			contains: "runs " + other,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := programPathStale(tt.installed, tt.want)
			if (tt.contains == "") != (got.Reason == "") {
				t.Fatalf("Reason = %q, want contains %q", got.Reason, tt.contains)
			}
			if tt.contains != "" && !strings.Contains(got.Reason, tt.contains) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, tt.contains)
			}
			if got.Breaks != tt.breaks {
				t.Errorf("Breaks = %v, want %v", got.Breaks, tt.breaks)
			}
			if got.stale() != (tt.contains != "") {
				t.Errorf("stale() = %v for Reason %q", got.stale(), got.Reason)
			}
		})
	}
}

func TestExecStartProgram(t *testing.T) {
	for _, tt := range []struct {
		name string
		unit string
		want string
	}{
		{
			name: "prism's own unit",
			unit: "[Service]\nType=simple\nExecStart=/usr/local/bin/prism " + daemonArg + "\nRestart=on-failure\n",
			want: "/usr/local/bin/prism",
		},
		{
			name: "no arguments",
			unit: "[Service]\nExecStart=/usr/local/bin/prism\n",
			want: "/usr/local/bin/prism",
		},
		{
			name: "systemd's ignore-failure prefix",
			unit: "[Service]\nExecStart=-/usr/local/bin/prism " + daemonArg + "\n",
			want: "/usr/local/bin/prism",
		},
		{
			// Not a shape prism writes, but systemd's own parse of it:
			// the command ends at the first space.
			name: "other arguments",
			unit: "[Service]\nExecStart=/usr/local/bin/prism --port 7331 " + daemonArg + "\n",
			want: "/usr/local/bin/prism",
		},
		{
			name: "ExecStartPre is not ExecStart",
			unit: "[Service]\nExecStartPre=/bin/true\nExecStart=/usr/local/bin/prism " + daemonArg + "\n",
			want: "/usr/local/bin/prism",
		},
		{name: "no ExecStart at all", unit: "[Unit]\nDescription=x\n", want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := execStartProgram(tt.unit); got != tt.want {
				t.Errorf("execStartProgram = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlistProgram(t *testing.T) {
	plist := `<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.prism.daemon</string>
  <key>ProgramArguments</key>
  <array>
    <string>/opt/homebrew/bin/prism</string>
    <string>` + daemonArg + `</string>
  </array>
</dict>
</plist>`
	if got := plistProgram(plist); got != "/opt/homebrew/bin/prism" {
		t.Errorf("plistProgram = %q, want the program path", got)
	}
	// The Label's <string> comes first in the file and must not be
	// mistaken for the program.
	if got := plistProgram(strings.Replace(plist, "ProgramArguments", "Other", 1)); got != "" {
		t.Errorf("plistProgram = %q, want \"\" with no ProgramArguments", got)
	}
	if got := plistProgram(""); got != "" {
		t.Errorf("plistProgram = %q, want \"\"", got)
	}
}

// A program path containing a space is one systemd would mis-split, but
// reading it back whole is what keeps prism from deciding the binary
// has gone missing and rewriting the unit on every invocation.
func TestExecStartProgramKeepsSpacedPath(t *testing.T) {
	program := writeBinary(t, filepath.Join(t.TempDir(), "My Tools", "prism"))
	unit := "[Service]\nExecStart=" + program + " " + daemonArg + "\n"
	if got := execStartProgram(unit); got != program {
		t.Errorf("execStartProgram = %q, want %q", got, program)
	}
}
