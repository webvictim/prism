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

// OpenAIMiddleware wraps next with OpenAIRequest scrubbing.
func OpenAIMiddleware(next http.Handler, logger *log.Logger, debug bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if OpenAIRequest(w, r, logger, debug) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// OpenAIRequest normalises an OpenAI-bound request in place. It strips
// the client's Authorization header, and for JSON POSTs to
// /v1/chat/completions renames max_tokens → max_completion_tokens
// (newer models reject the legacy name). JSON POSTs to either
// /v1/chat/completions or /v1/responses also get the config-supplied
// openai_strip_fields / openai_strip_tool_types removed (empty by
// default). It returns true when it has already written a response and
// the caller must not forward.
//
// Per-model parameter quirks are deliberately not handled here: which
// parameters a model refuses is discovered from the gateway's own error
// message by internal/chatcompat, so no model names live in prism.
func OpenAIRequest(w http.ResponseWriter, r *http.Request, logger *log.Logger, debug bool) bool {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	// The tunnel provides auth via mTLS.
	r.Header.Del("Authorization")

	isChat := strings.HasPrefix(r.URL.Path, "/v1/chat/completions")
	isResponses := r.URL.Path == "/v1/responses"
	if r.Method != http.MethodPost || !(isChat || isResponses) {
		return false
	}
	if isResponses && len(extra.OpenAIFields) == 0 && len(extra.OpenAIToolTypes) == 0 {
		// Nothing configured: leave Responses bodies byte-for-byte.
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

	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		return false
	}

	changed := false

	if StripOpenAIFields(obj) {
		changed = true
	}
	if stripToolTypes(obj, extra.OpenAIToolTypes, logger, debug) {
		changed = true
	}

	if mt, ok := obj["max_tokens"]; ok && isChat {
		if _, hasNew := obj["max_completion_tokens"]; !hasNew {
			obj["max_completion_tokens"] = mt
			delete(obj, "max_tokens")
			changed = true
			if debug {
				logger.Printf("openai-scrub: renamed max_tokens → max_completion_tokens")
			}
		}
	}

	if changed {
		rewritten, err := json.Marshal(obj)
		if err == nil {
			body = rewritten
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	return false
}
