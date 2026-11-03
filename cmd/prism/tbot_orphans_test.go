//go:build !windows

package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/tbot"
)

func TestTbotsOnConfig(t *testing.T) {
	yaml := "/Users/x/.config/prism/tbot/tbot.yaml"
	procs := map[int]string{
		30: "tbot start -c " + yaml + " --diag-addr 127.0.0.1:7332",
		10: "/opt/homebrew/bin/tbot start -c " + yaml,
		20: "tbot start -c /etc/other/tbot.yaml",
	}
	if got := tbotsOnConfig(procs, yaml); !slices.Equal(got, []int{10, 30}) {
		t.Errorf("tbotsOnConfig = %v, want [10 30]", got)
	}
}

func TestIsPrismDaemon(t *testing.T) {
	for cmd, want := range map[string]bool{
		"/opt/homebrew/opt/prism/bin/prism __daemon": true,
		"/home/u/bin/prism-linux-amd64 __daemon":     true,
		"prism __daemon --debug":                     true,
		"/opt/homebrew/opt/prism/bin/prism up":       false,
		"/usr/bin/python3 __daemon":                  false,
		"prism":                                      false,
		"":                                           false,
	} {
		if got := isPrismDaemon(cmd); got != want {
			t.Errorf("isPrismDaemon(%q) = %v, want %v", cmd, got, want)
		}
	}
}

// fakeTbotSource is a stand-in for tbot: it sleeps, and dies on SIGTERM
// unless told to ignore it.
const fakeTbotSource = `package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if os.Getenv("FAKE_TBOT_IGNORE_TERM") != "" {
		signal.Ignore(syscall.SIGTERM)
	}
	time.Sleep(time.Minute)
}
`

// buildFakeTbot compiles fakeTbotSource into a binary named tbot, which
// is what pgrep -x matches on. It is built rather than copied: macOS
// kills a copy of a system binary like sleep on launch.
func buildFakeTbot(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain to build a fake tbot")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(fakeTbotSource), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "tbot")
	if out, err := exec.Command(gobin, "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build fake tbot: %v\n%s", err, out)
	}
	return bin
}

// fakeTbot starts bin with the argv prism gives tbot, so the real lookup
// finds it on yaml. Its exit is reaped in the background so a stopped
// one doesn't linger as a zombie, which would still answer signal 0.
func fakeTbot(t *testing.T, bin, yaml string, ignoreTerm bool) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "start", "-c", yaml)
	if ignoreTerm {
		cmd.Env = append(os.Environ(), "FAKE_TBOT_IGNORE_TERM=1")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		procs, _ := tbotProcesses()
		if slices.Contains(tbotsOnConfig(procs, yaml), cmd.Process.Pid) {
			if ignoreTerm {
				// Let it reach signal.Ignore before anyone sends TERM.
				time.Sleep(200 * time.Millisecond)
			}
			return cmd
		}
	}
	t.Fatal("the fake tbot never showed up to pgrep/ps")
	return nil
}

// End to end through pgrep and ps: a tbot left on prism's config is
// stopped, and one on another config is left alone.
func TestStopOrphanedTbots(t *testing.T) {
	bin := buildFakeTbot(t)
	dir := t.TempDir()
	mine := fakeTbot(t, bin, tbot.TbotYAMLPath(dir), false)
	theirs := fakeTbot(t, bin, filepath.Join(t.TempDir(), "tbot.yaml"), false)

	var logs bytes.Buffer
	if err := stopOrphanedTbots(dir, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("stopOrphanedTbots: %v\n%s", err, logs.String())
	}
	if !waitGone(mine.Process.Pid, 3*time.Second) {
		t.Error("the tbot on prism's config is still running")
	}
	if !processAlive(theirs.Process.Pid) {
		t.Error("a tbot on another config was stopped — that one is none of prism's business")
	}
	if !strings.Contains(logs.String(), "left running on prism's tbot.yaml") {
		t.Errorf("stopping it should be logged:\n%s", logs.String())
	}
}

// tbot is asked first, so it can finish a renewal; one that won't go
// is killed once the grace runs out.
func TestStopProcessEscalates(t *testing.T) {
	bin := buildFakeTbot(t)
	polite := fakeTbot(t, bin, "/nowhere/a.yaml", false)
	start := time.Now()
	if err := stopProcess(polite.Process.Pid, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("a process that honours SIGTERM took %s to stop", d)
	}

	stubborn := fakeTbot(t, bin, "/nowhere/b.yaml", true)
	start = time.Now()
	if err := stopProcess(stubborn.Process.Pid, 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 300*time.Millisecond {
		t.Errorf("stopped in %s: it was killed without waiting out the grace", d)
	}
	if processAlive(stubborn.Process.Pid) {
		t.Error("a process ignoring SIGTERM survived")
	}
}
