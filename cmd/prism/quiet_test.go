package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/state"
	usagepkg "github.com/webvictim/prism/internal/usage"
)

// asPrismEnv makes the test binary run prism's main, so a test can run
// the real command line in a child and see exactly what reaches stdout.
const asPrismEnv = "PRISM_TEST_AS_PRISM"

func TestMain(m *testing.M) {
	if os.Getenv(asPrismEnv) == "1" {
		// Never become a daemon that would outlive the test.
		if len(os.Args) > 1 && os.Args[1] == daemonArg {
			os.Exit(99)
		}
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestQuietOnlyForOne(t *testing.T) {
	for value, want := range map[string]bool{"1": true, "": false, "0": false, "true": false, "yes": false} {
		t.Setenv("PRISM_QUIET", value)
		if got := quiet(); got != want {
			t.Errorf("PRISM_QUIET=%q: quiet() = %v, want %v", value, got, want)
		}
	}
}

// `prism env` exists to print the exports, so quiet doesn't silence it.
func TestQuietExports(t *testing.T) {
	sb := newSandbox(t)
	port := sb.running(t)
	exports := fmt.Sprintf(`export ANTHROPIC_BASE_URL=http://127.0.0.1:%[1]d
export ANTHROPIC_AUTH_TOKEN=teleport
unset ANTHROPIC_API_KEY
export OPENAI_BASE_URL=http://127.0.0.1:%[1]d/v1
export OPENAI_API_KEY=teleport
unset CLAUDE_CODE_USE_MANTLE
unset CLAUDE_CODE_SKIP_MANTLE_AUTH
unset ANTHROPIC_BEDROCK_MANTLE_BASE_URL
export CLAUDE_CODE_AUTO_MODE_SERVER="${CLAUDE_CODE_AUTO_MODE_SERVER:-0}"
`, port)

	for _, tt := range []struct {
		name  string
		cmd   string
		quiet bool
		want  string
	}{
		{name: "up", cmd: "up", want: exports},
		{name: "quiet up", cmd: "up", quiet: true, want: ""},
		{name: "env", cmd: "env", want: exports},
		{name: "quiet env", cmd: "env", quiet: true, want: exports},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run := sb.prism(t, tt.quiet, tt.cmd)
			if run.code != 0 {
				t.Fatalf("exit %d; stderr:\n%s", run.code, run.stderr)
			}
			if run.stdout != tt.want {
				t.Errorf("stdout:\n%s\nwant:\n%s", run.stdout, tt.want)
			}
			if tt.cmd == "up" && !strings.Contains(run.stderr, "prism: already running") {
				t.Errorf("stderr does not say prism is already running:\n%s", run.stderr)
			}
		})
	}
}

