package router

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gravitational/prism/internal/usage"
)

// captureUsage returns middleware that extracts token usage from API
// responses and writes it to the usage log. It handles both streaming
// (SSE) and non-streaming (JSON) responses without adding latency.
func captureUsage(next http.Handler, w *usage.Writer, proxy string, logger *log.Logger) http.Handler {
	if w == nil {
		return next
	}
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !isAPIPath(r.URL.Path) {
			next.ServeHTTP(rw, r)
			return
		}

		backend := "anthropic"
		if isOpenAIPath(r.URL.Path) {
			backend = "openai"
		}

		// Extract model from request body (we need to peek without consuming).
		model := extractModelFromRequest(r)

		cw := &captureWriter{
			ResponseWriter: rw,
			backend:        backend,
			model:          model,
			proxy:          proxy,
			usageWriter:    w,
			logger:         logger,
		}
		next.ServeHTTP(cw, r)
		cw.finalize()
	})
}

func isAPIPath(path string) bool {
	return strings.HasPrefix(path, "/v1/")
}

// extractModelFromRequest reads the model field from the request body
// without consuming it. The body is restored for downstream handlers.
func extractModelFromRequest(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var obj struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &obj)
	return obj.Model
}

// captureWriter wraps an http.ResponseWriter to intercept response data
// for usage extraction.
type captureWriter struct {
	http.ResponseWriter
	backend     string
	model       string
	proxy       string
	usageWriter *usage.Writer
	logger      *log.Logger

	streaming bool
	headerSet bool
	status    int

	// Non-streaming: buffer the full body.
	body bytes.Buffer

	// Streaming: accumulate SSE lines to extract usage events.
	sseBuf    bytes.Buffer
	sseRecord usage.Record
}

func (cw *captureWriter) WriteHeader(code int) {
	cw.status = code
	cw.headerSet = true
	ct := cw.Header().Get("Content-Type")
	cw.streaming = strings.Contains(ct, "text/event-stream")
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *captureWriter) Write(b []byte) (int, error) {
	if !cw.headerSet {
		cw.WriteHeader(200)
	}

	// Always write to client immediately.
	n, err := cw.ResponseWriter.Write(b)

	if cw.status < 200 || cw.status >= 300 {
		return n, err
	}

	if cw.streaming {
		cw.processSSEChunk(b[:n])
	} else {
		cw.body.Write(b[:n])
	}
	return n, err
}

func (cw *captureWriter) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *captureWriter) Unwrap() http.ResponseWriter {
	return cw.ResponseWriter
}

// finalize is called after the handler returns to emit the usage record.
func (cw *captureWriter) finalize() {
	if cw.status < 200 || cw.status >= 300 {
		return
	}

	var rec usage.Record
	if cw.streaming {
		rec = cw.sseRecord
	} else {
		rec = cw.parseNonStreamingUsage()
	}

	if rec.InputTokens == 0 && rec.OutputTokens == 0 {
		return
	}

	rec.Backend = cw.backend
	rec.Model = cw.model
	rec.Proxy = cw.proxy
	cw.usageWriter.Write(rec)
}

func (cw *captureWriter) parseNonStreamingUsage() usage.Record {
	body := cw.body.Bytes()
	if len(body) == 0 {
		return usage.Record{}
	}

	if cw.backend == "anthropic" {
		return parseAnthropicUsage(body)
	}
	return parseOpenAIUsage(body)
}

// processSSEChunk parses incoming SSE data for usage fields.
// Anthropic streams usage in message_start (input) and message_delta (output).
// OpenAI streams usage in the final chunk when stream_options.include_usage is set.
func (cw *captureWriter) processSSEChunk(chunk []byte) {
	cw.sseBuf.Write(chunk)

	for {
		line, err := cw.sseBuf.ReadBytes('\n')
		if err != nil {
			// Incomplete line — put it back.
			cw.sseBuf.Write(line)
			return
		}
		line = bytes.TrimRight(line, "\r\n")

		if cw.backend == "anthropic" {
			cw.processAnthropicSSELine(line)
		} else {
			cw.processOpenAISSELine(line)
		}
	}
}

func (cw *captureWriter) processAnthropicSSELine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return
	}
	data := line[6:]

	var event struct {
		Type    string `json:"type"`
		Message struct {
			Model string `json:"model"`
			Usage struct {
				InputTokens        int64 `json:"input_tokens"`
				OutputTokens       int64 `json:"output_tokens"`
				CacheReadTokens    int64 `json:"cache_read_input_tokens"`
				CacheCreationToken int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			OutputTokens       int64 `json:"output_tokens"`
			CacheReadTokens    int64 `json:"cache_read_input_tokens"`
			CacheCreationToken int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return
	}

	switch event.Type {
	case "message_start":
		if event.Message.Model != "" {
			cw.model = event.Message.Model
		}
		cw.sseRecord.InputTokens = event.Message.Usage.InputTokens
		cw.sseRecord.OutputTokens = event.Message.Usage.OutputTokens
		cw.sseRecord.CacheRead = event.Message.Usage.CacheReadTokens
		cw.sseRecord.CacheCreate = event.Message.Usage.CacheCreationToken
	case "message_delta":
		cw.sseRecord.OutputTokens = event.Usage.OutputTokens
		if event.Usage.CacheReadTokens > 0 {
			cw.sseRecord.CacheRead = event.Usage.CacheReadTokens
		}
		if event.Usage.CacheCreationToken > 0 {
			cw.sseRecord.CacheCreate = event.Usage.CacheCreationToken
		}
	}
}

func (cw *captureWriter) processOpenAISSELine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return
	}
	data := line[6:]
	if bytes.Equal(data, []byte("[DONE]")) {
		return
	}

	var chunk struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return
	}
	if chunk.Model != "" {
		cw.model = chunk.Model
	}
	if chunk.Usage != nil {
		cw.sseRecord.InputTokens = chunk.Usage.PromptTokens
		cw.sseRecord.OutputTokens = chunk.Usage.CompletionTokens
	}
}

// parseAnthropicUsage extracts usage from a non-streaming Anthropic response.
func parseAnthropicUsage(body []byte) usage.Record {
	var resp struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			CacheReadTokens    int64 `json:"cache_read_input_tokens"`
			CacheCreationToken int64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return usage.Record{}
	}
	r := usage.Record{
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		CacheRead:    resp.Usage.CacheReadTokens,
		CacheCreate:  resp.Usage.CacheCreationToken,
	}
	if resp.Model != "" {
		r.Model = resp.Model
	}
	return r
}

// parseOpenAIUsage extracts usage from a non-streaming OpenAI response.
func parseOpenAIUsage(body []byte) usage.Record {
	var resp struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return usage.Record{}
	}
	if resp.Usage == nil {
		return usage.Record{}
	}
	r := usage.Record{
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}
	if resp.Model != "" {
		r.Model = resp.Model
	}
	return r
}
