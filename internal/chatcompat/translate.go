package chatcompat

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// chatOnlyFields are chat/completions parameters the Responses API has no
// equivalent for. They're dropped rather than forwarded, since forwarding
// them just earns an "Unsupported parameter" rejection.
var chatOnlyFields = []string{
	"n", "stop", "logprobs", "top_logprobs", "logit_bias",
	"seed", "stream_options", "response_format",
}

// translateRequest converts a chat/completions request body into a
// Responses API request body. Fields it doesn't recognise are passed
// through untouched — the gateway is the authority on what it accepts,
// and the handler's adaptive retry drops whatever gets rejected. That's
// what keeps model-specific knowledge out of prism.
func translateRequest(chat map[string]any, logger *log.Logger, debug bool) (map[string]any, error) {
	out := make(map[string]any, len(chat))
	for k, v := range chat {
		out[k] = v
	}

	input, err := translateMessages(chat["messages"])
	if err != nil {
		return nil, err
	}
	delete(out, "messages")
	out["input"] = input

	// Both chat spellings collapse onto max_output_tokens; the newer
	// max_completion_tokens wins if a client sent both.
	if v, ok := out["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := out["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	delete(out, "max_completion_tokens")
	delete(out, "max_tokens")

	if v, ok := out["reasoning_effort"]; ok {
		out["reasoning"] = map[string]any{"effort": v}
		delete(out, "reasoning_effort")
	}

	var dropped []string
	for _, k := range chatOnlyFields {
		if _, ok := out[k]; ok {
			delete(out, k)
			dropped = append(dropped, k)
		}
	}
	if debug && len(dropped) > 0 {
		logger.Printf("chatcompat: dropped chat-only fields: %s", strings.Join(dropped, ", "))
	}

	return out, nil
}

// translateMessages converts chat `messages` into Responses `input`.
func translateMessages(v any) ([]any, error) {
	if v == nil {
		return nil, fmt.Errorf("missing required field: messages")
	}
	msgs, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("messages must be an array")
	}
	out := make([]any, 0, len(msgs))
	for i, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("messages[%d] must be an object", i)
		}
		role, _ := mm["role"].(string)
		if role == "" {
			return nil, fmt.Errorf("messages[%d] missing role", i)
		}
		// Assistant turns are prior output, so their text parts have to
		// be tagged output_text; everything else is input_text.
		textType := "input_text"
		if role == "assistant" {
			textType = "output_text"
		}
		parts, err := translateContent(mm["content"], textType)
		if err != nil {
			return nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		out = append(out, map[string]any{"role": role, "content": parts})
	}
	return out, nil
}

// translateContent converts a chat message's content — a bare string or
// an array of parts — into Responses content parts.
func translateContent(v any, textType string) ([]any, error) {
	switch c := v.(type) {
	case nil:
		return []any{}, nil
	case string:
		return []any{map[string]any{"type": textType, "text": c}}, nil
	case []any:
		parts := make([]any, 0, len(c))
		for _, raw := range c {
			part, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content parts must be objects")
			}
			switch part["type"] {
			case "text", "input_text", "output_text":
				text, _ := part["text"].(string)
				parts = append(parts, map[string]any{"type": textType, "text": text})
			case "image_url":
				url, err := imageURL(part["image_url"])
				if err != nil {
					return nil, err
				}
				parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
			case "input_image":
				parts = append(parts, part)
			default:
				return nil, fmt.Errorf("unsupported content part type %v", part["type"])
			}
		}
		return parts, nil
	default:
		return nil, fmt.Errorf("content must be a string or an array of parts")
	}
}

func imageURL(v any) (string, error) {
	switch u := v.(type) {
	case string:
		return u, nil
	case map[string]any:
		s, _ := u["url"].(string)
		if s == "" {
			return "", fmt.Errorf("image_url missing url")
		}
		return s, nil
	default:
		return "", fmt.Errorf("image_url must be a string or an object")
	}
}

// --- Responses API reply shapes ---

type responsePayload struct {
	ID                string `json:"id"`
	CreatedAt         int64  `json:"created_at"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Output []struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *responseUsage `json:"usage"`
}

type responseUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// text returns the assistant-visible text, concatenated across output
// items. Reasoning items are skipped: they carry no plaintext content,
// only an encrypted blob.
func (r *responsePayload) text() string {
	var sb strings.Builder
	for _, item := range r.Output {
		if item.Type != "message" {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "output_text" {
				sb.WriteString(part.Text)
			}
		}
	}
	return sb.String()
}

// finishReason maps a Responses status onto a chat finish_reason.
func (r *responsePayload) finishReason() string {
	if r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "max_output_tokens" {
		return "length"
	}
	return "stop"
}

func (r *responsePayload) chatUsage() *chatUsage {
	if r.Usage == nil {
		return nil
	}
	u := &chatUsage{
		PromptTokens:     r.Usage.InputTokens,
		CompletionTokens: r.Usage.OutputTokens,
		TotalTokens:      r.Usage.TotalTokens,
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if c := r.Usage.InputTokensDetails.CachedTokens; c > 0 {
		u.PromptTokensDetails = &promptTokensDetails{CachedTokens: c}
	}
	if c := r.Usage.OutputTokensDetails.ReasoningTokens; c > 0 {
		u.CompletionTokensDetails = &completionTokensDetails{ReasoningTokens: c}
	}
	return u
}

// --- chat/completions reply shapes ---

type chatCompletion struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   *chatUsage   `json:"usage,omitempty"`
}

type chatChoice struct {
	Index        int          `json:"index"`
	Message      *chatMessage `json:"message,omitempty"`
	Delta        *chatMessage `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

type chatMessage struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

type chatUsage struct {
	PromptTokens            int64                    `json:"prompt_tokens"`
	CompletionTokens        int64                    `json:"completion_tokens"`
	TotalTokens             int64                    `json:"total_tokens"`
	PromptTokensDetails     *promptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *completionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type promptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type completionTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// translateResponse converts a non-streaming Responses reply into a
// chat/completions reply.
func translateResponse(body []byte) ([]byte, error) {
	var resp responsePayload
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse Responses reply: %w", err)
	}
	text := resp.text()
	reason := resp.finishReason()
	cc := chatCompletion{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.CreatedAt,
		Model:   resp.Model,
		Choices: []chatChoice{{
			Index:        0,
			Message:      &chatMessage{Role: "assistant", Content: &text},
			FinishReason: &reason,
		}},
		Usage: resp.chatUsage(),
	}
	return json.Marshal(cc)
}
