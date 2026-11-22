package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/beam"
	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/tshwrap"
)

// beamTest implements `prism beam test`: start a beam of its own, open its
// forwards, send one request to each LLM app through it, and remove it —
// the whole path the daemon depends on, end to end, without touching the
// daemon or its beam. It is the check to run after `prism setup beams`, on
// a new cluster, or with beam_roles changed, since a narrower set of roles
// is only proven enough by inference actually working.
func beamTest(args []string) error {
	fs := flag.NewFlagSet("beam test", flag.ContinueOnError)
	keep := fs.Bool("keep", false, "leave the test beam running (it expires on its own)")
	model := fs.String("anthropic-model", "", "Anthropic model to ask (default: the probe model)")
	oaiModel := fs.String("openai-model", "", "OpenAI model to ask (default: the probe model)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	step := func(n int, format string, a ...any) {
		fmt.Fprintf(os.Stderr, "prism: [%d/6] %s\n", n, fmt.Sprintf(format, a...))
	}

	// 1. The user's own login.
	if _, ok := loginUsable(); !ok {
		if !isTerminal(os.Stdin) {
			return fmt.Errorf("[1/6] beams need your own Teleport login: run `prism login`")
		}
		fmt.Fprintln(os.Stderr, "prism: beams need your own Teleport login. Logging in…")
		if err := tshLogin(tshwrap.LoginTTLMinutes, nil); err != nil {
			return err
		}
	}
	step(1, "Teleport login %s", describeLogin())

	// 2. A beam, with the roles a daemon would give it.
	roles := "all your standing roles"
	if len(cfg.BeamRoles) > 0 {
		roles = "roles " + strings.Join(cfg.BeamRoles, ",") + " (beam_roles)"
	}
	logger := log.New(io.Discard, "", 0)
	if os.Getenv("PRISM_DEBUG") != "" {
		logger = log.New(os.Stderr, "prism: ", 0)
	}
	trial := beam.NewTrial(beam.NewTshOps(logger, func() []string { return cfg.BeamRoles }))
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	t0 := time.Now()
	b, err := trial.Start(ctx)
	if err != nil {
		return fmt.Errorf("[2/6] start a beam with %s: %w", roles, err)
	}
	step(2, "started beam %s with %s (%s)", b.ID, roles, since(t0))
	failed := false
	defer func() {
		rctx, rcancel := context.WithTimeout(context.Background(), time.Minute)
		defer rcancel()
		link := beam.Record{UUID: b.UUID, Proxy: cmp.Or(cfg.Proxy, tshwrap.Proxy())}.Link("")
		switch err := trial.Close(rctx, *keep); {
		case *keep:
			step(6, "left %s running, as asked: it expires at %s", b.ID, b.Expires.Local().Format("15:04"))
		case err != nil:
			step(6, "could not remove %s: %v — it expires at %s anyway", b.ID, err, b.Expires.Local().Format("15:04"))
		default:
			step(6, "removed %s; its summary appears in Beams in about 15 minutes", b.ID)
		}
		fmt.Fprintln(os.Stderr, "prism:       "+link)
		if !failed {
			fmt.Fprintln(os.Stderr, "prism: beam test passed: inference works through a beam.")
		}
	}()

	// 3. Its forwards.
	t0 = time.Now()
	if err := trial.Connect(ctx); err != nil {
		failed = true
		return fmt.Errorf("[3/6] %w", err)
	}
	step(3, "forwards open through %s (%s)", b.ID, since(t0))

	// 4–5. One request to each app.
	a, o := probeDefaults(cfg)
	for i, p := range []struct {
		app, path, model string
		body             map[string]any
	}{
		{"anthropic", "/v1/messages", cmp.Or(*model, a.Model), map[string]any{
			"max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "Reply with just: ok"}}}},
		{"openai", "/v1/responses", cmp.Or(*oaiModel, o.Model), map[string]any{
			"max_output_tokens": 16, "input": "Reply with just: ok"}},
	} {
		p.body["model"] = p.model
		detail, err := trialRequest(ctx, trial, p.app, p.path, p.body)
		if err != nil {
			failed = true
			hint := ""
			if len(cfg.BeamRoles) > 0 && strings.Contains(err.Error(), "403") {
				hint = " — beam_roles may not grant access to the " + p.app + " app"
			}
			return fmt.Errorf("[%d/6] %s inference through %s: %w%s", 4+i, p.app, b.ID, err, hint)
		}
		step(4+i, "%s inference ok: %s", p.app, detail)
	}
	return nil
}

// trialRequest sends one small request through the trial's beam and
// describes the answer.
func trialRequest(ctx context.Context, trial *beam.Trial, app, path string, body map[string]any) (string, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, trial.URL(app, path), bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if app == "anthropic" {
		req.Header.Set("Anthropic-Version", "2023-06-01")
	}
	setProbeHeaders(req, "beam-test")
	t0 := time.Now()
	resp, err := trial.Client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, oneLine(string(reply)))
	}
	served := modelFromReply(reply)
	if served == "" {
		served = "?"
	}
	return fmt.Sprintf("model %s, %s", served, since(t0)), nil
}

func since(t time.Time) string { return time.Since(t).Round(100 * time.Millisecond).String() }
