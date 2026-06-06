//go:build !linux && !darwin

package main

import "fmt"

func isServiceManaged() bool { return false }

func serviceStart() error { return fmt.Errorf("service management not available on this platform") }

func serviceStop() error { return fmt.Errorf("service management not available on this platform") }

func serviceIsActive() bool { return false }

func journalFollow() error { return fmt.Errorf("journal not available on this platform") }

func cmdInstall(_ []string) error {
	return fmt.Errorf("service installation is only supported on Linux (systemd) and macOS (LaunchAgent)")
}

func cmdUninstall(_ []string) error {
	return fmt.Errorf("service installation is only supported on Linux (systemd) and macOS (LaunchAgent)")
}

func serviceManagedLabel() string { return "" }
