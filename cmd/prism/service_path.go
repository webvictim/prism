package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// daemonArg is the hidden subcommand a service definition launches.
const daemonArg = "__daemon"

// A service definition (systemd unit or launchd plist) names an
// absolute path to the prism binary, and Homebrew — how most people
// install prism — hands out a version-pinned one:
//
//	/home/linuxbrew/.linuxbrew/Cellar/prism/0.1.21/bin/prism
//
// The next `brew upgrade` deletes that directory and the definition
// keeps pointing into it. systemd reports `status=203/EXEC` in a
// restart loop until it hits the start limit; launchd simply never
// spawns the job. Either way prism looks installed and isn't.
//
// The same binary has a stable spelling that survives the upgrade —
// Homebrew's `opt` symlink, or whichever bin directory is on PATH — so
// that is what gets written. os.Executable() can't supply it: on Linux
// it reads /proc/self/exe, which the kernel has already resolved, so
// the symlink the user actually invoked is gone before prism looks.
// The stable path has to be found by searching for a launcher that
// resolves back to the running binary.

// serviceProgramPath returns the path to this binary to record in a
// service definition: a stable launcher when one points at it,
// otherwise the binary itself.
func serviceProgramPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(self)
	if err != nil {
		return "", err
	}
	if stable := stableProgramPath(resolved, programPathCandidates(resolved)); stable != "" {
		return stable, nil
	}
	return resolved, nil
}

// stableProgramPath returns the first candidate that is a version-free
// path to the binary at resolved, or "" when only the versioned path
// exists.
//
// The candidates are a fixed list consulted in a fixed order with PATH
// last, because the answer has to be the same on every invocation: a
// path that varied with the caller's environment would make
// programPathStale rewrite the service definition on every `prism up`.
func stableProgramPath(resolved string, candidates []string) string {
	for _, candidate := range candidates {
		if candidate == "" || candidate == resolved || versionedPath(candidate) {
			continue
		}
		if sameExecutable(candidate, resolved) {
			return candidate
		}
	}
	return ""
}

func programPathCandidates(resolved string) []string {
	name := filepath.Base(resolved)
	candidates := []string{brewOptPath(resolved)}
	var dirs []string
	if prefix := os.Getenv("HOMEBREW_PREFIX"); prefix != "" {
		dirs = append(dirs, filepath.Join(prefix, "bin"))
	}
	dirs = append(dirs,
		"/opt/homebrew/bin",
		"/home/linuxbrew/.linuxbrew/bin",
		"/usr/local/bin",
		"/usr/bin",
	)
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"))
	}
	for _, dir := range dirs {
		candidates = append(candidates, filepath.Join(dir, name))
	}
	// PATH last: it is the only candidate that depends on who is asking.
	if p, err := exec.LookPath(name); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			candidates = append(candidates, abs)
		}
	}
	return candidates
}

// brewOptPath maps a Homebrew Cellar path to the version-independent
// opt path for the same file:
//
//	/opt/homebrew/Cellar/prism/0.1.28/bin/prism
//	→ /opt/homebrew/opt/prism/bin/prism
//
// Homebrew maintains that symlink for every formula's current version,
// on macOS and Linuxbrew alike, which makes it a better answer than any
// bin directory: it is there even when prism isn't on PATH.
func brewOptPath(resolved string) string {
	prefix, rest, found := strings.Cut(resolved, "/Cellar/")
	if !found {
		return ""
	}
	// formula / version / …path within the keg.
	parts := strings.Split(rest, "/")
	if len(parts) < 3 || prefix == "" {
		return ""
	}
	return filepath.Join(append([]string{prefix, "opt", parts[0]}, parts[2:]...)...)
}

