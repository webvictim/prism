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
	// ClaudeAuth selects which credential `prism claude` (and
	// `prism env`) hands Claude Code:
	//
	//   auto  — default: always inject prism's dummy gateway token
	//           except in forward-proxy mode with a usable claude.ai
	//           login, which is left alone for Remote Control's sake.
	//   token — always inject. The escape hatch for a stored login
	//           that still looks usable but no longer works.
	//   login — never inject; Claude Code uses its own credential.
	//
	// Empty means auto. See cmd/prism/claude_auth.go.
	ClaudeAuth string `json:"claude_auth,omitempty"`
	// ModelDiscovery lets prism read the cluster's served models: the
	// daemon keeps a cached copy, which narrows Codex's model picker and
	// picks the probe models for prism test / doctor / tbot reset. A
	// pointer so that an absent key means on, the default; false turns all
	// of it off. It fails open: any problem leaves Codex its own catalog
	// and the probes their built-ins, with a warning.
	ModelDiscovery *bool `json:"model_discovery,omitempty"`
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
	// AnthropicStripToolTypes are extra tool type prefixes to drop from
	// /v1/messages requests, on top of the built-in list in
	// internal/scrub. It exists so a new client tool the gateway rejects
	// ("tool type '...' is not supported") can be worked around without
	// a prism release. Read at daemon startup.
	AnthropicStripToolTypes []string `json:"anthropic_strip_tool_types,omitempty"`
	// AnthropicStripFields are extra top-level /v1/messages fields to
	// drop, on top of the built-in list in internal/scrub — the same
	// escape hatch for a new body field the gateway rejects. Read at
	// daemon startup.
	AnthropicStripFields []string `json:"anthropic_strip_fields,omitempty"`
	// OpenAIStripFields / OpenAIStripToolTypes are the OpenAI
	// equivalents, applied to /v1/responses and /v1/chat/completions
	// (tool types only matter on /v1/responses; the chat/completions
	// shim rejects tools outright). Empty by default. Read at daemon
	// startup.
	OpenAIStripFields    []string `json:"openai_strip_fields,omitempty"`
	OpenAIStripToolTypes []string `json:"openai_strip_tool_types,omitempty"`
	// AnthropicTestModel is the model `prism test`, `prism doctor` and
	// `prism tbot reset` name in their probe requests, replacing the
	// built-in default (cmd/prism/test.go) so a gateway that stops
	// serving it needs a config change rather than a release. Read by
	// each command as it runs, so no restart is needed.
	AnthropicTestModel string `json:"anthropic_test_model,omitempty"`
	// OpenAITestModel is the same for `prism test`'s OpenAI probes. It
	// has no built-in default: empty sends no model, which a gateway with
	// a fallback answers with its own choice.
	OpenAITestModel string `json:"openai_test_model,omitempty"`
	// BeamMode decides whether inference goes out through a Teleport beam,
	// which records it as a Beams session (see internal/beam):
	//
	//   required  — default: only through a beam. One is started when the
	//               first request needs it; with none to be had, requests
	//               are refused with an error saying how to fix it.
	//   preferred — through a beam when one can be started, otherwise
	//               through the tunnels, unrecorded, with a warning.
	//   off       — through the tunnels, unrecorded, unless a beam was
	//               started with `prism beam start`.
	//
	// Empty means required, except on a machine set up for tbot, where it
	// means off until the user opts in with `prism setup beams` (see
	// BeamUnmigrated). The daemon re-reads it, so a change applies without
	// a restart.
	BeamMode string `json:"beam_mode,omitempty"`
	// BeamAutoLogin lets the daemon run `tsh login` itself when a beam is
	// needed and there is no login, holding the request until the beam is
	// up. Off by default: a login opens a browser, which is only harmless
	// where the identity provider signs the user in without a prompt.
	BeamAutoLogin bool `json:"beam_auto_login,omitempty"`
	// BeamRoles, when set, are the only roles delegated to the beams prism
	// starts (`tsh beams add --roles`), instead of all the user's standing
	// roles. Relaying inference needs only access to the LLM apps; whether
	// a narrower set also reaches the cluster's MCP servers and other
	// tools is up to the cluster's roles.
	BeamRoles []string `json:"beam_roles,omitempty"`
	// BeamScope is how inference is divided between beams: "session"
	// (empty; each agent session its own beam) or "repo" (one per
	// repository).
	BeamScope string `json:"beam_scope,omitempty"`
	// BeamMax caps how many beams prism runs at once; zero means the
	// built-in cap (beam.MaxBeams). At the cap, a session shares a beam
	// with another session of the same repository.
	BeamMax int `json:"beam_max,omitempty"`
	// SkillAuto, when false, stops the launchers keeping the prism-beams
	// skill installed and current. Nil means on; `prism setup skill
	// --remove` sets it false, so a removal sticks, and `prism setup skill`
	// clears it.
	SkillAuto *bool `json:"skill_auto,omitempty"`
	// QuotaLimits replaces prism's assumed output-token limits for the
	// tenant's LLM apps, "anthropic=5M,openai=5M" (internal/quota). The
	// cluster's own limits can't be read with the tsh/tctl clusters pin,
	// so without this prism assumes 5M a day for a daily window and 30M
	// for a lifetime one. Read by each command as it runs.
	QuotaLimits string `json:"quota_limits,omitempty"`
	// StatuslineAuto lets `prism claude` show the tenant's quota in
	// statusline when Claude Code's status line is statusline: it points
	// that one session at `prism usage --statusline` and adds prism's two
	// segments to statusline's settings. A pointer so that absent means
	// on; `prism setup statusline --remove` sets false so a removal
	// sticks, and `prism setup statusline` clears it.
	StatuslineAuto *bool `json:"statusline_auto,omitempty"`
}

