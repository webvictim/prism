// Package config holds prism's persistent, machine-wide configuration —
// the bits that survive `prism down --rm` (unlike state.json, which is
// per-session and gets wiped on full teardown).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Config is the on-disk shape. Add fields here as needed; everything is
// optional, so older config files keep parsing.
type Config struct {
	// Proxy is the Teleport proxy address (host:port) that all `tsh`
	// commands should target. When non-empty, prism exports it as
	// TELEPORT_PROXY for its subprocesses. An existing TELEPORT_PROXY
	// in the environment still wins.
	Proxy string `json:"proxy,omitempty"`
	// Identity selects which identity backend `prism up` uses for the
	// data-port subprocess: "tsh" (default; interactive login) or
	// "tbot" (Machine ID with bound-keypair, self-refreshing). Set
	// once via `prism config set identity tbot` after
	// `prism tbot bootstrap` has been run.
	Identity string `json:"identity,omitempty"`
	// TbotDir is the path to the tbot working directory containing
	// tbot.yaml, role.yaml, the storage subdir, and prism's sidecar
	// .prism-tbot.json. Required when Identity == "tbot".
	TbotDir string `json:"tbot.dir,omitempty"`
}

// UnmarshalJSON accepts both the current `tbot.dir` and the legacy
// `tbot_dir` keys so older config files keep loading.
func (c *Config) UnmarshalJSON(b []byte) error {
	type alias Config
	aux := struct {
		*alias
		LegacyTbotDir string `json:"tbot_dir,omitempty"`
	}{alias: (*alias)(c)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if c.TbotDir == "" && aux.LegacyTbotDir != "" {
		c.TbotDir = aux.LegacyTbotDir
	}
	return nil
}

func dir() (string, error) {
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		xdg = filepath.Join(home, ".config")
	}
	return filepath.Join(xdg, "prism"), nil
}

// Path returns the absolute path to the config file.
func Path() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

// Load reads the config file, or returns a zero-valued Config (no error)
// if the file doesn't exist yet.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}

// Save writes the config atomically (mode 0600).
func Save(c *Config) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	p := filepath.Join(d, "config.json")
	tmp := p + ".tmp"
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
