package chatcompat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// streamEvent is the subset of a Responses SSE payload we care about.
// The `event:` line names the event too, but the JSON repeats it in
// `type`, so only `data:` lines need parsing.
type streamEvent struct {
	Type     string           `json:"type"`
	Delta    string           `json:"delta"`
	Response *responsePayload `json:"response"`
}

// translateStream pumps a Responses SSE stream to the client as
// chat/completions chunks. It flushes after every chunk so clients see
// tokens as they arrive.
func (h *Handler) translateStream(w http.ResponseWriter, upstream io.Reader) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	var id, model string
	var created int64
	sentRole := false
	done := false

	sc := bufio.NewScanner(upstream)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		var ev streamEvent
		if err := json.Unmarshal(line[6:], &ev); err != nil {
			continue
		}

		switch ev.Type {
		case "response.created":
			if ev.Response != nil {
				id, model, created = ev.Response.ID, ev.Response.Model, ev.Response.CreatedAt
			}
			// OpenAI clients expect the role to arrive in the first chunk.
			if !sentRole {
				sentRole = true
				h.writeChunk(w, chunk(id, model, created, &chatMessage{Role: "assistant"}, nil, nil))
				flush()
			}

		case "response.output_text.delta":
			if ev.Delta == "" {
				continue
			}
			if !sentRole {
				sentRole = true
				h.writeChunk(w, chunk(id, model, created, &chatMessage{Role: "assistant"}, nil, nil))
			}
			text := ev.Delta
			h.writeChunk(w, chunk(id, model, created, &chatMessage{Content: &text}, nil, nil))
			flush()

		case "response.completed", "response.incomplete":
			reason := "stop"
			var u *chatUsage
			if ev.Response != nil {
				reason = ev.Response.finishReason()
				u = ev.Response.chatUsage()
				if ev.Response.Model != "" {
					model = ev.Response.Model
				}
			}
			// Usage rides the final chunk unconditionally so the
			// router's capture middleware can record it.
			h.writeChunk(w, chunk(id, model, created, &chatMessage{}, &reason, u))
			h.writeDone(w)
			flush()
			done = true

		case "response.failed", "error":
			h.Logger.Printf("chatcompat: upstream stream error: %s", line[6:])
			h.writeDone(w)
			flush()
			done = true
		}
		if done {
			return
		}
	}
	if err := sc.Err(); err != nil {
		h.Logger.Printf("chatcompat: read upstream stream: %v", err)
	}
	// Upstream ended without a terminal event — close the stream cleanly
	// so the client doesn't hang.
	if !done {
		h.writeDone(w)
		flush()
	}
}

// chunk builds one chat.completion.chunk.
func chunk(id, model string, created int64, delta *chatMessage, finishReason *string, u *chatUsage) chatCompletion {
	return chatCompletion{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []chatChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
		Usage:   u,
	}
}

func (h *Handler) writeChunk(w http.ResponseWriter, c chatCompletion) {
	b, err := json.Marshal(c)
	if err != nil {
		h.Logger.Printf("chatcompat: marshal chunk: %v", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
}

func (h *Handler) writeDone(w http.ResponseWriter) {
	fmt.Fprint(w, "data: [DONE]\n\n")
}
