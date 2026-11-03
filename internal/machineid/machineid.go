// Package machineid reads a stable identifier for the physical machine,
// used to tell "this is the same computer" from "this is a copy of
// someone else's computer".
//
// prism needs that distinction because its tbot identity is a Teleport
// bound keypair on disk. Clone a Mac with Migration Assistant and the
// clone inherits the keypair, the sidecar naming `prism-bot-<oldhost>`
// and a LaunchAgent that starts at login — so two machines renew one
// bound-keypair join state, which can lock the bot and break prism on
// the original machine. A hostname tells you nothing here: the clone
// answers to the old name until it is renamed, and may keep it forever.
//
// macOS: IOPlatformUUID, from the IORegistry. It is derived from the
// hardware and Migration Assistant does not carry it over.
//
// Linux: /etc/machine-id, falling back to /var/lib/dbus/machine-id.
// **This catches less than the macOS source does**: machine-id is
// generated at first boot and a disk or VM image clone copies it
// verbatim — systemd's own guidance is that golden images ship the file
// empty so it is regenerated. So it catches a filesystem copy onto a
// machine that has booted before, and misses a cloned disk image. The
// hardware equivalent, /sys/class/dmi/id/product_uuid, is root-only on
// most distributions, and prism runs as the user.
package machineid

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Current returns this machine's identifier, or an error when the
// platform has no readable one. Callers treat an error as "can't tell"
// and must not block the user on it.
func Current() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return "", fmt.Errorf("ioreg: %w", err)
		}
		id := parseIOPlatformUUID(string(out))
		if id == "" {
			return "", fmt.Errorf("ioreg output carried no IOPlatformUUID")
		}
		return id, nil
	case "linux":
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			b, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if id := strings.TrimSpace(string(b)); id != "" {
				return id, nil
			}
		}
		return "", fmt.Errorf("no machine id in /etc/machine-id or /var/lib/dbus/machine-id")
	default:
		return "", fmt.Errorf("no machine id source on %s", runtime.GOOS)
	}
}

// parseIOPlatformUUID pulls the UUID out of `ioreg -rd1 -c
// IOPlatformExpertDevice`, whose line reads:
//
//	"IOPlatformUUID" = "6E1B2C3D-4F5A-5B6C-7D8E-9F0A1B2C3D4E"
//
// Parsed rather than shelled out through grep/awk so the whole thing
// stays one exec and is testable without a Mac.
func parseIOPlatformUUID(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		_, after, found := strings.Cut(line, `"IOPlatformUUID"`)
		if !found {
			continue
		}
		_, after, found = strings.Cut(after, `"`)
		if !found {
			continue
		}
		id, _, found := strings.Cut(after, `"`)
		if !found {
			continue
		}
		if id = strings.TrimSpace(id); id != "" {
			return id
		}
	}
	return ""
}
