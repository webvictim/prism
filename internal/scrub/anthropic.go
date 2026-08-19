// Package scrub normalises API requests for the cluster's gateways.
// Both the local router and the MITM forward proxy apply the same
// scrubbing, so requests reach the gateway in an identical shape
// regardless of how the client connects to prism.
package scrub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// NonStreamMaxTokensCap is the largest max_tokens the Bedrock-backed
// gateway accepts for non-streaming requests. Above it, Bedrock rejects
// the call with "request needs to use streaming" (verified: 8192 → 200,
// 8193 → 400).
const NonStreamMaxTokensCap = 8192

// anthropicStripFields are top-level /v1/messages request fields the
// Bedrock-backed gateway rejects. Add new ones here when a new Claude
// Code feature breaks.
var anthropicStripFields = []string{
	"metadata",
	"context_management",
	"thinking",
	"diagnostics",
	"output_config",
}

// cacheControlAllowedKeys are the cache_control keys the Bedrock-backed
// gateway accepts. Claude Code in forward-proxy (OAuth) mode adds
// "scope" (prompt-caching-scope beta), which Bedrock rejects with a
// generic "inference provider rejected the request" 400. The gateway
// validates cache_control strictly, so unknown keys are removed; by
// contrast it ignores unknown keys elsewhere (e.g. per-tool
// advanced-tool-use fields), which are left untouched to keep
// tampering minimal.
var cacheControlAllowedKeys = map[string]bool{
	"type": true,
	"ttl":  true,
}

// StripAuthHeaders removes client-supplied auth headers. The tunnel
// authenticates via mTLS — dummy tokens from env vars (e.g. "teleport")
// would otherwise be rejected by the gateway.
func StripAuthHeaders(h http.Header) {
	h.Del("Authorization")
	h.Del("X-Api-Key")
}

// AnthropicMiddleware wraps next with AnthropicRequest scrubbing.
func AnthropicMiddleware(next http.Handler, logger *log.Logger, debug bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if AnthropicRequest(w, r, logger, debug) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AnthropicRequest scrubs an Anthropic-bound request in place. It
// strips auth headers, and for JSON POSTs to /v1/messages rewrites the
// body for Bedrock compatibility. It returns true when it has already
// written a response (short-circuit or read error) and the caller must
// not forward the request.
func AnthropicRequest(w http.ResponseWriter, r *http.Request, logger *log.Logger, debug bool) bool {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	StripAuthHeaders(r.Header)

	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") {
		return false
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return false
	}

	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		http.Error(w, "prism: read body: "+err.Error(), http.StatusBadRequest)
		return true
	}

	if shouldShortCircuit(body) {
		if debug {
			logger.Printf("scrub: short-circuit 400 (output_config.format) %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prism: output_config.format is not supported by the cluster gateway"}}`+"\n")
		return true
	}

	body = anthropicBody(body, logger, debug)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return false
}

// anthropicBody rewrites a /v1/messages JSON body for Bedrock
// compatibility. Unparseable bodies are returned unchanged.
func anthropicBody(body []byte, logger *log.Logger, debug bool) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	for _, k := range anthropicStripFields {
		if _, ok := obj[k]; ok {
			delete(obj, k)
			changed = true
		}
	}
	// Sanitize cache_control everywhere it can appear: system blocks,
	// message content (including nested tool_result content), and
	// tool definitions. The tools walk stays at the top level of each
	// tool so JSON-schema property names inside input_schema (which
	// could legitimately be called "cache_control") are left alone.
	if scrubCacheControls(obj["system"]) {
		changed = true
	}
	if scrubCacheControls(obj["messages"]) {
		changed = true
	}
	if tools, ok := obj["tools"].([]any); ok {
		for _, t := range tools {
			if tool, ok := t.(map[string]any); ok {
				if scrubCacheControl(tool) {
					changed = true
				}
			}
		}
	}
	streaming, _ := obj["stream"].(bool)
	if !streaming {
		if mt, ok := numberAsFloat(obj["max_tokens"]); ok && mt > NonStreamMaxTokensCap {
			if debug {
				logger.Printf("scrub: capping max_tokens %.0f → %d (non-streaming)", mt, NonStreamMaxTokensCap)
			}
			obj["max_tokens"] = NonStreamMaxTokensCap
			changed = true
		}
	}
	if !changed {
		return body
	}
	if debug {
		logger.Printf("scrub: /v1/messages model=%v stream=%v keys=%v", obj["model"], obj["stream"], mapKeys(obj))
	}
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}

// scrubCacheControls recursively drops unsupported keys from every
// cache_control object under v, reporting whether anything was removed.
func scrubCacheControls(v any) bool {
	changed := false
	switch x := v.(type) {
	case map[string]any:
		if scrubCacheControl(x) {
			changed = true
		}
		for k, child := range x {
			// Don't descend into tool_use inputs — arbitrary user data
			// where a key called "cache_control" is not ours to touch.
			if k == "input" {
				continue
			}
			if scrubCacheControls(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range x {
			if scrubCacheControls(child) {
				changed = true
			}
		}
	}
	return changed
}

// scrubCacheControl sanitizes the cache_control object directly on m,
// if present.
func scrubCacheControl(m map[string]any) bool {
	cc, ok := m["cache_control"].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	for k := range cc {
		if !cacheControlAllowedKeys[k] {
			delete(cc, k)
			changed = true
		}
	}
	return changed
}

// shouldShortCircuit reports whether the request asks for structured
// output (output_config.format), which the gateway cannot honour.
// Silently stripping it would corrupt the caller's expectations, so
// these requests are rejected outright; Claude Code retries without it.
func shouldShortCircuit(body []byte) bool {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	oc, _ := obj["output_config"].(map[string]any)
	if oc == nil {
		return false
	}
	_, hasFormat := oc["format"]
	return hasFormat
}

func numberAsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
}

func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
