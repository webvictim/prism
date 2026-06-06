//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const unitName = "prism.service"

func unitDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func unitPath() (string, error) {
	dir, err := unitDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, unitName), nil
}

func isServiceManaged() bool {
	p, err := unitPath()
	if err != nil {
		return false
	}
	if _, err := os.Stat(p); err != nil {
		return false
	}
	return checkUserBus() == nil
}

func serviceStart() error {
	return exec.Command("systemctl", "--user", "start", unitName).Run()
}

func serviceStop() error {
	return exec.Command("systemctl", "--user", "stop", unitName).Run()
}

func serviceIsActive() bool {
	return exec.Command("systemctl", "--user", "is-active", "--quiet", unitName).Run() == nil
}

func cmdInstall(_ []string) error {
	if err := checkUserBus(); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return fmt.Errorf("resolve symlinks: %w", err)
	}

	unit := fmt.Sprintf(`[Unit]
Description=Prism — local AI router via Teleport
After=network-online.target

[Service]
Type=simple
ExecStart=%s __daemon
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, self)

	dir, err := unitDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	p := filepath.Join(dir, unitName)
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", p, err)
	}
	fmt.Fprintf(os.Stderr, "prism: wrote %s\n", p)

	if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %w", err)
	}
	if err := exec.Command("systemctl", "--user", "enable", unitName).Run(); err != nil {
		return fmt.Errorf("systemctl enable: %w", err)
	}
	fmt.Fprintln(os.Stderr, "prism: enabled prism.service")

	// Enable lingering so the user service survives logout.
	if lingerEnabled() {
		fmt.Fprintln(os.Stderr, "prism: lingering already enabled (service survives logout)")
	} else if err := exec.Command("loginctl", "enable-linger").Run(); err != nil {
		fmt.Fprintf(os.Stderr, "prism: warning: could not enable lingering (run `sudo loginctl enable-linger %s`)\n", os.Getenv("USER"))
		fmt.Fprintln(os.Stderr, "  (without lingering, the service will stop when you log out)")
	} else {
		fmt.Fprintln(os.Stderr, "prism: enabled lingering (service survives logout)")
	}

	fmt.Fprintln(os.Stderr, "\nNext: prism up")
	return nil
}

func cmdUninstall(_ []string) error {
	if !isServiceManaged() {
		return fmt.Errorf("no systemd unit installed — nothing to uninstall")
	}

	if serviceIsActive() {
		fmt.Fprintln(os.Stderr, "prism: stopping service…")
		_ = exec.Command("systemctl", "--user", "stop", unitName).Run()
	}
	_ = exec.Command("systemctl", "--user", "disable", unitName).Run()

	p, err := unitPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", p, err)
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()

	fmt.Fprintf(os.Stderr, "prism: removed %s and disabled service\n", p)
	fmt.Fprintln(os.Stderr, "prism: `prism up` will use fork-exec mode from now on")
	return nil
}

func checkUserBus() error {
	if err := exec.Command("systemctl", "--user", "status").Run(); err != nil {
		return fmt.Errorf("systemd user bus not available (is XDG_RUNTIME_DIR set? try connecting via regular ssh)\n\n  XDG_RUNTIME_DIR=%s", os.Getenv("XDG_RUNTIME_DIR"))
	}
	return nil
}

func lingerEnabled() bool {
	uid := fmt.Sprint(os.Getuid())
	_, err := os.Stat(filepath.Join("/var/lib/systemd/linger", os.Getenv("USER")))
	if err == nil {
		return true
	}
	// Fallback: check via loginctl show-user
	out, err := exec.Command("loginctl", "show-user", uid, "--property=Linger").Output()
	if err != nil {
		return false
	}
	return string(out) == "Linger=yes\n"
}
