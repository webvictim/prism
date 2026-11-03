package tbot

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestNamesForHost(t *testing.T) {
	for _, tt := range []struct {
		hostname string
		want     Names
	}{
		{"athena", Names{"prism-bot-token-athena", "prism-bot-role-athena", "prism-bot-athena"}},
		// A Mac reports itself as Gaia.local; the names must match the
		// ones derived from the bare hostname.
		{"Gaia.local", Names{"prism-bot-token-gaia", "prism-bot-role-gaia", "prism-bot-gaia"}},
		{"  gaia.example.com  ", Names{"prism-bot-token-gaia", "prism-bot-role-gaia", "prism-bot-gaia"}},
		{"", Names{"prism-bot-token-unknown", "prism-bot-role-unknown", "prism-bot-unknown"}},
	} {
		if got := NamesForHost(tt.hostname); got != tt.want {
			t.Errorf("NamesForHost(%q) = %+v, want %+v", tt.hostname, got, tt.want)
		}
	}
	if got := NamesForHost("gaia").Host(); got != "gaia" {
		t.Errorf("Host() = %q, want gaia", got)
	}
}

func TestCheckMachine(t *testing.T) {
	write := func(t *testing.T, id string) string {
		t.Helper()
		dir := t.TempDir()
		if err := SaveSidecar(dir, &SidecarConfig{TokenName: "prism-bot-token-athena", MachineID: id}); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("same machine", func(t *testing.T) {
		if err := CheckMachine(write(t, "MACHINE-A"), "MACHINE-A"); err != nil {
			t.Errorf("CheckMachine = %v, want nil", err)
		}
	})

	t.Run("cloned machine", func(t *testing.T) {
		dir := write(t, "MACHINE-A")
		err := CheckMachine(dir, "MACHINE-B")
		var foreign *ForeignMachineError
		if !errors.As(err, &foreign) {
			t.Fatalf("CheckMachine = %v, want a *ForeignMachineError", err)
		}
		if foreign.Recorded != "MACHINE-A" || foreign.Current != "MACHINE-B" {
			t.Errorf("error carries %+v", foreign)
		}
		// The message is read by someone who has no idea what a bound
		// keypair is, so it has to name the way out.
		if msg := foreign.Error(); !strings.Contains(msg, "prism tbot reset") || !strings.Contains(msg, dir) {
			t.Errorf("error message is not actionable: %s", msg)
		}
	})

	t.Run("adopts an unrecorded machine", func(t *testing.T) {
		dir := write(t, "")
		if err := CheckMachine(dir, "MACHINE-A"); err != nil {
			t.Fatalf("CheckMachine = %v, want nil", err)
		}
		sidecar, err := LoadSidecar(dir)
		if err != nil {
			t.Fatal(err)
		}
		if sidecar.MachineID != "MACHINE-A" {
			t.Errorf("machine_id = %q, want it recorded", sidecar.MachineID)
		}
		// Recording it must not disturb the rest of the sidecar.
		if sidecar.TokenName != "prism-bot-token-athena" {
			t.Errorf("token_name = %q, want it preserved", sidecar.TokenName)
		}
	})

	t.Run("unknown current machine is not a clone", func(t *testing.T) {
		if err := CheckMachine(write(t, "MACHINE-A"), ""); err != nil {
			t.Errorf("CheckMachine = %v, want nil when this machine has no id", err)
		}
	})

	t.Run("missing sidecar is not a clone", func(t *testing.T) {
		if err := CheckMachine(filepath.Join(t.TempDir(), "absent"), "MACHINE-A"); err != nil {
			t.Errorf("CheckMachine = %v, want nil", err)
		}
	})
}
