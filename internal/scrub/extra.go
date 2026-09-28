package scrub

import (
	"log"
	"strings"
)

// Extra holds strip lists supplied by config (anthropic_strip_fields,
// anthropic_strip_tool_types, openai_strip_fields,
// openai_strip_tool_types). They exist so a new client field or tool
// the gateway rejects can be worked around without a prism release —
// for some users prism is the only path to inference. Each list is
// additive: built-in lists always apply as well.
type Extra struct {
	AnthropicFields    []string // top-level /v1/messages fields
	AnthropicToolTypes []string // tool "type" prefixes in /v1/messages
	OpenAIFields       []string // top-level /v1/responses + chat/completions fields
	OpenAIToolTypes    []string // tool "type" prefixes in /v1/responses
}

var extra Extra

// SetExtra installs config-supplied strip lists. Call it once before
// serving requests; it is not synchronised.
func SetExtra(e Extra) {
	extra = Extra{
		AnthropicFields:    cleanList(e.AnthropicFields),
		AnthropicToolTypes: cleanList(e.AnthropicToolTypes),
		OpenAIFields:       cleanList(e.OpenAIFields),
		OpenAIToolTypes:    cleanList(e.OpenAIToolTypes),
	}
}

// StripOpenAIFields removes the configured openai_strip_fields from a
// request body, reporting whether anything was removed. Exported for
// internal/chatcompat, which builds its own upstream request.
func StripOpenAIFields(obj map[string]any) bool {
	return stripFields(obj, extra.OpenAIFields)
}

func cleanList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stripFields(obj map[string]any, fields ...[]string) bool {
	changed := false
	for _, list := range fields {
		for _, k := range list {
			if _, ok := obj[k]; ok {
				delete(obj, k)
				changed = true
			}
		}
	}
	return changed
}

// stripToolTypes removes tool definitions whose "type" starts with any
// of prefixes. A tool_choice naming a removed tool (by name, or by type
// for hosted tools) is dropped too, and so is an emptied tools array
// along with any tool_choice, since gateways reject tool_choice without
// tools.
func stripToolTypes(obj map[string]any, prefixes []string, logger *log.Logger, debug bool) bool {
	tools, ok := obj["tools"].([]any)
	if !ok || len(prefixes) == 0 {
		return false
	}
	kept := tools[:0:0]
	removed := map[string]bool{}
	for _, t := range tools {
		if tool, ok := t.(map[string]any); ok {
			typ, _ := tool["type"].(string)
			if typ != "" && hasAnyPrefix(typ, prefixes) {
				name, _ := tool["name"].(string)
				if name != "" {
					removed[name] = true
				}
				if debug {
					logger.Printf("scrub: dropping unsupported tool type=%s name=%s", typ, name)
				}
				continue
			}
		}
		kept = append(kept, t)
	}
	if len(kept) == len(tools) {
		return false
	}
	if len(kept) == 0 {
		delete(obj, "tools")
		delete(obj, "tool_choice")
		return true
	}
	obj["tools"] = kept
	if tc, ok := obj["tool_choice"].(map[string]any); ok {
		name, _ := tc["name"].(string)
		typ, _ := tc["type"].(string)
		if removed[name] || hasAnyPrefix(typ, prefixes) {
			delete(obj, "tool_choice")
		}
	}
	return true
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
