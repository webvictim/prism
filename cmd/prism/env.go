package main

import (
	"fmt"
	"os"

	"github.com/gravitational/prism/internal/state"
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
	base := fmt.Sprintf("http://127.0.0.1:%d", s.LocalPort)
	fmt.Printf("export ANTHROPIC_API_KEY=teleport\n")
	fmt.Printf("export ANTHROPIC_API_URL=%s\n", base)
	fmt.Printf("export ANTHROPIC_AUTH_TOKEN=teleport\n")
	fmt.Printf("export ANTHROPIC_BASE_URL=%s\n", base)
	fmt.Printf("export OPENAI_API_BASE=%s/v1\n", base)
	fmt.Printf("export OPENAI_API_KEY=teleport\n")
	fmt.Printf("export OPENAI_BASE_URL=%s/v1\n", base)
	return nil
}
