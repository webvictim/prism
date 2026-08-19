package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	in := &Config{
		Proxy:                  "teleport.example.com:443",
		Identity:               "tbot",
		TbotDir:                "/var/lib/prism-tbot",
		ClaudeForwardProxyMode: true,
	}
	if err := Save(in); err != nil {
		t.Fatal(err)
	}

	out, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if *out != *in {
		t.Errorf("round trip mismatch:\n got: %+v\nwant: %+v", out, in)
	}
}

func TestLoadMissingReturnsZeroConfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || *c != (Config{}) {
		t.Errorf("Load = %+v, want zero config for missing file", c)
	}
}

func TestLoadLegacyTbotDirKey(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	dir := filepath.Join(tmp, "prism")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"tbot_dir":"/legacy/path"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.TbotDir != "/legacy/path" {
		t.Errorf("TbotDir = %q, want legacy key honoured", c.TbotDir)
	}

	// Current key wins over legacy.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"tbot.dir":"/new/path","tbot_dir":"/legacy/path"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.TbotDir != "/new/path" {
		t.Errorf("TbotDir = %q, want current key to win", c.TbotDir)
	}
}
