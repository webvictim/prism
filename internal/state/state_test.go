package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	in := &State{
		LocalPort:      7331,
		AnthropicPort:  7333,
		OpenAIPort:     7334,
		IdentitySource: IdentitySourceTbot,
		DaemonPID:      1234,
		Debug:          true,
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}

	out, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("Load returned nil after Save")
	}
	if *out != *in {
		t.Errorf("round trip mismatch:\n got: %+v\nwant: %+v", out, in)
	}
}

func TestLoadMissingReturnsNil(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Errorf("Load = %+v, want nil for missing state file", s)
	}
}

func TestDelete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := Save(&State{LocalPort: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Delete(); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil || s != nil {
		t.Errorf("after Delete: state=%+v err=%v, want nil/nil", s, err)
	}
	// Deleting again is not an error.
	if err := Delete(); err != nil {
		t.Errorf("second Delete errored: %v", err)
	}
}

func TestIdentityDefault(t *testing.T) {
	var s *State
	if got := s.Identity(); got != IdentitySourceTSH {
		t.Errorf("nil state Identity = %q, want tsh", got)
	}
	if got := (&State{}).Identity(); got != IdentitySourceTSH {
		t.Errorf("empty Identity = %q, want tsh", got)
	}
	if got := (&State{IdentitySource: IdentitySourceTbot}).Identity(); got != IdentitySourceTbot {
		t.Errorf("Identity = %q, want tbot", got)
	}
}

func TestIsLegacyState(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	if IsLegacyState() {
		t.Error("missing state file reported as legacy")
	}

	dir := filepath.Join(tmp, "prism")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(`{"local_port":7331}`)
	if IsLegacyState() {
		t.Error("current-format state reported as legacy")
	}

	write(`{"local_port":7331,"beam_id":"beam-123"}`)
	if !IsLegacyState() {
		t.Error("beam_id state not reported as legacy")
	}
}
