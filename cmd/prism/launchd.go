//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/webvictim/prism/internal/logfile"
	"github.com/webvictim/prism/internal/state"
)

const (
	plistLabel = "com.prism.daemon"
	plistName  = "com.prism.daemon.plist"
)

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", plistName), nil
}

func domainTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func serviceTarget() string {
	return domainTarget() + "/" + plistLabel
}

func isServiceManaged() bool {
	p, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// serviceStale reports whether the installed plist still runs this
// binary by a path that will keep working, and still points at the
// crash log; see service_path.go.
func serviceStale() serviceStaleness {
	p, err := plistPath()
	if err != nil {
		return serviceStaleness{}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return serviceStaleness{}
	}
	want, err := serviceProgramPath()
	if err != nil {
		return serviceStaleness{}
	}
	if s := programPathStale(plistProgram(string(data)), want); s.stale() {
		return s
	}
	logDir, err := state.DaemonLogDir()
	if err != nil {
		return serviceStaleness{}
	}
	if !strings.Contains(string(data), filepath.Join(logDir, "crash.log")) {
		return serviceStaleness{Reason: "no crash log path recorded"}
	}
	return serviceStaleness{}
}

func serviceStart() error {
	p, err := plistPath()
	if err != nil {
		return err
	}
	// launchd keeps the copy of the plist it loaded, and refuses to
	// bootstrap a label it already holds — including a job it has given
	// up on. Unloading first is what makes a repaired plist take
	// effect; "not loaded" is the normal case and not an error here.
	_ = exec.Command("launchctl", "bootout", serviceTarget()).Run()
	return exec.Command("launchctl", "bootstrap", domainTarget(), p).Run()
}

func serviceStop() error {
	return exec.Command("launchctl", "bootout", serviceTarget()).Run()
}

func serviceIsActive() bool {
	return exec.Command("launchctl", "print", serviceTarget()).Run() == nil
}

func journalFollow() error {
	logDir, err := state.DaemonLogDir()
	if err != nil {
		return err
	}
	return tailFollow(logfile.LatestPath(logDir), os.Stdout)
}

// plistFile renders the LaunchAgent for a prism binary at program.
// Separate from cmdInstall so a test can read the program back out of
// it with plistProgram, the way serviceStale does.
func plistFile(program, pathEnv, crashLog string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>%s</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, plistLabel, program, daemonArg, pathEnv, crashLog, crashLog)
}

func cmdInstall(args []string) error {
	if wantsHelp(args) {
		return printCommandHelp("install")
	}
	self, err := serviceProgramPath()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}

	logDir, err := state.DaemonLogDir()
	if err != nil {
		return err
	}
	crashLog := filepath.Join(logDir, "crash.log")

	pathEnv := os.Getenv("PATH")

	plist := plistFile(self, pathEnv, crashLog)

	p, err := plistPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := os.WriteFile(p, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	fmt.Fprintf(os.Stderr, "prism: wrote %s\n", p)
	fmt.Fprintln(os.Stderr, "prism: installed LaunchAgent (will restart on crash and start on login)")
	fmt.Fprintln(os.Stderr, "\nNext: prism up")
	return nil
}

func cmdUninstall(args []string) error {
	if wantsHelp(args) {
		return printCommandHelp("uninstall")
	}
	if !isServiceManaged() {
		return fmt.Errorf("no LaunchAgent installed — nothing to uninstall")
	}

	if serviceIsActive() {
		fmt.Fprintln(os.Stderr, "prism: stopping LaunchAgent…")
		_ = exec.Command("launchctl", "bootout", serviceTarget()).Run()
	}

	p, err := plistPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", p, err)
	}

	fmt.Fprintf(os.Stderr, "prism: removed %s\n", p)
	fmt.Fprintln(os.Stderr, "prism: `prism up` will use fork-exec mode from now on")
	return nil
}

func serviceManagedLabel() string { return "launchd-managed" }
