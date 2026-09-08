package chatcompat

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// responsesSSE mirrors what the live gateway streams: an event: line, a
// data: line carrying the same name in `type`, and a blank separator.
// The reasoning item and the `obfuscation` field are both present and
// both irrelevant to the translation.
const responsesSSE = `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_abc","created_at":1788888186,"model":"openai.gpt-5.6-sol","status":"in_progress","output":[]}}

event: response.in_progress
data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_abc","model":"openai.gpt-5.6-sol"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","content":[],"encrypted_content":"rsn_xxx"}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":1,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","content_index":0,"delta":"Four","obfuscation":"abc","output_index":1,"sequence_number":6}

event: response.output_text.delta
data: {"type":"response.output_text.delta","content_index":0,"delta":", five","obfuscation":"abcdef","output_index":1,"sequence_number":7}

event: response.output_text.done
data: {"type":"response.output_text.done","text":"Four, five"}

event: response.completed
data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_abc","created_at":1788888186,"model":"openai.gpt-5.6-sol","status":"completed","incomplete_details":null,"output":[{"type":"message","content":[{"type":"output_text","text":"Four, five"}]}],"usage":{"input_tokens":11,"output_tokens":52,"total_tokens":63,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":40}}}}

`

type sseChunk struct {
	Object  string `json:"object"`
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int     `json:"index"`
		FinishReason *string `json:"finish_reason"`
		Delta        struct {
			Role    string  `json:"role"`
			Content *string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
}

// runStream translates a fixture and returns the emitted chunks plus
// whether the stream terminated with [DONE].
func runStream(t *testing.T, fixture string) ([]sseChunk, bool, *httptest.ResponseRecorder) {
	t.Helper()
	h := New(1, discardLogger(), false, true)
	rec := httptest.NewRecorder()
	h.translateStream(rec, strings.NewReader(fixture))

	var chunks []sseChunk
	sawDone := false
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			sawDone = true
			continue
		}
		var c sseChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			t.Fatalf("emitted invalid chunk JSON %q: %v", data, err)
		}
		chunks = append(chunks, c)
	}
	return chunks, sawDone, rec
}

func TestStreamTranslation(t *testing.T) {
	chunks, sawDone, rec := runStream(t, responsesSSE)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !sawDone {
		t.Error("stream did not end with [DONE]")
	}
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4 (role, two deltas, final): %+v", len(chunks), chunks)
	}

	for i, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("chunk %d object = %q", i, c.Object)
		}
		if c.ID != "resp_abc" || c.Model != "openai.gpt-5.6-sol" || c.Created != 1788888186 {
			t.Errorf("chunk %d envelope = id=%q model=%q created=%d", i, c.ID, c.Model, c.Created)
		}
	}

	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk should carry the role, got %+v", chunks[0].Choices[0].Delta)
	}

	var text strings.Builder
	for _, c := range chunks {
		if d := c.Choices[0].Delta.Content; d != nil {
			text.WriteString(*d)
		}
	}
	if text.String() != "Four, five" {
		t.Errorf("assembled text = %q, want %q", text.String(), "Four, five")
	}

	final := chunks[len(chunks)-1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Errorf("final finish_reason = %v, want stop", final.Choices[0].FinishReason)
	}
	// Usage must ride the final chunk unconditionally so the router's
	// capture middleware records it.
	if final.Usage == nil || final.Usage.PromptTokens != 11 || final.Usage.CompletionTokens != 52 {
		t.Errorf("final usage = %+v, want 11/52", final.Usage)
	}
	for i, c := range chunks[:len(chunks)-1] {
		if c.Choices[0].FinishReason != nil {
			t.Errorf("chunk %d has a finish_reason before the end", i)
		}
	}
}

func TestStreamTruncationIsLength(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.output_text.delta","delta":"cut"}

data: {"type":"response.incomplete","response":{"id":"r","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}

`
	chunks, sawDone, _ := runStream(t, fixture)
	if !sawDone {
		t.Error("stream did not end with [DONE]")
	}
	final := chunks[len(chunks)-1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want length", final.Choices[0].FinishReason)
	}
}

// A stream that dies without a terminal event must still be closed, or
// clients hang waiting for [DONE].
func TestStreamWithoutTerminalEventStillCloses(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.output_text.delta","delta":"partial"}

`
	_, sawDone, _ := runStream(t, fixture)
	if !sawDone {
		t.Error("truncated stream must still emit [DONE]")
	}
}

func TestStreamFailureEventClosesStream(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"r","model":"m"}}

data: {"type":"response.failed","response":{"id":"r","error":{"message":"boom"}}}

`
	_, sawDone, _ := runStream(t, fixture)
	if !sawDone {
		t.Error("failed stream must emit [DONE]")
	}
}

// A delta arriving before response.created still needs the role chunk.
func TestStreamRoleEmittedEvenWithoutCreatedEvent(t *testing.T) {
	fixture := `data: {"type":"response.output_text.delta","delta":"hi"}

data: {"type":"response.completed","response":{"id":"r","model":"m","status":"completed"}}

`
	chunks, _, _ := runStream(t, fixture)
	if len(chunks) < 2 || chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("expected a leading role chunk, got %+v", chunks)
	}
}
