package main

import (
	"fmt"
	"strings"
)

// Codex isn't steerable by environment variable: `OPENAI_BASE_URL` has
// been ignored for a long time (verified on 0.145 through 0.165), so
// codex talks to api.openai.com with the dummy key prism hands out and
// fails. Since 0.165 it fails twice over, first on a WebSocket and then
// on the HTTPS fallback to the same host:
//
//	ERROR: unexpected status 401 Unauthorized: Incorrect API key
//	provided: teleport. ... url: wss://api.openai.com/v1/responses
//
// Overriding the built-in `openai` provider isn't an option either —
// codex rejects that with "only supports changing `base_url`, `auth`,
// and `http_headers`; other non-default provider fields are not
// supported", and `base_url` alone doesn't move the WebSocket endpoint.
//
// So prism registers a provider of its own. A provider that isn't
// built-in has `supports_websockets` false, which is what keeps codex on
// plain HTTPS — prism's router speaks HTTP, not WebSockets.
const codexProvider = "prism"

// codexConfigArgs splices the `-c` overrides that point codex at prism's
// router into its command line.
//
// They go immediately after the first non-flag argument, which is the
// subcommand when there is one (`codex exec …`), because codex 0.155 and
// older discard the root-level `-c` list entirely as soon as the
// subcommand carries one of its own — so `prism codex exec -c
// sandbox_mode=…` would silently lose the provider. Newer codex merges
// the two lists, and either way a subcommand-level override is honoured.
// With nothing but flags (or nothing at all) there is no subcommand to
// land after, and the root list is the only one.
//
// Placing them first also lets anything the user passes win: codex
// applies `-c` in order and the last value for a key is the one used, so
// `prism codex exec -c model_provider=openai` still reaches the real API
// if that's what someone wants.
func codexConfigArgs(port int, args []string) []string {
	overrides := []string{
		"-c", codexProviderOverride(port),
		"-c", "model_provider=" + codexProvider,
	}

	at := codexOverrideIndex(args)
	out := make([]string, 0, len(args)+len(overrides))
	out = append(out, args[:at]...)
	out = append(out, overrides...)
	return append(out, args[at:]...)
}

// codexProviderOverride is the provider definition itself, as the TOML
// codex's `-c` parses.
func codexProviderOverride(port int) string {
	return fmt.Sprintf(
		`model_providers.%s={name="prism",base_url="http://127.0.0.1:%d/v1",env_key="OPENAI_API_KEY",wire_api="responses",supports_websockets=false}`,
		codexProvider, port,
	)
}

// codexOverrideIndex returns where prism's overrides belong: just past
// the first non-flag argument, or the end of the list if every argument
// is a flag. A `-c`/`--config` value is skipped over, so a user's own
// override doesn't get mistaken for the subcommand.
func codexOverrideIndex(args []string) int {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c", "--config":
			i++ // its value, which may look like anything
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			return i + 1
		}
	}
	return len(args)
}
