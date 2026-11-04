package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tbot"
	"github.com/webvictim/prism/internal/tshwrap"
)

// clusterApps lists the cluster's apps, which is where the LLM apps say
// which models they serve (spec.inference.models). The cluster offers no
// models endpoint (/v1/models answers 404 on both gateways), so this is
// the only source of the list short of probing.
//
// It asks as prism's own tbot identity first: the identity output the
// daemon's tbot keeps fresh (tbot.IdentityFilePath) needs no tsh login,
// which a machine on tbot usually doesn't have. Then as the user's tsh
// login. Either sees the same two apps, because the bot's role grants
// them and Teleport lists exactly what a role can access. If both fail,
// both reasons are reported: "no identity file" alone would hide an
// expired login and vice versa.
func clusterApps(cfg *config.Config) ([]tshwrap.App, error) {
	var tried []error
	if path := tbotIdentityFile(cfg); path != "" {
		apps, err := tshwrap.AppsListAs(path)
		if err == nil {
			return apps, nil
		}
		tried = append(tried, fmt.Errorf("as prism's tbot identity: %w", err))
	}
	apps, err := tshwrap.AppsList()
	if err == nil {
		return apps, nil
	}
	tried = append(tried, fmt.Errorf("as your tsh login: %w", err))
	return nil, fmt.Errorf("could not list the cluster's apps: %w", errors.Join(tried...))
}

// tbotIdentityFile is the tsh identity the daemon's tbot writes, when this
// machine is on tbot and the file exists. Its certificates last minutes,
// so a file left by a daemon that has since stopped fails at tsh and the
// login is tried next.
func tbotIdentityFile(cfg *config.Config) string {
	if cfg == nil || cfg.Identity != state.IdentitySourceTbot {
		return ""
	}
	dir, err := resolveTbotDir("")
	if err != nil {
		return ""
	}
	path := tbot.IdentityFilePath(dir)
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}
