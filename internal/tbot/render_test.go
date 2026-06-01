package tbot

import (
	"strings"
	"testing"
)

func TestRenderTbotYAML(t *testing.T) {
	got := string(RenderTbotYAML(TbotYAMLOpts{
		TokenName:          "prism-bot",
		RegistrationSecret: "deadbeef",
		StoragePath:        "/Users/gus/.config/prism/tbot/storage",
		Services: []AppTunnelService{
			{Name: AppTunnelServiceName, AppName: "agile-grid-72ae", Port: 7331},
		},
		ProxyServer: "odd-firefly.beams.sh:443",
	}))
	mustContainAll(t, got, []string{
		"version: v2",
		"token: prism-bot",
		"join_method: bound_keypair",
		"registration_secret: deadbeef",
		"path: /Users/gus/.config/prism/tbot/storage",
		"type: application-tunnel",
		"name: " + AppTunnelServiceName,
		"app_name: agile-grid-72ae",
		"listen: tcp://127.0.0.1:7331",
		"proxy_server: odd-firefly.beams.sh:443",
	})
}

func TestRenderTbotYAMLMultiService(t *testing.T) {
	got := string(RenderTbotYAML(TbotYAMLOpts{
		TokenName:          "prism-bot",
		RegistrationSecret: "deadbeef",
		StoragePath:        "/tmp/storage",
		Services: []AppTunnelService{
			{Name: AppTunnelServiceName, AppName: "agile-grid-72ae", Port: 7334},
			{Name: OpenAITunnelServiceName, AppName: "openai", Port: 7335},
		},
		ProxyServer: "odd-firefly.beams.sh:443",
	}))
	mustContainAll(t, got, []string{
		"name: " + AppTunnelServiceName,
		"app_name: agile-grid-72ae",
		"listen: tcp://127.0.0.1:7334",
		"name: " + OpenAITunnelServiceName,
		"app_name: openai",
		"listen: tcp://127.0.0.1:7335",
	})
}

func TestRenderRoleYAML(t *testing.T) {
	got := string(RenderRoleYAML(RoleYAMLOpts{
		RoleName: "prism-bot-role-athena",
	}))
	mustContainAll(t, got, []string{
		"kind: role",
		"name: prism-bot-role-athena",
		`"teleport.internal/beams/app-type": "llm"`,
	})
}

func TestRenderTokenYAML(t *testing.T) {
	got := string(RenderTokenYAML(TokenYAMLOpts{
		TokenName: "prism-bot-token",
		BotName:   "prism-bot",
	}))
	mustContainAll(t, got, []string{
		"kind: token",
		"name: prism-bot-token",
		"bot_name: prism-bot",
		"join_method: bound_keypair",
		"roles: [Bot]",
	})
}

func TestRenderBotYAML(t *testing.T) {
	got := string(RenderBotYAML(BotYAMLOpts{
		BotName:  "prism-bot",
		RoleName: "prism-bot-role",
	}))
	mustContainAll(t, got, []string{
		"kind: bot",
		"name: prism-bot",
		"roles: ['prism-bot-role']",
	})
}

func mustContainAll(t *testing.T, got string, wants []string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("rendered output missing %q\nfull output:\n%s", w, got)
		}
	}
}
