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

// openaiFixedTempPrefixes lists model prefixes that reject non-default
// temperature values (reasoning models).
var openaiFixedTempPrefixes = []string{
	"o1",
	"o3",
	"o4",
	"gpt-5.5",
}

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
// (newer models reject the legacy name) and drops non-default
// temperature for reasoning models. It returns true when it has
// already written a response and the caller must not forward.
func OpenAIRequest(w http.ResponseWriter, r *http.Request, logger *log.Logger, debug bool) bool {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	// The tunnel provides auth via mTLS.
	r.Header.Del("Authorization")

	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/chat/completions") {
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

	if mt, ok := obj["max_tokens"]; ok {
		if _, hasNew := obj["max_completion_tokens"]; !hasNew {
			obj["max_completion_tokens"] = mt
			delete(obj, "max_tokens")
			changed = true
			if debug {
				logger.Printf("openai-scrub: renamed max_tokens → max_completion_tokens")
			}
		}
	}

	if openaiModelRequiresDefaultTemp(obj) {
		if temp, ok := obj["temperature"]; ok {
			if f, fok := numberAsFloat(temp); fok && f != 1.0 {
				delete(obj, "temperature")
				changed = true
				if debug {
					logger.Printf("openai-scrub: stripped temperature=%v for model %v", temp, obj["model"])
				}
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

// openaiModelRequiresDefaultTemp returns true for models that reject
// non-default temperature values (reasoning models like o1, o3, gpt-5.5).
func openaiModelRequiresDefaultTemp(obj map[string]any) bool {
	model, _ := obj["model"].(string)
	if model == "" {
		return false
	}
	for _, prefix := range openaiFixedTempPrefixes {
		if strings.HasPrefix(model, prefix) {
			return true
		}
	}
	return false
}
