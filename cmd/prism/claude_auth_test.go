package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestClaudeLoginUsable(t *testing.T) {
	for _, tt := range []struct {
		name  string
		login claudeLogin
		want  bool
	}{
		{
			name:  "a paid claude.ai login is worth keeping",
			login: claudeLogin{LoggedIn: true, AuthMethod: "claude.ai", SubscriptionType: "pro"},
			want:  true,
		},
		{
			name:  "max too",
			login: claudeLogin{LoggedIn: true, AuthMethod: "claude.ai", SubscriptionType: "max"},
			want:  true,
		},
		{
			name:  "nothing configured",
			login: claudeLogin{AuthMethod: "none"},
			want:  false,
		},
		{
			// The population this whole change is for: Claude Code was
			// signed in once, the plan behind it is gone.
			name:  "a claude.ai login without an eligible plan",
			login: claudeLogin{LoggedIn: true, AuthMethod: "claude.ai", SubscriptionType: "free"},
			want:  false,
		},
		{
			name:  "a claude.ai login reporting no plan at all",
			login: claudeLogin{LoggedIn: true, AuthMethod: "claude.ai"},
			want:  false,
		},
		{
			// prism clears its own variables before probing, so a token
			// still reported here is the user's.
			name:  "the user's own oauth token",
			login: claudeLogin{LoggedIn: true, AuthMethod: "oauth_token"},
			want:  true,
		},
		{
			name:  "an apiKeyHelper",
			login: claudeLogin{LoggedIn: true, AuthMethod: "apiKeyHelper"},
			want:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.login.usable(); got != tt.want {
				t.Errorf("usable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPlanClaudeAuth(t *testing.T) {
	var (
		pro      = claudeLogin{LoggedIn: true, AuthMethod: "claude.ai", SubscriptionType: "pro"}
		lapsed   = claudeLogin{LoggedIn: true, AuthMethod: "claude.ai", SubscriptionType: "free"}
		none     = claudeLogin{AuthMethod: "none"}
		probeErr = errors.New("claude not on PATH")
	)

	for _, tt := range []struct {
		name         string
		mode         string
		forwardProxy bool
		pinned       bool
		login        claudeLogin
		probeErr     error
		want         claudeAuthPlan
		wantProbed   bool
	}{
		{
			// Router mode points ANTHROPIC_BASE_URL at prism, so a
			// claude.ai login buys nothing and a lapsed one must not be
			// consulted. No probe needed at all.
			name:  "router mode always injects without probing",
			mode:  claudeAuthAuto,
			login: pro,
			want:  claudeAuthPlan{GatewayToken: true},
		},
		{
			name:         "forward-proxy mode leaves a paid login alone",
			mode:         claudeAuthAuto,
			forwardProxy: true,
			login:        pro,
			wantProbed:   true,
		},
		{
			name:         "forward-proxy mode injects for a lapsed plan",
			mode:         claudeAuthAuto,
			forwardProxy: true,
			login:        lapsed,
			want:         claudeAuthPlan{GatewayToken: true},
			wantProbed:   true,
		},
		{
			name:         "forward-proxy mode injects when nothing is configured",
			mode:         claudeAuthAuto,
			forwardProxy: true,
			login:        none,
			want:         claudeAuthPlan{GatewayToken: true},
			wantProbed:   true,
		},
		{
			// Enabling forward-proxy mode is itself a statement that an
			// account is behind it, so an unreadable probe must not
			// knock a working Remote Control session off OAuth.
			name:         "a failed probe leaves auth alone",
			mode:         claudeAuthAuto,
			forwardProxy: true,
			probeErr:     probeErr,
			wantProbed:   true,
		},
		{
			// The case this exists for. A token is refused under a pin
			// and no credential stops at "Please run /login", so the
			// only way through is not to be a first-party client — and
			// the login state is then irrelevant, so nothing is probed.
			name:   "a pinned machine switches provider instead",
			mode:   claudeAuthAuto,
			pinned: true,
			login:  none,
			want:   claudeAuthPlan{Platform: true, Note: claudeAuthPlatformNote},
		},
		{
			// Even with a perfectly good login: it may well not be in
			// the pinned organization, and prism can't tell.
			name:   "a pin outranks a usable login",
			mode:   claudeAuthAuto,
			pinned: true,
			login:  pro,
			want:   claudeAuthPlan{Platform: true, Note: claudeAuthPlatformNote},
		},
		{
			// Remote Control needs a first-party session, which a pinned
			// machine can't start at all, so platform mode still wins.
			name:         "a pin outranks forward-proxy mode",
			mode:         claudeAuthAuto,
			forwardProxy: true,
			pinned:       true,
			login:        pro,
			want:         claudeAuthPlan{Platform: true, Note: claudeAuthPlatformNote},
		},
		{
			name:  "platform mode can be asked for outright",
			mode:  claudeAuthPlatform,
			login: pro,
			want:  claudeAuthPlan{Platform: true},
		},
		{
			name:  "token mode overrides a paid login",
			mode:  claudeAuthToken,
			login: pro,
			want:  claudeAuthPlan{GatewayToken: true},
		},
		{
			name:         "token mode overrides forward-proxy mode too",
			mode:         claudeAuthToken,
			forwardProxy: true,
			login:        pro,
			want:         claudeAuthPlan{GatewayToken: true},
		},
		{
			// An explicit mode is an instruction, not a hint: prism
			// follows it even though the pin will refuse the token.
			name:   "token mode is still honoured on a pinned machine",
			mode:   claudeAuthToken,
			pinned: true,
			login:  none,
			want:   claudeAuthPlan{GatewayToken: true},
		},
		{
			name:  "login mode never injects, even in router mode",
			mode:  claudeAuthLogin,
			login: none,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			probed := false
			probe := func() (claudeLogin, error) {
				probed = true
				return tt.login, tt.probeErr
			}
			got := planClaudeAuth(tt.mode, tt.forwardProxy, tt.pinned, probe)
			if got != tt.want {
				t.Errorf("planClaudeAuth() = %+v, want %+v", got, tt.want)
			}
			if probed != tt.wantProbed {
				t.Errorf("probed = %v, want %v", probed, tt.wantProbed)
			}
		})
	}
}

// Platform mode and the gateway token are alternatives, never both: a
// credential of any kind is what a sign-in pin refuses, so one arriving
// alongside the provider would undo the whole thing.
func TestPlanClaudeAuthNeverMixesCredentialAndProvider(t *testing.T) {
	probe := func() (claudeLogin, error) { return claudeLogin{AuthMethod: "none"}, nil }
	for _, mode := range claudeAuthModes {
		for _, forwardProxy := range []bool{false, true} {
			for _, pinned := range []bool{false, true} {
				plan := planClaudeAuth(mode, forwardProxy, pinned, probe)
				if plan.Platform && plan.GatewayToken {
					t.Errorf("mode %q forwardProxy=%v pinned=%v: %+v carries both",
						mode, forwardProxy, pinned, plan)
				}
			}
		}
	}
}

// `claude auth status --json` exits 1 when there's no login but still
// prints the report. Reading the exit status as a failed probe would
// turn "nobody is logged in" into "leave the login alone", the exact
// case this feature exists for.
func TestClaudeLoginStatusReadsReportDespiteExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()

	for _, tt := range []struct {
		name    string
		script  string
		want    claudeLogin
		wantErr bool
	}{
		{
			name:   "report on a non-zero exit is still a report",
			script: "#!/bin/sh\necho '{\"loggedIn\":false,\"authMethod\":\"none\"}'\nexit 1\n",
			want:   claudeLogin{AuthMethod: "none"},
		},
		{
			name:   "a logged-in report",
			script: "#!/bin/sh\necho '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"subscriptionType\":\"pro\"}'\n",
			want:   claudeLogin{LoggedIn: true, AuthMethod: "claude.ai", SubscriptionType: "pro"},
		},
		{
			name:    "output that isn't a report is a failed probe",
			script:  "#!/bin/sh\necho 'command not found' >&2\nexit 127\n",
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			bin := filepath.Join(dir, "claude-stub")
			if err := os.WriteFile(bin, []byte(tt.script), 0o700); err != nil {
				t.Fatal(err)
			}
			got, err := claudeLoginStatus(bin, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("claudeLoginStatus() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("claudeLoginStatus() failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("claudeLoginStatus() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The probe has to see the user's own configuration: prism's own token
// would otherwise come back as a login and keep itself alive forever.
func TestWithoutPrismAuthVars(t *testing.T) {
	env := withoutPrismAuthVars([]string{
		"PATH=/usr/bin",
		"ANTHROPIC_AUTH_TOKEN=" + claudeGatewayToken,
		"ANTHROPIC_API_KEY=teleport",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:7331",
		"CLAUDE_CODE_OAUTH_TOKEN=user-token",
		"HOME=/home/gus",
	})

	for _, unwanted := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"} {
		if _, found := envValue(env, unwanted); found {
			t.Errorf("%s survived the probe environment", unwanted)
		}
	}
	// The user's own credential is theirs to report.
	for _, want := range []string{"PATH=/usr/bin", "HOME=/home/gus", "CLAUDE_CODE_OAUTH_TOKEN=user-token"} {
		if !slices.Contains(env, want) {
			t.Errorf("%q was dropped", want)
		}
	}
}
