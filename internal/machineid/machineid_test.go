package machineid

import (
	"runtime"
	"testing"
)

func TestParseIOPlatformUUID(t *testing.T) {
	// Trimmed from real `ioreg -rd1 -c IOPlatformExpertDevice` output.
	const out = `+-o J316sAP  <class IOPlatformExpertDevice, id 0x100000225, registered>
    {
      "IOPolledInterface" = "AppleARMWatchdogTimerHibernateHandler is not serializable"
      "IOPlatformSerialNumber" = "X7YQ2N8KLM"
      "IOPlatformUUID" = "6E1B2C3D-4F5A-5B6C-7D8E-9F0A1B2C3D4E"
      "platform-name" = <"t6020">
    }
`
	if got, want := parseIOPlatformUUID(out), "6E1B2C3D-4F5A-5B6C-7D8E-9F0A1B2C3D4E"; got != want {
		t.Errorf("parseIOPlatformUUID = %q, want %q", got, want)
	}

	// The serial number sits on an adjacent line and must not be
	// mistaken for the UUID.
	for _, bad := range []string{
		"",
		`      "IOPlatformSerialNumber" = "X7YQ2N8KLM"`,
		`      "IOPlatformUUID" = `,
		`      "IOPlatformUUID" = "`,
		`      "IOPlatformUUID" = ""`,
		`      "IOPlatformUUID" = "   "`,
	} {
		if got := parseIOPlatformUUID(bad); got != "" {
			t.Errorf("parseIOPlatformUUID(%q) = %q, want \"\"", bad, got)
		}
	}
}

// Current has to work on the machine running the tests, on the two
// platforms prism supports tbot on. Nothing asserts the value — only
// that there is one, and that it doesn't change between calls.
func TestCurrent(t *testing.T) {
	id, err := Current()
	switch runtime.GOOS {
	case "darwin", "linux":
		if err != nil {
			t.Skipf("no machine id available here: %v", err)
		}
		if id == "" {
			t.Fatal("Current returned an empty id with no error")
		}
		again, err := Current()
		if err != nil || again != id {
			t.Errorf("Current is not stable: %q then %q (err %v)", id, again, err)
		}
	default:
		if err == nil {
			t.Errorf("Current() = %q on %s, want an error", id, runtime.GOOS)
		}
	}
}
