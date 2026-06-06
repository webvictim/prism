//go:build !linux

package main

import "fmt"

func isServiceManaged() bool { return false }

func serviceStart() error { return fmt.Errorf("service management not available on this platform") }

func serviceStop() error { return fmt.Errorf("service management not available on this platform") }

func serviceIsActive() bool { return false }

func cmdInstall(_ []string) error {
	return fmt.Errorf("service installation is currently only supported on Linux (systemd)")
}

func cmdUninstall(_ []string) error {
	return fmt.Errorf("service installation is currently only supported on Linux (systemd)")
}
