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
	// ClaudeForwardProxyMode enables forward-proxy mode for
	// `prism claude`. Instead of setting ANTHROPIC_BASE_URL (which
	// disables Remote Control), prism acts as an HTTPS forward proxy
	// via HTTPS_PROXY and MITM's api.anthropic.com traffic. All other
	// traffic is blind-tunneled. Default false (existing behaviour).
	ClaudeForwardProxyMode bool `json:"claude_forward_proxy_mode,omitempty"`
	// OpenAIChatCompletionsShim controls whether the router translates
	// /v1/chat/completions requests into Responses API calls against the
	// OpenAI gateway. Newer gateways only serve OpenAI models on
	// /v1/responses, so the shim is what keeps chat/completions-only
	// clients (MacWhisper, Teleport session summaries) working.
	//
	// It's a pointer so that an absent key means enabled — the default —
	// while an explicit false is recorded on disk. Turn it off to talk to
	// an older gateway that still serves /v1/chat/completions natively.
	OpenAIChatCompletionsShim *bool `json:"openai_chat_completions_shim,omitempty"`
}

// ChatCompletionsShimEnabled reports whether the router should translate
// /v1/chat/completions into Responses API calls. Enabled unless the
// config explicitly says otherwise.
func (c *Config) ChatCompletionsShimEnabled() bool {
	return c == nil || c.OpenAIChatCompletionsShim == nil || *c.OpenAIChatCompletionsShim
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

// Dir returns the prism config directory path (~/.config/prism).
func Dir() (string, error) { return dir() }

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
