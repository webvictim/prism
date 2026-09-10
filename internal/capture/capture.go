// Package capture extracts token usage from API responses as they are
// streamed to the client, and records request status and size.
//
// It is shared by the local router and the MITM forward proxy, the same
// way internal/scrub is shared for the request direction. Both paths
// front the same gateway and must account for it identically; when this
// code was duplicated the two copies drifted, and the forward proxy
// spent that time recording the model the client asked for rather than
// the one the gateway actually served.
package capture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/webvictim/prism/internal/usage"
)

// Backend names, as recorded in usage records.
const (
	BackendAnthropic = "anthropic"
	BackendOpenAI    = "openai"
)

// Options configures a Writer.
type Options struct {
	// Backend selects the response parser and is recorded verbatim.
	Backend string
	// Model is the model named by the request body, if any. It is the
	// weakest source and only used when the response names none.
	Model string
	// Proxy is the Teleport proxy address, recorded for reporting.
	Proxy string
	// UsageWriter receives the finished record. Required.
	UsageWriter *usage.Writer
}

// Writer wraps an http.ResponseWriter to inspect response data without
// adding latency: bytes reach the client first, and are only then
// parsed. Non-streaming responses are buffered; streaming responses are
// scanned line by line as they flush through.
type Writer struct {
	http.ResponseWriter
	backend     string
	model       string
	proxy       string
	usageWriter *usage.Writer

	streaming bool
	headerSet bool
	status    int

	// Non-streaming: buffer the full body.
	body bytes.Buffer

	// Streaming: accumulate SSE lines to extract usage events.
	sseBuf    bytes.Buffer
	sseRecord usage.Record
}

// New wraps rw. Finalize must be called once the handler has returned.
func New(rw http.ResponseWriter, opts Options) *Writer {
	backend := opts.Backend
	if backend == "" {
		backend = BackendAnthropic
	}
	return &Writer{
		ResponseWriter: rw,
		backend:        backend,
		model:          opts.Model,
		proxy:          opts.Proxy,
		usageWriter:    opts.UsageWriter,
	}
}

// Status returns the response status, defaulting to 200.
func (cw *Writer) Status() int {
	if cw.status == 0 {
		return http.StatusOK
	}
	return cw.status
}

func (cw *Writer) WriteHeader(code int) {
	cw.status = code
	cw.headerSet = true
	ct := cw.Header().Get("Content-Type")
	cw.streaming = strings.Contains(ct, "text/event-stream")
	cw.ResponseWriter.WriteHeader(code)
}

