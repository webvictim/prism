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

func plistIsStale() bool {
	p, err := plistPath()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return false
	}
	content := string(data)
	if !strings.Contains(content, self) {
		return true
	}
	logDir, _ := state.DaemonLogDir()
	crashLog := filepath.Join(logDir, "crash.log")
	if !strings.Contains(content, crashLog) {
		return true
	}
	return false
}

func serviceStart() error {
	p, err := plistPath()
	if err != nil {
		return err
	}
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

func cmdInstall(_ []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	logDir, err := state.DaemonLogDir()
	if err != nil {
		return err
	}
	crashLog := filepath.Join(logDir, "crash.log")

	pathEnv := os.Getenv("PATH")

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>__daemon</string>
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
`, plistLabel, self, pathEnv, crashLog, crashLog)

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

func cmdUninstall(_ []string) error {
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
