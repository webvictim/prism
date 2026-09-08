package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/state"
)

// defaultTestPrompt is the probe prompt. There is deliberately no default
// model: omitting `model` lets the gateway pick and report its own
// default, which keeps model names out of prism entirely.
const defaultTestPrompt = "say hi in three words"

// Probe formats. These name wire protocols, not models.
const (
	formatAnthropic         = "anthropic"
	formatOpenAIResponses   = "openai-responses"
	formatOpenAICompletions = "openai-completions"
)

var allFormats = []string{formatAnthropic, formatOpenAICompletions, formatOpenAIResponses}

func cmdTest(args []string) error {
	// First arg (if it doesn't start with -) selects which backend.
	target := "all"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		target = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet("test", flag.ExitOnError)
	prompt := fs.String("prompt", defaultTestPrompt, "prompt to send")
	model := fs.String("model", "", "model name (default: let the gateway choose)")
	format := fs.String("format", "", "wire format: "+strings.Join(allFormats, "|"))
	stream := fs.Bool("stream", false, "request a streaming response")
	if err := fs.Parse(args); err != nil {
		return err
	}

	formats, err := resolveFormats(target, *format)
	if err != nil {
		return err
	}

	s, err := loadActiveSession()
	if err != nil {
		return err
	}

	var firstErr error
	for _, f := range formats {
		if err := runFormatProbe(s, f, *model, *prompt, *stream); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// resolveFormats turns the positional target and --format into the list
// of probes to run. --format wins when given.
func resolveFormats(target, format string) ([]string, error) {
	if format != "" {
		for _, f := range allFormats {
			if format == f {
				return []string{f}, nil
			}
		}
		return nil, fmt.Errorf("test: unknown format %q (want %s)", format, strings.Join(allFormats, "|"))
	}
	switch target {
	case "all":
		return allFormats, nil
	case "anthropic":
		return []string{formatAnthropic}, nil
	case "openai":
		return []string{formatOpenAICompletions, formatOpenAIResponses}, nil
	default:
		return nil, fmt.Errorf("test: unknown target %q (want anthropic|openai|all)", target)
	}
}

func runFormatProbe(s *state.State, format, model, prompt string, stream bool) error {
	var path string
	body := map[string]any{}
	// Omitting `model` is meaningful: the gateway picks its default and
	// names it in the reply, so `prism test` reports what's actually
	// being served without prism hardcoding a name.
	if model != "" {
		body["model"] = model
	}
	if stream {
		body["stream"] = true
	}

	switch format {
	case formatAnthropic:
		path = "/v1/messages"
		body["max_tokens"] = 40
		body["messages"] = []map[string]string{{"role": "user", "content": prompt}}
	case formatOpenAICompletions:
		path = "/v1/chat/completions"
		body["max_tokens"] = 40
		body["messages"] = []map[string]string{{"role": "user", "content": prompt}}
	case formatOpenAIResponses:
		path = "/v1/responses"
		body["max_output_tokens"] = 2000
		body["input"] = prompt
	default:
		return fmt.Errorf("test: unknown format %q", format)
	}

	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", s.LocalPort, path)
	req, err := http.NewRequest("POST", url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if format == formatAnthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		// A dummy token, as OpenAI-compatible clients send. prism strips
		// it before forwarding — the tunnel authenticates with mTLS.
		req.Header.Set("Authorization", "Bearer prism")
	}
	return runProbe(format, req, stream)
}

func loadActiveSession() (*state.State, error) {
	s, err := state.Load()
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, fmt.Errorf("no active session — run `prism up` first")
	}
	return s, nil
}

func runProbe(label string, req *http.Request, stream bool) error {
	start := time.Now()
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	defer resp.Body.Close()

	if stream && resp.StatusCode == 200 {
		return reportStream(label, req, resp, start)
	}

	rb, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(os.Stderr, "→ [%s] POST %s [%d, %s]%s\n",
		label, req.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond),
		modelSuffix(modelFromReply(rb)))
	if resp.StatusCode != 200 {
		fmt.Fprintln(os.Stderr, string(rb))
		return fmt.Errorf("%s: non-200 from upstream", label)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, rb, "", "  "); err != nil {
		os.Stdout.Write(rb)
	} else {
		pretty.WriteTo(os.Stdout)
		fmt.Println()
	}
	return nil
}

// reportStream consumes an SSE reply, printing the assembled text and a
// summary. This is the CLI check on the chat/completions shim's stream
// translation.
func reportStream(label string, req *http.Request, resp *http.Response, start time.Time) error {
	var text strings.Builder
	events, chunks := 0, 0
	model := ""
	// Terminal marker differs by protocol: chat/completions ends with
	// `data: [DONE]`, Anthropic with message_stop, the Responses API with
	// response.completed.
	complete := false

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		events++
		if data == "[DONE]" {
			complete = true
			continue
		}
		switch eventType([]byte(data)) {
		case "message_stop", "response.completed", "response.incomplete":
			// Anthropic and raw Responses streams have no [DONE].
			complete = true
		}
		if m := modelFromReply([]byte(data)); m != "" {
			model = m
		}
		if delta := streamDelta([]byte(data)); delta != "" {
			chunks++
			text.WriteString(delta)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: read stream: %w", label, err)
	}

	fmt.Fprintf(os.Stderr, "→ [%s] POST %s [%d, %s]%s stream: %d events, %d text chunks, complete=%v\n",
		label, req.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond),
		modelSuffix(model), events, chunks, complete)
	fmt.Println(text.String())
	if !complete {
		return fmt.Errorf("%s: stream ended without a terminal event", label)
	}
	return nil
}

// streamDelta pulls the incremental text out of one SSE payload, in any
// of the three formats prism can be asked to probe.
func streamDelta(data []byte) string {
	var ev struct {
		// chat/completions chunk
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
		// Anthropic content_block_delta / Responses output_text.delta
		Delta json.RawMessage `json:"delta"`
		Type  string          `json:"type"`
	}
	if err := json.Unmarshal(data, &ev); err != nil {
		return ""
	}
	if len(ev.Choices) > 0 && ev.Choices[0].Delta.Content != "" {
		return ev.Choices[0].Delta.Content
	}
	if ev.Type == "response.output_text.delta" {
		var s string
		if json.Unmarshal(ev.Delta, &s) == nil {
			return s
		}
	}
	if ev.Type == "content_block_delta" {
		var d struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(ev.Delta, &d) == nil {
			return d.Text
		}
	}
	return ""
}

// eventType reads the `type` field of an SSE payload.
func eventType(data []byte) string {
	var e struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &e) != nil {
		return ""
	}
	return e.Type
}

// modelFromReply reports the model the gateway actually served, which may
// differ from what was asked for — unknown names are silently aliased to
// the gateway's current default.
func modelFromReply(body []byte) string {
	var r struct {
		Model    string `json:"model"`
		Response *struct {
			Model string `json:"model"`
		} `json:"response"`
		Message *struct {
			Model string `json:"model"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return ""
	}
	if r.Model != "" {
		return r.Model
	}
	if r.Response != nil && r.Response.Model != "" {
		return r.Response.Model
	}
	if r.Message != nil && r.Message.Model != "" {
		return r.Message.Model
	}
	return ""
}

func modelSuffix(model string) string {
	if model == "" {
		return ""
	}
	return " model=" + model
}