// sameExecutable reports whether two paths name the same file, symlinks
// followed. Compared by identity rather than by string so that /var
// vs /private/var, a bind mount or a chain of symlinks can't read as
// two different binaries.
func sameExecutable(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

var versionComponent = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)+`)

// versionedPath reports whether any component of p looks like a version
// number — the shape of a Homebrew Cellar path, and of most other
// package managers that keep one directory per installed version.
//
// A heuristic, deliberately: being wrong only ever makes prism prefer
// another spelling of the same file, or rewrite a definition it could
// have left alone. Service definitions are POSIX-only, so "/" is the
// only separator worth splitting on.
func versionedPath(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if versionComponent.MatchString(part) {
			return true
		}
	}
	return false
}

// serviceStaleness describes an installed service definition that no
// longer matches this binary.
type serviceStaleness struct {
	// Reason is empty when the definition is current.
	Reason string
	// Breaks marks a definition that will stop working on its own: the
	// recorded path is already gone, or it names a version the next
	// upgrade will delete. Those are worth repairing even while the
	// daemon is running, because nothing else will notice before the
	// next login or crash-restart. A definition that merely names a
	// different prism is not — see healServiceDefinition.
	Breaks bool
}

func (s serviceStaleness) stale() bool { return s.Reason != "" }

// programPathStale compares the program path recorded in an installed
// service definition against the one prism would write now.
func programPathStale(installed, want string) serviceStaleness {
	switch {
	case installed == "":
		return serviceStaleness{Reason: "no program path recorded", Breaks: true}
	case installed == want:
		return serviceStaleness{}
	}
	if _, err := os.Stat(installed); err != nil {
		return serviceStaleness{Reason: installed + " no longer exists", Breaks: true}
	}
	if !sameExecutable(installed, want) {
		return serviceStaleness{Reason: "runs " + installed + ", not " + want}
	}
	// The same binary under another name, which is only worth rewriting
	// when the recorded spelling carries a version an upgrade will
	// remove — and only when prism found a spelling that doesn't.
	if versionedPath(installed) && !versionedPath(want) {
		return serviceStaleness{
			Reason: installed + " names a version that an upgrade will remove",
			Breaks: true,
		}
	}
	return serviceStaleness{}
}

// execStartProgram returns the binary named by a unit file's ExecStart,
// and plistProgram the first entry of a plist's ProgramArguments.
//
// Both readers live here, outside the build tags their callers sit
// behind, so that each one still compiles and runs under test on the
// other platform.
func execStartProgram(unit string) string {
	for line := range strings.SplitSeq(unit, "\n") {
		value, ok := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !ok {
			continue
		}
		// systemd allows -, +, !, : prefixes on the command; a path
		// can't start with one.
		value = strings.TrimLeft(value, "-+!:@")
		// prism writes `<path> __daemon`, so dropping that suffix reads
		// back a path containing a space — which systemd itself would
		// mis-split, but which must at least round-trip here rather
		// than look like a path that has gone missing. Anything else
		// ends at the first space, as systemd parses it.
		if program, ok := strings.CutSuffix(value, " "+daemonArg); ok {
			if !strings.ContainsAny(program, " \t") || fileExists(program) {
				return program
			}
		}
		if i := strings.IndexAny(value, " \t"); i >= 0 {
			return value[:i]
		}
		return value
	}
	return ""
}

func plistProgram(content string) string {
	_, rest, ok := strings.Cut(content, "<key>ProgramArguments</key>")
	if !ok {
		return ""
	}
	_, rest, ok = strings.Cut(rest, "<string>")
	if !ok {
		return ""
	}
	program, _, ok := strings.Cut(rest, "</string>")
	if !ok {
		return ""
	}
	return strings.TrimSpace(program)
}

// healServiceDefinition rewrites an installed service definition whose
// recorded binary path has gone stale, and reports what it did on
// stderr: a repair the user didn't ask for is still a change to a file
// they own.
//
// startup widens it to any mismatch — prism is about to start the
// service, so it may as well point it at the binary doing the starting,
// which is also how a dev build takes over a brew-installed unit.
// Mid-session only the self-breaking cases qualify; see
// serviceStaleness.Breaks.
func healServiceDefinition(startup bool) error {
	if !isServiceManaged() {
		return nil
	}
	s := serviceStale()
	if !s.stale() || (!startup && !s.Breaks) {
		return nil
	}
	fmt.Fprintf(os.Stderr, "prism: repairing %s service definition (%s)\n", serviceManagedLabel(), s.Reason)
	if err := cmdInstall(nil); err != nil {
		return fmt.Errorf("repair service definition: %w", err)
	}
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
