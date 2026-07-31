package tbot

import (
	"fmt"
	"strings"
)

// AppTunnelServiceName is the `name` we assign to the application-tunnel
// service block in the generated tbot.yaml. Naming the service lets us
// probe `/readyz/<name>` on the --diag-addr endpoint to introspect just
// the tunnel's readiness, rather than tbot's overall /readyz which also
// covers heartbeat / ca-rotation / etc.
const AppTunnelServiceName = "prism-tunnel"

// OpenAITunnelServiceName is the `name` for the second application-tunnel
// service block that targets the cluster-wide OpenAI gateway.
const OpenAITunnelServiceName = "prism-openai-tunnel"

// AppTunnelService describes one application-tunnel service block in
// the generated tbot.yaml.
type AppTunnelService struct {
	Name    string // e.g. "prism-tunnel", "prism-openai-tunnel"
	AppName string // Teleport app name
	Port    int    // 127.0.0.1 port this tunnel listens on
}

// TbotYAMLOpts is the input to RenderTbotYAML. Everything is required.
type TbotYAMLOpts struct {
	TokenName          string
	RegistrationSecret string
	StoragePath        string // absolute path to <TbotDir>/storage
	Services           []AppTunnelService
	ProxyServer        string // Teleport proxy host:port
}

// RenderTbotYAML produces the contents of tbot.yaml. Prism owns this
// file end-to-end — no user-supplied comments are preserved across
// rewrites. If you need to customise more knobs, set them here and
// re-bootstrap.
func RenderTbotYAML(o TbotYAMLOpts) []byte {
	var b strings.Builder
	fmt.Fprintln(&b, "# tbot config written by prism. Do not edit by hand —")
	fmt.Fprintln(&b, "# prism rewrites this file on every daemon spawn.")
	fmt.Fprintln(&b, "version: v2")
	fmt.Fprintln(&b, "onboarding:")
	fmt.Fprintf(&b, "  token: %s\n", o.TokenName)
	fmt.Fprintln(&b, "  join_method: bound_keypair")
	fmt.Fprintln(&b, "  bound_keypair:")
	fmt.Fprintf(&b, "    registration_secret: %s\n", o.RegistrationSecret)
	fmt.Fprintln(&b, "storage:")
	fmt.Fprintln(&b, "  type: directory")
	fmt.Fprintf(&b, "  path: %s\n", o.StoragePath)
	fmt.Fprintln(&b, "  symlinks: try-secure")
	fmt.Fprintln(&b, "  acls: \"off\"")
	fmt.Fprintln(&b, "services:")
	for _, svc := range o.Services {
		fmt.Fprintln(&b, "  - type: application-tunnel")
		fmt.Fprintf(&b, "    name: %s\n", svc.Name)
		fmt.Fprintf(&b, "    app_name: %s\n", svc.AppName)
		fmt.Fprintf(&b, "    listen: tcp://127.0.0.1:%d\n", svc.Port)
	}
	fmt.Fprintln(&b, "debug: false")
	fmt.Fprintf(&b, "proxy_server: %s\n", o.ProxyServer)
	fmt.Fprintln(&b, "credential_ttl: 12h0m0s")
	fmt.Fprintln(&b, "renewal_interval: 8h0m0s")
	fmt.Fprintln(&b, "oneshot: false")
	fmt.Fprintln(&b, "fips: false")
	return []byte(b.String())
}

// RoleYAMLOpts is the input to RenderRoleYAML.
type RoleYAMLOpts struct {
	RoleName string
}

// RenderRoleYAML produces a role granting access to the cluster-wide
// LLM gateway apps (anthropic + openai). Written once at bootstrap;
// re-apply with `tctl create -f role.yaml` if updated.
func RenderRoleYAML(o RoleYAMLOpts) []byte {
	var b strings.Builder
	fmt.Fprintln(&b, "# Role written by `prism tbot bootstrap`. Apply once via:")
	fmt.Fprintln(&b, "#   tctl create -f role.yaml")
	fmt.Fprintln(&b, "kind: role")
	fmt.Fprintln(&b, "version: v7")
	fmt.Fprintln(&b, "metadata:")
	fmt.Fprintf(&b, "  name: %s\n", o.RoleName)
	fmt.Fprintf(&b, "  description: prism-managed bot role (%s)\n", o.RoleName)
	fmt.Fprintln(&b, "spec:")
	fmt.Fprintln(&b, "  allow:")
	fmt.Fprintln(&b, "    app_labels:")
	fmt.Fprintln(&b, "      \"teleport.internal/beams/app-type\": \"llm\"")
	fmt.Fprintln(&b, "  deny: {}")
	fmt.Fprintln(&b, "  options:")
	fmt.Fprintln(&b, "    max_session_ttl: 24h0m0s")
	return []byte(b.String())
}

// TokenYAMLOpts is the input to RenderTokenYAML.
type TokenYAMLOpts struct {
	TokenName string // matches tbot.yaml's onboarding.token
	BotName   string // the bot identity this token can join as
}

// RenderTokenYAML produces a bound-keypair join token spec. Apply once
// via `tctl create -f token.yaml`. Read the resulting registration
// secret with `tctl get token/<name> --format=json | jq -r ...`.
func RenderTokenYAML(o TokenYAMLOpts) []byte {
	var b strings.Builder
	fmt.Fprintln(&b, "# Join token written by `prism tbot bootstrap`. Apply via:")
	fmt.Fprintln(&b, "#   tctl create -f token.yaml")
	fmt.Fprintln(&b, "# Then read the bound-keypair registration_secret with:")
	fmt.Fprintln(&b, "#   tctl get token/"+o.TokenName+" --format=json | jq -r '.[0].status.bound_keypair.registration_secret'")
	fmt.Fprintln(&b, "kind: token")
	fmt.Fprintln(&b, "version: v2")
	fmt.Fprintln(&b, "metadata:")
	fmt.Fprintf(&b, "  name: %s\n", o.TokenName)
	fmt.Fprintln(&b, "spec:")
	fmt.Fprintln(&b, "  roles: [Bot]")
	fmt.Fprintf(&b, "  bot_name: %s\n", o.BotName)
	fmt.Fprintln(&b, "  join_method: bound_keypair")
	fmt.Fprintln(&b, "  bound_keypair:")
	fmt.Fprintln(&b, "    recovery:")
	fmt.Fprintln(&b, "      mode: standard")
	fmt.Fprintln(&b, "      limit: 999999999")
	return []byte(b.String())
}

// BotYAMLOpts is the input to RenderBotYAML.
type BotYAMLOpts struct {
	BotName  string
	RoleName string
}

// RenderBotYAML produces the bot resource that ties the bot identity
// to its role. Apply once via `tctl create -f bot.yaml` before the
// token.
func RenderBotYAML(o BotYAMLOpts) []byte {
	var b strings.Builder
	fmt.Fprintln(&b, "# Bot identity written by `prism tbot bootstrap`. Apply via:")
	fmt.Fprintln(&b, "#   tctl create -f bot.yaml")
	fmt.Fprintln(&b, "kind: bot")
	fmt.Fprintln(&b, "version: v1")
	fmt.Fprintln(&b, "metadata:")
	fmt.Fprintf(&b, "  name: %s\n", o.BotName)
	fmt.Fprintln(&b, "spec:")
	fmt.Fprintf(&b, "  roles: ['%s']\n", o.RoleName)
	return []byte(b.String())
}