func (cw *Writer) Write(b []byte) (int, error) {
	if !cw.headerSet {
		cw.WriteHeader(http.StatusOK)
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

func (cw *Writer) Flush() {
	if f, ok := cw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (cw *Writer) Unwrap() http.ResponseWriter {
	return cw.ResponseWriter
}

// Finalize writes the usage record for the response and returns it. The
// boolean reports whether anything was recorded: a non-2xx response, or
// one carrying no usage object, produces no record.
//
// Formatting for the request log is deliberately not done here — see
// Summary — so the router and the forward proxy cannot drift apart in
// how they report the same gateway.
func (cw *Writer) Finalize() (usage.Record, bool) {
	if cw.usageWriter == nil {
		return usage.Record{}, false
	}
	if cw.status < 200 || cw.status >= 300 {
		return usage.Record{}, false
	}

	var rec usage.Record
	if cw.streaming {
		rec = cw.sseRecord
	} else {
		rec = cw.parseNonStreamingUsage()
	}

	if rec.InputTokens == 0 && rec.OutputTokens == 0 {
		return usage.Record{}, false
	}

	rec.Backend = cw.backend
	// Prefer the model the response reports — the gateway aliases unknown
	// names to whatever it currently serves, and a client may omit the
	// field entirely, so the request is the weaker source.
	if rec.Model == "" {
		rec.Model = cw.model
	}
	rec.Proxy = cw.proxy
	cw.usageWriter.Write(rec)
	return rec, true
}

// Summary formats a usage record for the request log. Every field is
// always present, zeros included, so a line can be parsed without
// checking which fields happen to be there; a model the gateway did not
// report is spelled "?".
func Summary(rec usage.Record) string {
	model := rec.Model
	if model == "" {
		model = "?"
	}
	return fmt.Sprintf("model=%s in=%d out=%d cache_read=%d cache_write=%d",
		model, rec.InputTokens, rec.OutputTokens, rec.CacheRead, rec.CacheCreate)
}

func (cw *Writer) parseNonStreamingUsage() usage.Record {
	body := cw.body.Bytes()
	if len(body) == 0 {
		return usage.Record{}
	}

	if cw.backend == BackendAnthropic {
		return parseAnthropicUsage(body)
	}
	return parseOpenAIUsage(body)
}

// processSSEChunk parses incoming SSE data for usage fields.
// Anthropic streams usage in message_start (input) and message_delta (output).
// OpenAI streams usage in the final chunk when stream_options.include_usage is set.
func (cw *Writer) processSSEChunk(chunk []byte) {
	cw.sseBuf.Write(chunk)

	for {
		line, err := cw.sseBuf.ReadBytes('\n')
		if err != nil {
			// Incomplete line — put it back.
			cw.sseBuf.Write(line)
			return
		}
		line = bytes.TrimRight(line, "\r\n")

		if cw.backend == BackendAnthropic {
			cw.processAnthropicSSELine(line)
		} else {
			cw.processOpenAISSELine(line)
		}
	}
}

func (cw *Writer) processAnthropicSSELine(line []byte) {
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

func (cw *Writer) processOpenAISSELine(line []byte) {
	if !bytes.HasPrefix(line, []byte("data: ")) {
		return
	}
	data := line[6:]
	if bytes.Equal(data, []byte("[DONE]")) {
		return
	}

	var chunk struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
		// Responses API: the terminal event nests the whole response.
		Response *struct {
			Model string         `json:"model"`
			Usage *responseUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return
	}

	// Responses API stream (Codex and anything else hitting
	// /v1/responses directly): usage arrives on response.completed.
	if chunk.Type == "response.completed" || chunk.Type == "response.incomplete" {
		if chunk.Response == nil {
			return
		}
		if chunk.Response.Model != "" {
			cw.model = chunk.Response.Model
		}
		if u := chunk.Response.Usage; u != nil {
			cw.sseRecord.InputTokens = u.InputTokens
			cw.sseRecord.OutputTokens = u.OutputTokens
			cw.sseRecord.CacheRead = u.InputTokensDetails.CachedTokens
		}
		return
	}

	// chat/completions stream.
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

// responseUsage is the Responses API usage object. It differs from
// chat/completions, which spells the same counts prompt_/completion_.
type responseUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// parseOpenAIUsage extracts usage from a non-streaming OpenAI response,
// accepting both the chat/completions and Responses API shapes — the
// latter is what Codex and other /v1/responses clients return.
func parseOpenAIUsage(body []byte) usage.Record {
	var resp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
		Usage  *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			// Responses API spellings, on the same object.
			InputTokens        int64 `json:"input_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
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
	if r.InputTokens == 0 && r.OutputTokens == 0 {
		r.InputTokens = resp.Usage.InputTokens
		r.OutputTokens = resp.Usage.OutputTokens
		r.CacheRead = resp.Usage.InputTokensDetails.CachedTokens
	}
	if resp.Model != "" {
		r.Model = resp.Model
	}
	return r
}

// ExtractModel reads the model field from the request body without
// consuming it. The body is restored for downstream handlers.
func ExtractModel(r *http.Request) string {
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

// StatusRecorder wraps an http.ResponseWriter to record the response
// status and byte count for request logging.
type StatusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

// NewStatusRecorder wraps rw, defaulting the status to 200.
func NewStatusRecorder(rw http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: rw, status: http.StatusOK}
}

// Status returns the recorded response status.
func (s *StatusRecorder) Status() int { return s.status }

// Bytes returns the number of response body bytes written.
func (s *StatusRecorder) Bytes() int64 { return s.bytes }

func (s *StatusRecorder) WriteHeader(c int) { s.status = c; s.ResponseWriter.WriteHeader(c) }

func (s *StatusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *StatusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *StatusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
