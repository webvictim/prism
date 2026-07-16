// Package state persists prism's runtime state across CLI invocations.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// IdentitySource selects how the tunnel subprocesses get their Teleport
// identity:
//
//   - IdentitySourceTSH (default): the user's interactive `tsh` login.
//   - IdentitySourceTbot: a long-lived `tbot` daemon using Machine ID.
const (
	IdentitySourceTSH  = "tsh"
	IdentitySourceTbot = "tbot"
)

type State struct {
	LocalPort      int       `json:"local_port"`
	AnthropicPort  int       `json:"anthropic_port"`
	OpenAIPort     int       `json:"openai_port"`
	IdentitySource string    `json:"identity_source,omitempty"`
	TbotDir        string    `json:"tbot_dir,omitempty"`
	TbotDiagPort   int       `json:"tbot_diag_port,omitempty"`
	DaemonPID      int       `json:"daemon_pid,omitempty"`
	Debug          bool      `json:"debug,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
}

// Identity returns the effective identity source, defaulting empty to
// IdentitySourceTSH for backwards compat.
func (s *State) Identity() string {
	if s == nil || s.IdentitySource == "" {
		return IdentitySourceTSH
	}
	return s.IdentitySource
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

func Path() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "state.json"), nil
}

func Load() (*State, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &s, nil
}

func Save(s *State) error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	p := filepath.Join(d, "state.json")
	tmp := p + ".tmp"
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func Delete() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// DaemonLogDir returns the directory where daemon log files are stored.
func DaemonLogDir() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// IsLegacyState returns true if the state file contains fields from the
// old beam-based architecture (beam_id, app_name, publish_mode, etc.).
// Used by `prism up` to detect and migrate stale state.
func IsLegacyState() bool {
	p, err := Path()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return false
	}
	for _, key := range []string{"beam_id", "app_name", "publish_mode", "app_public_url"} {
		if v, ok := raw[key]; ok && v != nil && v != "" {
			return true
		}
	}
	return false
}
