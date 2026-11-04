package main

import (
	"fmt"
	"io"
	"os"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/mitm"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tshwrap"
)

func cmdEnv(_ []string) error {
	return printEnv(os.Stdout)
}

func printEnv(w io.Writer) error {
	s, err := state.Load()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Fprintln(os.Stderr, "prism: no active session (run `prism up`)")
		return nil
	}

	// Local auto mode classifier, unless already chosen; see toolEnv.
	defer fmt.Fprintf(w, "export %s=\"${%s:-0}\"\n", autoModeServerEnv, autoModeServerEnv)

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
	if note := substitutionNote(); note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	anthropicCredential := "unset ANTHROPIC_AUTH_TOKEN\n"
	if auth.GatewayToken {
		anthropicCredential = "export ANTHROPIC_AUTH_TOKEN=" + claudeGatewayToken + "\n"
	}

	// Platform mode replaces the credential with a provider, so these
	// three go out together with the token left unset.
	defer func() {
		if !auth.Platform {
			fmt.Fprintf(w, "unset %s\nunset %s\nunset %s\n",
				claudePlatformEnv, claudePlatformSkipAuthEnv, claudePlatformBaseURLEnv)
			return
		}
		fmt.Fprintf(w, "export %s=1\nexport %s=1\nexport %s=http://127.0.0.1:%d\n",
			claudePlatformEnv, claudePlatformSkipAuthEnv, claudePlatformBaseURLEnv, s.LocalPort)
	}()

	if forwardProxy {
		configDir, _ := config.Dir()
		caPath := mitm.CACertPath(configDir)
		fmt.Fprintf(w, "export HTTPS_PROXY=http://127.0.0.1:%d\n", s.LocalPort)
		fmt.Fprintf(w, "export NODE_EXTRA_CA_CERTS=%s\n", caPath)
		fmt.Fprintf(w, "unset ANTHROPIC_BASE_URL\n")
		fmt.Fprint(w, anthropicCredential)
		fmt.Fprintf(w, "unset ANTHROPIC_API_KEY\n")
		fmt.Fprintf(w, "export OPENAI_BASE_URL=http://127.0.0.1:%d/v1\n", s.LocalPort)
		fmt.Fprintf(w, "export OPENAI_API_KEY=teleport\n")
	} else {
		fmt.Fprintf(w, "export ANTHROPIC_BASE_URL=http://127.0.0.1:%d\n", s.LocalPort)
		fmt.Fprint(w, anthropicCredential)
		fmt.Fprintf(w, "unset ANTHROPIC_API_KEY\n")
		fmt.Fprintf(w, "export OPENAI_BASE_URL=http://127.0.0.1:%d/v1\n", s.LocalPort)
		fmt.Fprintf(w, "export OPENAI_API_KEY=teleport\n")
	}
	return nil
}
