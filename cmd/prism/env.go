package main

import (
	"fmt"
	"os"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/mitm"
	"github.com/webvictim/prism/internal/state"
)

func cmdEnv(_ []string) error {
	s, err := state.Load()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Fprintln(os.Stderr, "prism: no active session (run `prism up`)")
		return nil
	}

	cfg, _ := config.Load()
	if cfg != nil && cfg.ClaudeForwardProxyMode {
		configDir, _ := config.Dir()
		caPath := mitm.CACertPath(configDir)
		fmt.Printf("export HTTPS_PROXY=http://127.0.0.1:%d\n", s.LocalPort)
		fmt.Printf("export NODE_EXTRA_CA_CERTS=%s\n", caPath)
		fmt.Printf("unset ANTHROPIC_BASE_URL\n")
		fmt.Printf("unset ANTHROPIC_AUTH_TOKEN\n")
		fmt.Printf("unset ANTHROPIC_API_KEY\n")
		fmt.Printf("export OPENAI_BASE_URL=http://127.0.0.1:%d/v1\n", s.LocalPort)
		fmt.Printf("export OPENAI_API_KEY=teleport\n")
	} else {
		fmt.Printf("export ANTHROPIC_BASE_URL=http://127.0.0.1:%d\n", s.LocalPort)
		fmt.Printf("unset ANTHROPIC_AUTH_TOKEN\n")
		fmt.Printf("unset ANTHROPIC_API_KEY\n")
		fmt.Printf("export OPENAI_BASE_URL=http://127.0.0.1:%d/v1\n", s.LocalPort)
		fmt.Printf("export OPENAI_API_KEY=teleport\n")
	}
	return nil
}