func TestQuietLaunchLeavesStdoutToTheTool(t *testing.T) {
	sb := newSandbox(t)
	sb.running(t)
	sb.fake(t, "claude", `printf 'claude args=[%s] stdin=[%s]\n' "$*" "$(cat)"`+"\n")

	// A recent model swap, so prism has a notice to print.
	dir, err := usagepkg.Dir()
	if err != nil {
		t.Fatal(err)
	}
	w, err := usagepkg.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(usagepkg.Record{Model: "claude-sonnet-5", RequestedModel: "claude-opus-5-5", Backend: "anthropic"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	const want = "claude args=[-p --output-format stream-json] stdin=[from-caller]\n"
	for _, quiet := range []bool{false, true} {
		t.Run(fmt.Sprintf("quiet=%v", quiet), func(t *testing.T) {
			run := sb.prism(t, quiet, "claude", "-p", "--output-format", "stream-json")
			if run.code != 0 {
				t.Fatalf("exit %d; stderr:\n%s", run.code, run.stderr)
			}
			if run.stdout != want {
				t.Errorf("stdout:\n%s\nwant:\n%s", run.stdout, want)
			}
			if !strings.Contains(run.stderr, "served a different model") {
				t.Errorf("stderr has no substitution notice:\n%s", run.stderr)
			}
		})
	}
}

func TestQuietColdStartTshLogin(t *testing.T) {
	for _, tt := range []struct {
		quiet      bool
		wantStdout string
		wantStderr string
	}{
		{quiet: false, wantStdout: "tsh app login anthropic stdin=[from-caller]\n"},
		{quiet: true, wantStderr: "tsh app login anthropic stdin=[]\n"},
	} {
		t.Run(fmt.Sprintf("quiet=%v", tt.quiet), func(t *testing.T) {
			sb := newSandbox(t)
			// Failing the login stops prism before it starts a daemon. A
			// stdin that was closed rather than empty would exit 3.
			sb.fake(t, "tsh", `in=$(cat) || exit 3
printf 'tsh %s stdin=[%s]\n' "$*" "$in"
exit 1
`)
			sb.fake(t, "tool", "echo tool ran\n")

			run := sb.prism(t, tt.quiet, "exec", "tool")
			if run.code != 1 {
				t.Errorf("exit %d, want 1", run.code)
			}
			if run.stdout != tt.wantStdout {
				t.Errorf("stdout:\n%s\nwant:\n%s", run.stdout, tt.wantStdout)
			}
			if !strings.Contains(run.stderr, tt.wantStderr) {
				t.Errorf("stderr does not contain %q:\n%s", tt.wantStderr, run.stderr)
			}
			if !strings.Contains(run.stderr, "prism: app login anthropic: exit status 1\n") {
				t.Errorf("stderr does not report the failed login:\n%s", run.stderr)
			}
		})
	}
}

// `prism pi` runs `pi update --models` first when Pi has no catalog.
func TestQuietPiCatalogRefresh(t *testing.T) {
	fixtures := t.TempDir()
	writePiStoreFixture(t, fixtures)

	for _, tt := range []struct {
		quiet      bool
		wantStdout string
		wantStderr string
	}{
		{quiet: false, wantStdout: "pi update stdin=[from-caller]\npi args=[-c] stdin=[]\n"},
		{quiet: true, wantStdout: "pi args=[-c] stdin=[from-caller]\n", wantStderr: "pi update stdin=[]\n"},
	} {
		t.Run(fmt.Sprintf("quiet=%v", tt.quiet), func(t *testing.T) {
			sb := newSandbox(t)
			sb.running(t)
			sb.env = append(sb.env,
				"PI_CODING_AGENT_DIR="+t.TempDir(),
				"PI_STORE_FIXTURE="+filepath.Join(fixtures, "models-store.json"))
			sb.fake(t, "pi", `if [ "$1" = update ]; then
	in=$(cat) || exit 3
	printf 'pi update stdin=[%s]\n' "$in"
	cp "$PI_STORE_FIXTURE" "$PI_CODING_AGENT_DIR/models-store.json" || exit 1
	exit 0
fi
printf 'pi args=[%s] stdin=[%s]\n' "$*" "$(cat)"
`)

			run := sb.prism(t, tt.quiet, "pi", "-c")
			if run.code != 0 {
				t.Fatalf("exit %d; stderr:\n%s", run.code, run.stderr)
			}
			if run.stdout != tt.wantStdout {
				t.Errorf("stdout:\n%s\nwant:\n%s", run.stdout, tt.wantStdout)
			}
			if !strings.Contains(run.stderr, tt.wantStderr) {
				t.Errorf("stderr does not contain %q:\n%s", tt.wantStderr, run.stderr)
			}
		})
	}
}

// callerStdin is meant for the tool, so nothing prism runs first may
// read it.
const callerStdin = "from-caller\n"

type sandbox struct {
	bin string
	env []string
}

// newSandbox gives prism its own HOME and config dir, so no test can
// find the real LaunchAgent, systemd unit or session.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	home := t.TempDir()
	configHome := filepath.Join(home, ".config")
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	// The test process writes state, config and usage through prism's own
	// packages too.
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", configHome)
	// On auto, the exports would depend on this machine's managed settings.
	if err := config.Save(&config.Config{ClaudeAuth: claudeAuthToken}); err != nil {
		t.Fatal(err)
	}
	return &sandbox{bin: bin, env: []string{
		asPrismEnv + "=1",
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + configHome,
		"PATH=" + bin + ":/usr/bin:/bin",
	}}
}

func (s *sandbox) fake(t *testing.T, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		t.Fatal(err)
	}
}

// running records a session whose router port answers, which is all
// prism checks to treat the daemon as up.
func (s *sandbox) running(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := l.Addr().(*net.TCPAddr).Port
	if err := state.Save(&state.State{LocalPort: port, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	return port
}

type prismRun struct {
	stdout, stderr string
	code           int
}

func (s *sandbox) prism(t *testing.T, quiet bool, args ...string) prismRun {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A child left waiting on a prompt must fail the test, not hang it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Env = append([]string(nil), s.env...)
	if quiet {
		cmd.Env = append(cmd.Env, "PRISM_QUIET=1")
	}
	cmd.Stdin = strings.NewReader(callerStdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("prism %s did not finish; stderr:\n%s", strings.Join(args, " "), stderr.String())
	}
	run := prismRun{stdout: stdout.String(), stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		run.code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("prism %s: %v", strings.Join(args, " "), err)
	}
	return run
}
