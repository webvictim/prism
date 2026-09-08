package chatcompat

import (
	"encoding/json"
	"io"
	"log"
	"testing"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func mustTranslate(t *testing.T, body string) map[string]any {
	t.Helper()
	var in map[string]any
	if err := json.Unmarshal([]byte(body), &in); err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	out, err := translateRequest(in, discardLogger(), false)
	if err != nil {
		t.Fatalf("translateRequest: %v", err)
	}
	return out
}

func TestTranslateStringContent(t *testing.T) {
	out := mustTranslate(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, ok := out["messages"]; ok {
		t.Error("messages should be replaced by input")
	}
	input, ok := out["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v, want 1 message", out["input"])
	}
	msg := input[0].(map[string]any)
	if msg["role"] != "user" {
		t.Errorf("role = %v, want user", msg["role"])
	}
	part := msg["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "hi" {
		t.Errorf("part = %#v, want input_text/hi", part)
	}
	if out["model"] != "m" {
		t.Errorf("model = %v, want m passed through", out["model"])
	}
}

func TestTranslateOmittedModelStaysOmitted(t *testing.T) {
	out := mustTranslate(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	if _, ok := out["model"]; ok {
		t.Error("model must stay absent so the gateway picks its own default")
	}
}

func TestTranslateAssistantTurnUsesOutputText(t *testing.T) {
	out := mustTranslate(t, `{"messages":[
		{"role":"system","content":"be terse"},
		{"role":"assistant","content":"A"},
		{"role":"user","content":"B"}]}`)
	input := out["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("got %d messages, want 3", len(input))
	}
	want := []string{"input_text", "output_text", "input_text"}
	for i, w := range want {
		msg := input[i].(map[string]any)
		part := msg["content"].([]any)[0].(map[string]any)
		if part["type"] != w {
			t.Errorf("message %d part type = %v, want %v", i, part["type"], w)
		}
	}
	if input[0].(map[string]any)["role"] != "system" {
		t.Error("system role should be preserved in input")
	}
}

func TestTranslateContentPartArray(t *testing.T) {
	out := mustTranslate(t, `{"messages":[{"role":"user","content":[
		{"type":"text","text":"one"},{"type":"text","text":"two"}]}]}`)
	parts := out["input"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2", len(parts))
	}
	if parts[1].(map[string]any)["text"] != "two" {
		t.Errorf("second part = %#v", parts[1])
	}
}

func TestTranslateImagePart(t *testing.T) {
	out := mustTranslate(t, `{"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	part := out["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_image" {
		t.Errorf("type = %v, want input_image", part["type"])
	}
	if part["image_url"] != "data:image/png;base64,AAA" {
		t.Errorf("image_url = %v", part["image_url"])
	}
}

func TestTranslateMaxTokensSpellings(t *testing.T) {
	for _, tc := range []struct{ body, name string }{
		{`{"messages":[],"max_tokens":40}`, "legacy"},
		{`{"messages":[],"max_completion_tokens":40}`, "new"},
		{`{"messages":[],"max_tokens":9,"max_completion_tokens":40}`, "both"},
	} {
		out := mustTranslate(t, tc.body)
		if got, _ := out["max_output_tokens"].(float64); got != 40 {
			t.Errorf("%s: max_output_tokens = %v, want 40", tc.name, out["max_output_tokens"])
		}
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			if _, ok := out[k]; ok {
				t.Errorf("%s: %s should be removed", tc.name, k)
			}
		}
	}
}

func TestTranslateReasoningEffort(t *testing.T) {
	out := mustTranslate(t, `{"messages":[],"reasoning_effort":"high"}`)
	r, ok := out["reasoning"].(map[string]any)
	if !ok || r["effort"] != "high" {
		t.Errorf("reasoning = %#v, want effort=high", out["reasoning"])
	}
	if _, ok := out["reasoning_effort"]; ok {
		t.Error("reasoning_effort should be removed")
	}
}

func TestTranslateDropsChatOnlyFieldsButKeepsUnknowns(t *testing.T) {
	out := mustTranslate(t, `{"messages":[],"n":1,"stop":["x"],"seed":7,
		"stream_options":{"include_usage":true},"temperature":0.4,"future_field":"keep"}`)
	for _, k := range []string{"n", "stop", "seed", "stream_options"} {
		if _, ok := out[k]; ok {
			t.Errorf("%s should be dropped", k)
		}
	}
	// Unknown and possibly-valid fields pass through; the adaptive retry
	// removes whatever the gateway actually rejects.
	if out["temperature"] != 0.4 {
		t.Errorf("temperature = %v, want passed through", out["temperature"])
	}
	if out["future_field"] != "keep" {
		t.Errorf("future_field = %v, want passed through", out["future_field"])
	}
}

func TestTranslateRejectsBadMessages(t *testing.T) {
	for _, body := range []string{
		`{"model":"m"}`,
		`{"messages":"nope"}`,
		`{"messages":[{"content":"no role"}]}`,
		`{"messages":[{"role":"user","content":[{"type":"audio"}]}]}`,
	} {
		var in map[string]any
		_ = json.Unmarshal([]byte(body), &in)
		if _, err := translateRequest(in, discardLogger(), false); err == nil {
			t.Errorf("%s: expected an error", body)
		}
	}
}

// The live gateway returns a reasoning item alongside the message item;
// its content is empty, so only the message's text may be surfaced.
const responsesReply = `{
  "id":"resp_abc","created_at":1788887874,"model":"openai.gpt-5.6-sol",
  "object":"response","status":"completed","incomplete_details":null,
  "output":[
    {"type":"reasoning","content":[],"encrypted_content":"rsn_xxx"},
    {"type":"message","role":"assistant","status":"completed",
     "content":[{"type":"output_text","text":"Hi there, friend!"}]}
  ],
  "usage":{"input_tokens":11,"output_tokens":52,"total_tokens":63,
           "input_tokens_details":{"cached_tokens":4},
           "output_tokens_details":{"reasoning_tokens":40}}
}`

func TestTranslateResponse(t *testing.T) {
	out, err := translateResponse([]byte(responsesReply))
	if err != nil {
		t.Fatalf("translateResponse: %v", err)
	}
	var cc struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Index        int    `json:"index"`
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &cc); err != nil {
		t.Fatalf("result is not valid chat/completions JSON: %v", err)
	}
	if cc.Object != "chat.completion" || cc.ID != "resp_abc" || cc.Created != 1788887874 {
		t.Errorf("envelope = %+v", cc)
	}
	if cc.Model != "openai.gpt-5.6-sol" {
		t.Errorf("model = %q, want the model the gateway served", cc.Model)
	}
	if len(cc.Choices) != 1 {
		t.Fatalf("got %d choices, want 1", len(cc.Choices))
	}
	if cc.Choices[0].Message.Content != "Hi there, friend!" {
		t.Errorf("content = %q (reasoning item must be skipped)", cc.Choices[0].Message.Content)
	}
	if cc.Choices[0].Message.Role != "assistant" || cc.Choices[0].FinishReason != "stop" {
		t.Errorf("choice = %+v", cc.Choices[0])
	}
	if cc.Usage.PromptTokens != 11 || cc.Usage.CompletionTokens != 52 || cc.Usage.TotalTokens != 63 {
		t.Errorf("usage = %+v", cc.Usage)
	}
	if cc.Usage.PromptTokensDetails.CachedTokens != 4 {
		t.Errorf("cached_tokens = %d, want 4", cc.Usage.PromptTokensDetails.CachedTokens)
	}
	if cc.Usage.CompletionTokensDetails.ReasoningTokens != 40 {
		t.Errorf("reasoning_tokens = %d, want 40", cc.Usage.CompletionTokensDetails.ReasoningTokens)
	}
}

func TestTranslateResponseTruncationIsLength(t *testing.T) {
	body := `{"id":"r","model":"m","status":"incomplete",
	          "incomplete_details":{"reason":"max_output_tokens"},
	          "output":[{"type":"message","content":[{"type":"output_text","text":"cut"}]}]}`
	out, err := translateResponse([]byte(body))
	if err != nil {
		t.Fatalf("translateResponse: %v", err)
	}
	var cc struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(out, &cc)
	if cc.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q, want length", cc.Choices[0].FinishReason)
	}
	if cc.Usage != nil {
		t.Error("usage should be omitted when the gateway reports none")
	}
}
