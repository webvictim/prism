package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/webvictim/prism/internal/config"
)

// configKeys is the set of keys `prism config set|unset` understands,
// used to build the usage strings.
var configKeys = []string{"proxy", "identity", "tbot.dir", "claude_forward_proxy_mode", "openai_chat_completions_shim"}

// cmdConfig implements `prism config show | set <k> <v> | unset <k> | clear`.
//
// Keys currently understood: `proxy`, `identity`, `tbot.dir`,
// `claude_forward_proxy_mode`, `openai_chat_completions_shim`.
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
	case "openai_chat_completions_shim":
		// nil restores the default (enabled).
		c.OpenAIChatCompletionsShim = nil
	default:
		return fmt.Errorf("unknown config key %q (known: %s)", key, strings.Join(configKeys, ", "))
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