// SkillAutoEnabled reports whether the launchers keep the skill current.
func (c *Config) SkillAutoEnabled() bool { return c == nil || c.SkillAuto == nil || *c.SkillAuto }

// The values BeamMode accepts.
const (
	BeamRequired  = "required"
	BeamPreferred = "preferred"
	BeamOff       = "off"
)

// BeamModes are the values BeamMode accepts, default first.
var BeamModes = []string{BeamRequired, BeamPreferred, BeamOff}

// Beam returns the effective beam mode. Unrecognised values count as the
// default, so a typo can't switch recording off.
func (c *Config) Beam() string {
	if c == nil {
		return BeamRequired
	}
	switch c.BeamMode {
	case BeamPreferred, BeamOff:
		return c.BeamMode
	}
	if c.BeamUnmigrated() {
		return BeamOff
	}
	return BeamRequired
}

// BeamUnmigrated reports a machine set up for tbot that hasn't chosen a
// beam mode. Recording in Beams needs the user's own tsh login, which a
// tbot user may never have had, so such a machine isn't switched over on
// upgrade: it carries on unrecorded, and prism tells the user how to
// switch (`prism setup beams`) every time they run it.
func (c *Config) BeamUnmigrated() bool {
	return c != nil && c.BeamMode == "" && c.Identity == "tbot"
}

// ChatCompletionsShimEnabled reports whether the router should translate
// /v1/chat/completions into Responses API calls. Enabled unless the
// config explicitly says otherwise.
func (c *Config) ChatCompletionsShimEnabled() bool {
	return c == nil || c.OpenAIChatCompletionsShim == nil || *c.OpenAIChatCompletionsShim
}

func (c *Config) StatuslineAutoEnabled() bool {
	return c == nil || c.StatuslineAuto == nil || *c.StatuslineAuto
}

func (c *Config) ModelDiscoveryEnabled() bool {
	return c == nil || c.ModelDiscovery == nil || *c.ModelDiscovery
}

// UnmarshalJSON accepts both the current `tbot.dir` and the legacy
// `tbot_dir` keys so older config files keep loading.
func (c *Config) UnmarshalJSON(b []byte) error {
	type alias Config
	aux := struct {
		*alias
		LegacyTbotDir string `json:"tbot_dir,omitempty"`
		// The pre-release name, from when it only governed Codex.
		LegacyModelDiscovery *bool `json:"codex_model_discovery,omitempty"`
	}{alias: (*alias)(c)}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if c.TbotDir == "" && aux.LegacyTbotDir != "" {
		c.TbotDir = aux.LegacyTbotDir
	}
	if c.ModelDiscovery == nil && aux.LegacyModelDiscovery != nil {
		c.ModelDiscovery = aux.LegacyModelDiscovery
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
