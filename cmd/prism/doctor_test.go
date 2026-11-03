package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// The daemon log is where the reason a join failed actually lands —
// tbot's diag endpoint only ever says "initializing" — so this reader
// has to find tbot's own last error and nothing else.
func TestLastTbotError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.log")
	log := strings.Join([]string{
		`2026/11/03 09:39:19 POST /v1/messages 200 req=12B resp=34B model=a in=1 out=2 cache_read=0 cache_write=0 0.3s`,
		`2026/11/03 09:40:25 tbot: 2026-11-03T09:40:25.000-04:00 ERRO [TBOT] something earlier`,
		`2026/11/03 09:40:29 tunnel(anthropic): health probe failed: health 503 (failures=7)`,
		`2026/11/03 09:40:30 tbot: Original Error: trace.aggregate could not lookup signer for public key`,
		`2026/11/03 09:40:31 POST /v1/messages 502 req=12B resp=99B model=? in=0 out=0 cache_read=0 cache_write=0 0.1s`,
	}, "\n")
	if err := os.WriteFile(path, []byte(log+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := lastTbotError(path)
	if want := "trace.aggregate could not lookup signer for public key"; got != want {
		t.Errorf("lastTbotError = %q, want %q", got, want)
	}

	// Nothing from tbot: the request log and the tunnel supervisor's
	// own lines are not tbot errors, and quoting them would send
	// someone chasing the wrong thing.
	quiet := filepath.Join(dir, "quiet.log")
	if err := os.WriteFile(quiet, []byte("2026/11/03 09:39:19 POST /v1/messages 200\n2026/11/03 09:40:29 tunnel(anthropic): health probe failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastTbotError(quiet); got != "" {
		t.Errorf("lastTbotError = %q, want \"\" when tbot logged no error", got)
	}

	if got := lastTbotError(filepath.Join(dir, "absent.log")); got != "" {
		t.Errorf("lastTbotError on a missing file = %q, want \"\"", got)
	}

	// Only the tail of a large log is read, so an error in the last
	// window is still found while an ancient one is not.
	big := filepath.Join(dir, "big.log")
	var b strings.Builder
	b.WriteString("2026/11/03 00:00:00 tbot: Original Error: ancient failure\n")
	for b.Len() < 200<<10 {
		b.WriteString("2026/11/03 09:00:00 POST /v1/messages 200 req=1B resp=1B\n")
	}
	b.WriteString("2026/11/03 09:40:30 tbot: Original Error: recent failure\n")
	if err := os.WriteFile(big, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastTbotError(big); got != "recent failure" {
		t.Errorf("lastTbotError = %q, want the recent failure", got)
	}
}
