package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/webvictim/prism/internal/state"
)

const (
	defaultAnthropicTestModel = "claude-haiku-4-5"
	defaultOpenAITestModel    = "gpt-4o"
	defaultTestPrompt         = "say hi in three words"
)

func cmdTest(args []string) error {
	// First arg (if it doesn't start with -) selects which backend.
	target := "all"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		target = args[0]
		args = args[1:]
	}
	switch target {
	case "all":
		s, err := loadActiveSession()
		if err != nil {
			return err
		}
		anthErr := testAnthropic(s, defaultAnthropicTestModel, defaultTestPrompt)
		openaiErr := testOpenAI(s, defaultOpenAITestModel, defaultTestPrompt)
		if anthErr != nil {
			return anthErr
		}
		return openaiErr
	case "anthropic":
		return runTestSubcommand(args, "anthropic", defaultAnthropicTestModel, testAnthropic)
	case "openai":
		return runTestSubcommand(args, "openai", defaultOpenAITestModel, testOpenAI)
	default:
		return fmt.Errorf("test: unknown target %q (want anthropic|openai)", target)
	}
}

func runTestSubcommand(args []string, name, defaultModel string, fn func(*state.State, string, string) error) error {
	fs := flag.NewFlagSet("test "+name, flag.ExitOnError)
	prompt := fs.String("prompt", defaultTestPrompt, "prompt to send")
	model := fs.String("model", defaultModel, "model name")
	_ = fs.Parse(args)

	s, err := loadActiveSession()
	if err != nil {
		return err
	}
	return fn(s, *model, *prompt)
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

func testAnthropic(s *state.State, model, prompt string) error {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 40,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/messages", s.LocalPort)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	return runProbe("anthropic", req)
}

func testOpenAI(s *state.State, model, prompt string) error {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 40,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", s.LocalPort)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer prism")
	return runProbe("openai", req)
}

func runProbe(label string, req *http.Request) error {
	start := time.Now()
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(resp.Body)
	fmt.Fprintf(os.Stderr, "→ [%s] POST %s [%d, %s]\n", label, req.URL.Path, resp.StatusCode, time.Since(start).Round(time.Millisecond))
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
