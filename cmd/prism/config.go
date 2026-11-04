package main

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/webvictim/prism/internal/config"
)

// configKeys is the set of keys `prism config set|unset` understands,
// used to build the usage strings.
var configKeys = []string{"proxy", "identity", "tbot.dir", "claude_forward_proxy_mode", "claude_auth", "codex_model_discovery", "openai_chat_completions_shim", "anthropic_strip_fields", "anthropic_strip_tool_types", "openai_strip_fields", "openai_strip_tool_types", "anthropic_test_model", "openai_test_model"}

// stripListField returns the config field behind a comma-separated
// strip-list key, or nil if key isn't one. These lists are additive to
// the built-ins in internal/scrub and are read at daemon startup.
func stripListField(c *config.Config, key string) *[]string {
	switch key {
	case "anthropic_strip_fields":
		return &c.AnthropicStripFields
	case "anthropic_strip_tool_types":
		return &c.AnthropicStripToolTypes
	case "openai_strip_fields":
		return &c.OpenAIStripFields
	case "openai_strip_tool_types":
		return &c.OpenAIStripToolTypes
	}
	return nil
}

// testModelField returns the config field behind a probe-model key.
func testModelField(c *config.Config, key string) *string {
	if key == "anthropic_test_model" {
		return &c.AnthropicTestModel
	}
	return &c.OpenAITestModel
}

// cmdConfig implements `prism config show | set <k> <v> | unset <k> | clear`.
//
// Keys currently understood: `proxy`, `identity`, `tbot.dir`,
// `claude_forward_proxy_mode`, `claude_auth`,
// `codex_model_discovery`, `openai_chat_completions_shim`,
// the strip lists `{anthropic,openai}_strip_fields` (comma-separated
// top-level body fields) and `{anthropic,openai}_strip_tool_types`
// (comma-separated tool type prefixes), and the probe models
// `{anthropic,openai}_test_model`.
func cmdConfig(args []string) error {
	if len(args) == 0 {
		args = []string{"show"}
	}
	switch args[0] {
	case "show":
		return configShow()
	case "set":
		if len(args) != 3 {
			return fmt.Errorf("usage: prism config set <key> <value>  (keys: %s)", strings.Join(configKeys, ", "))
		}
		return configSet(args[1], args[2])
	case "unset":
		if len(args) != 2 {
			return fmt.Errorf("usage: prism config unset <key>  (keys: %s)", strings.Join(configKeys, ", "))
		}
		return configUnset(args[1])
	case "clear":
		return configClear()
	default:
		return fmt.Errorf("usage: prism config [show|set <k> <v>|unset <k>|clear]")
	}
}

func configShow() error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	p, _ := config.Path()
	fmt.Fprintf(os.Stderr, "config file: %s\n", p)
	b, _ := json.MarshalIndent(c, "", "  ")
	os.Stdout.Write(b)
	fmt.Println()
	return nil
}

func configSet(key, value string) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	if field := stripListField(c, key); field != nil {
		// Replaces the configured list; the built-in lists in
		// internal/scrub always apply on top of it.
		var items []string
		for s := range strings.SplitSeq(value, ",") {
			if s = strings.TrimSpace(s); s != "" {
				items = append(items, s)
			}
		}
		if len(items) == 0 {
			return fmt.Errorf("%s needs at least one comma-separated entry; use unset to clear", key)
		}
		*field = items
		if err := config.Save(c); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "prism: %s=%s (restart the daemon to apply)\n", key, strings.Join(items, ","))
		return nil
	}
	switch key {
	case "proxy":
		c.Proxy = value
	case "identity":
		switch value {
		case "tsh", "tbot":
			c.Identity = value
		default:
			return fmt.Errorf("invalid identity %q (must be \"tsh\" or \"tbot\")", value)
		}
	case "tbot.dir":
		expanded, err := expandHome(value)
		if err != nil {
			return err
		}
		c.TbotDir = expanded
	case "claude_forward_proxy_mode":
		switch value {
		case "true", "1", "on":
			c.ClaudeForwardProxyMode = true
		case "false", "0", "off":
			c.ClaudeForwardProxyMode = false
		default:
			return fmt.Errorf("invalid value %q for claude_forward_proxy_mode (use true/false)", value)
		}
	case "claude_auth":
		if !slices.Contains(claudeAuthModes, value) {
			return fmt.Errorf("invalid value %q for claude_auth (use %s)", value, strings.Join(claudeAuthModes, "/"))
		}
		c.ClaudeAuth = value
	case "codex_model_discovery":
		switch value {
		case "true", "1", "on":
			on := true
			c.CodexModelDiscovery = &on
		case "false", "0", "off":
			off := false
			c.CodexModelDiscovery = &off
		default:
			return fmt.Errorf("invalid value %q for codex_model_discovery (use true/false)", value)
		}
	case "openai_chat_completions_shim":
		switch value {
		case "true", "1", "on":
			on := true
			c.OpenAIChatCompletionsShim = &on
		case "false", "0", "off":
			off := false
			c.OpenAIChatCompletionsShim = &off
		default:
			return fmt.Errorf("invalid value %q for openai_chat_completions_shim (use true/false)", value)
		}
	case "anthropic_test_model", "openai_test_model":
		// No validation beyond non-empty: which names the gateway serves
		// is its configuration, and prism test reports a refused one.
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s needs a model id; use unset to restore the default", key)
		}
		*testModelField(c, key) = strings.TrimSpace(value)
	default:
		return fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(configKeys, ", "))
	}
	if err := config.Save(c); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "prism: %s=%s\n", key, value)
	return nil
}

func configUnset(key string) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	switch key {
	case "proxy":
		c.Proxy = ""
	case "identity":
		c.Identity = ""
	case "tbot.dir":
		c.TbotDir = ""
	case "claude_forward_proxy_mode":
		c.ClaudeForwardProxyMode = false
	case "claude_auth":
		// Empty restores the default (auto).
		c.ClaudeAuth = ""
	case "codex_model_discovery":
		c.CodexModelDiscovery = nil
	case "openai_chat_completions_shim":
		// nil restores the default (enabled).
		c.OpenAIChatCompletionsShim = nil
	case "anthropic_test_model", "openai_test_model":
		// Empty restores the built-in default.
		*testModelField(c, key) = ""
	default:
		field := stripListField(c, key)
		if field == nil {
			return fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(configKeys, ", "))
		}
		*field = nil
	}
	if err := config.Save(c); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "prism: unset %s\n", key)
	return nil
}

func configClear() error {
	p, err := config.Path()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(os.Stderr, "prism: config cleared")
	return nil
}
