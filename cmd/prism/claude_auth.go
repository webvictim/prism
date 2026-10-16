package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

// Claude Code will not send a request unless it believes it has a
// credential of its own. Through prism that belief is beside the point:
// the Teleport LLM app authenticates to Anthropic/Bedrock cluster-side
// and prism strips the client's `Authorization` / `X-Api-Key` before
// forwarding, so whatever the client sends is discarded. But a fresh
// install — or one whose claude.ai login has lapsed — stops at
// "Not logged in · Please run /login" and never gets that far.
//
// A dummy gateway token clears the gate. These constants and the
// decision below pick when prism hands one over.
const (
	// claudeGatewayToken is the dummy ANTHROPIC_AUTH_TOKEN value. Like
	// OPENAI_API_KEY, it is never used for anything: the router and the
	// MITM proxy both delete the header.
	claudeGatewayToken = "teleport"

	// claudeAuthAuto decides per mode (the default): always inject in
	// router mode, inject in forward-proxy mode only when Claude Code
	// has no usable login of its own.
	claudeAuthAuto = "auto"
	// claudeAuthToken always injects — the escape hatch for a login
	// that still looks usable but no longer works.
	claudeAuthToken = "token"
	// claudeAuthLogin never injects, leaving Claude Code on its own
	// credential.
	claudeAuthLogin = "login"
)

// claudeAuthModes are the accepted `claude_auth` config values.
var claudeAuthModes = []string{claudeAuthAuto, claudeAuthToken, claudeAuthLogin}

// claudeLogin is the part of `claude auth status --json` prism reads.
type claudeLogin struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	SubscriptionType string `json:"subscriptionType"`
}

// usable reports whether Claude Code has a credential worth preserving.
//
// The report is Claude Code's own cached view, so it answers "is there a
// login that looks usable", not "does that account still work" — it
// keeps reporting a subscription with the network unplugged. That's why
// router mode doesn't consult it at all and `claude_auth token` exists.
func (l claudeLogin) usable() bool {
	if !l.LoggedIn {
		return false
	}
	switch l.AuthMethod {
	case "claude.ai":
		// A lapsed or downgraded plan is reported here, which is the
		// signal that /login would be needed.
		switch strings.ToLower(l.SubscriptionType) {
		case "pro", "max", "team", "enterprise":
			return true
		}
		return false
	case "none", "":
		return false
	default:
		// Some credential the user supplied themselves (their own
		// CLAUDE_CODE_OAUTH_TOKEN, an apiKeyHelper, a Console key).
		// prism clears its own variables before probing, so this can't
		// be a token prism injected.
		return true
	}
}

// claudeLoginStatus asks Claude Code which credential it would use.
//
// prism's own variables are cleared first: an inherited
// ANTHROPIC_AUTH_TOKEN (from a parent `prism claude`, or a shell that
// ran `eval $(prism env)`) is otherwise reported straight back as a
// login, and prism would read its own dummy token as the user's.
func claudeLoginStatus(bin string, environ []string) (claudeLogin, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "auth", "status", "--json")
	cmd.Env = withoutPrismAuthVars(environ)
	out, err := cmd.Output()

	// `claude auth status --json` exits 1 when there is no login, but
	// still prints the report — so the exit status is not the signal,
	// output that won't parse is. Treating a non-zero exit as a failed
	// probe is exactly backwards: it reads "nobody is logged in" as
	// "don't touch the login".
	var login claudeLogin
	if jsonErr := json.Unmarshal(out, &login); jsonErr != nil {
		if err != nil {
			return claudeLogin{}, err
		}
		return claudeLogin{}, jsonErr
	}
	return login, nil
}

// withoutPrismAuthVars drops the endpoint and credential variables prism
// hands out, so a probe sees the user's own configuration.
func withoutPrismAuthVars(environ []string) []string {
	env := make([]string, 0, len(environ))
	for _, kv := range environ {
		switch {
		case strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN="),
			strings.HasPrefix(kv, "ANTHROPIC_API_KEY="),
			strings.HasPrefix(kv, "ANTHROPIC_BASE_URL="):
			continue
		}
		env = append(env, kv)
	}
	return env
}

// claudeGatewayTokenWanted reports whether `prism claude` should hand
// Claude Code the dummy gateway token.
//
//   - Router mode: always. ANTHROPIC_BASE_URL already points somewhere
//     that isn't the first-party API, so a claude.ai login buys nothing
//     — the gateway authenticates cluster-side either way — and never
//     consulting the stored login is what makes a lapsed one harmless.
//   - Forward-proxy mode: only when Claude Code has no usable login.
//     That mode exists for Remote Control, which needs a signed-in
//     first-party session, so a real credential is worth keeping.
//
// probe is called at most once, and only in forward-proxy mode; a probe
// that fails or times out leaves the user's own auth alone, since
// turning that mode on is itself a statement that an account is behind
// it.
func claudeGatewayTokenWanted(mode string, forwardProxy bool, probe func() (claudeLogin, error)) bool {
	switch mode {
	case claudeAuthToken:
		return true
	case claudeAuthLogin:
		return false
	}
	if !forwardProxy {
		return true
	}
	login, err := probe()
	if err != nil {
		return false
	}
	return !login.usable()
}
