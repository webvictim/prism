package main

import (
	"fmt"
	"os"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/mitm"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tshwrap"
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

	// Local auto mode classifier, unless already chosen; see toolEnv.
	defer fmt.Printf("export %s=\"${%s:-0}\"\n", autoModeServerEnv, autoModeServerEnv)

	cfg, _ := config.Load()
	forwardProxy := cfg != nil && cfg.ClaudeForwardProxyMode

	// Same decision `prism claude` makes; see claude_auth.go. Any note
	// goes to stderr — stdout here is eval'd by a shell.
	mode := claudeAuthAuto
	if cfg != nil && cfg.ClaudeAuth != "" {
		mode = cfg.ClaudeAuth
	}
	auth := planClaudeAuth(mode, forwardProxy, claudeLoginPinned(), func() (claudeLogin, error) {
		bin, err := tshwrap.LookPathStrict("claude")
		if err != nil {
			return claudeLogin{}, err
		}
		return claudeLoginStatus(bin, os.Environ())
	})
	if auth.Note != "" {
		fmt.Fprintln(os.Stderr, auth.Note)
	}
	anthropicCredential := "unset ANTHROPIC_AUTH_TOKEN\n"
	if auth.GatewayToken {
		anthropicCredential = "export ANTHROPIC_AUTH_TOKEN=" + claudeGatewayToken + "\n"
	}

	// Platform mode replaces the credential with a provider, so these
	// three go out together with the token left unset.
	defer func() {
		if !auth.Platform {
			fmt.Printf("unset %s\nunset %s\nunset %s\n",
				claudePlatformEnv, claudePlatformSkipAuthEnv, claudePlatformBaseURLEnv)
			return
		}
		fmt.Printf("export %s=1\nexport %s=1\nexport %s=http://127.0.0.1:%d\n",
			claudePlatformEnv, claudePlatformSkipAuthEnv, claudePlatformBaseURLEnv, s.LocalPort)
	}()

	if forwardProxy {
		configDir, _ := config.Dir()
		caPath := mitm.CACertPath(configDir)
		fmt.Printf("export HTTPS_PROXY=http://127.0.0.1:%d\n", s.LocalPort)
		fmt.Printf("export NODE_EXTRA_CA_CERTS=%s\n", caPath)
		fmt.Printf("unset ANTHROPIC_BASE_URL\n")
		fmt.Print(anthropicCredential)
		fmt.Printf("unset ANTHROPIC_API_KEY\n")
		fmt.Printf("export OPENAI_BASE_URL=http://127.0.0.1:%d/v1\n", s.LocalPort)
		fmt.Printf("export OPENAI_API_KEY=teleport\n")
	} else {
		fmt.Printf("export ANTHROPIC_BASE_URL=http://127.0.0.1:%d\n", s.LocalPort)
		fmt.Print(anthropicCredential)
		fmt.Printf("unset ANTHROPIC_API_KEY\n")
		fmt.Printf("export OPENAI_BASE_URL=http://127.0.0.1:%d/v1\n", s.LocalPort)
		fmt.Printf("export OPENAI_API_KEY=teleport\n")
	}
	return nil
}
