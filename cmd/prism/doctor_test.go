package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/state"
)

// The exit code is what makes `prism doctor` scriptable, and it comes
// from these counts: a warning must never fail the run, and a fail
// must never be reported as a warning.
func TestReportCounts(t *testing.T) {
	var buf bytes.Buffer
	r := &report{out: &buf}
	r.ok("a", "fine")
	r.info("b", "just so you know")
	r.warn("c", "odd", "do this")
	r.warn("d", "odd", "do that")
	r.fail("e", "broken", "fix it")

	if got := r.count(statusFail); got != 1 {
		t.Errorf("fails = %d, want 1", got)
	}
	if got := r.count(statusWarn); got != 2 {
		t.Errorf("warns = %d, want 2", got)
	}
	if got := r.count(statusOK); got != 1 {
		t.Errorf("oks = %d, want 1", got)
	}

	out := buf.String()
	// Every fix hint has to reach the reader — it is the whole point of
	// the line it hangs off.
	for _, want := range []string{"[ ok ]", "[warn]", "[FAIL]", "→ fix it", "→ do this"} {
		if !strings.Contains(out, want) {
			t.Errorf("report output is missing %q:\n%s", want, out)
		}
	}
	// An info line carries no verdict, so it must not be decorated
	// like one.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "just so you know") && strings.Contains(line, "ok") {
			t.Errorf("info line reads as a pass: %s", line)
		}
	}
}

func TestTbotDirFor(t *testing.T) {
	cfg := &config.Config{TbotDir: "/configured"}
	if got := tbotDirFor(cfg, nil); got != "/configured" {
		t.Errorf("tbotDirFor(cfg, nil) = %q, want the configured dir", got)
	}
	// A running session wins: it is the directory prism is actually
	// using, which is what a diagnosis has to describe.
	s := &state.State{TbotDir: "/running"}
	if got := tbotDirFor(cfg, s); got != "/running" {
		t.Errorf("tbotDirFor = %q, want the running session's dir", got)
	}
	if got := tbotDirFor(nil, nil); got != "" {
		t.Errorf("tbotDirFor(nil, nil) = %q, want empty", got)
	}
}

// Report lines are read in a terminal and pasted into chat, so a
// multi-line blob from a gateway or from tbot's /readyz has to collapse
// onto one line.
func TestOneLine(t *testing.T) {
	got := oneLine("  tunnel not ready:\n  {\"error\": \"join failed\"}\n\n")
	if want := `tunnel not ready: {"error": "join failed"}`; got != want {
		t.Errorf("oneLine = %q, want %q", got, want)
	}
	// Truncated to 300 characters plus the ellipsis that says so.
	if n := utf8.RuneCountInString(oneLine(strings.Repeat("x", 1000))); n != 301 {
		t.Errorf("oneLine kept %d characters, want 301", n)
	}
	if got := oneLine(""); got != "" {
		t.Errorf("oneLine(\"\") = %q", got)
	}
}
