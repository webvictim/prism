package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/state"
)

// Each tool runs once from PATH and once through a differently named
// override, and the two runs must look the same to the tool.
func TestToolBinOverrideGetsPATHSetup(t *testing.T) {
	const router = "http://127.0.0.1:7399"

	for _, tt := range []struct {
		tool     string
		launcher func([]string) error
		args     []string
		check    func(t *testing.T, sb toolSandbox, runs []toolRun)
	}{
		{
			tool:     "claude",
			launcher: cmdClaude,
			args:     []string{"-p", "say hi"},
			check: func(t *testing.T, _ toolSandbox, runs []toolRun) {
				for key, want := range map[string]string{
					"ANTHROPIC_BASE_URL":   router,
					"ANTHROPIC_AUTH_TOKEN": claudeGatewayToken,
					autoModeServerEnv:      "0",
				} {
					if got, _ := envValue(runs[0].env, key); got != want {
						t.Errorf("%s = %q, want %q", key, got, want)
					}
				}
				if got, found := envValue(runs[0].env, "ANTHROPIC_API_KEY"); found {
					t.Errorf("ANTHROPIC_API_KEY = %q, want it unset for claude", got)
				}
			},
		},
		{
			tool:     "codex",
			launcher: cmdCodex,
			args:     []string{"exec", "say hi"},
			check: func(t *testing.T, _ toolSandbox, runs []toolRun) {
				want := []string{"exec", "-c", codexProviderOverride(testToolPort), "-c", "model_provider=prism", "say hi"}
				if !slices.Equal(runs[0].args, want) {
					t.Errorf("args =\n  %q\nwant\n  %q", runs[0].args, want)
				}
			},
		},
		{
			tool:     "opencode",
			launcher: cmdOpencode,
			args:     []string{"run", "say hi"},
			check: func(t *testing.T, _ toolSandbox, runs []toolRun) {
				if got, _ := envValue(runs[0].env, "ANTHROPIC_API_KEY"); got != "teleport" {
					t.Errorf("ANTHROPIC_API_KEY = %q, want the dummy key", got)
				}
			},
		},
		{
			tool:     "pi",
			launcher: cmdPi,
			args:     []string{"-p", "say hi"},
			check: func(t *testing.T, sb toolSandbox, runs []toolRun) {
				if len(runs) != 2 || !slices.Equal(runs[0].args, []string{"update", "--models"}) {
					t.Fatalf("runs = %+v, want `update --models` and then the launch", runs)
				}
				if got, _ := envValue(runs[1].env, "ANTHROPIC_API_KEY"); got != "teleport" {
					t.Errorf("ANTHROPIC_API_KEY = %q, want the dummy key", got)
				}
				b, err := os.ReadFile(filepath.Join(sb.piDir, "models.json"))
				if err != nil {
					t.Fatalf("models.json was not written: %v", err)
				}
				var cfg struct {
					Providers map[string]struct {
						Models []struct {
							BaseURL string `json:"baseUrl"`
						} `json:"models"`
					} `json:"providers"`
				}
				if err := json.Unmarshal(b, &cfg); err != nil {
					t.Fatal(err)
				}
				for provider, want := range map[string]string{piAnthropic: router, piOpenAI: router + "/v1"} {
					models := cfg.Providers[provider].Models
					if len(models) != 1 || models[0].BaseURL != want {
						t.Errorf("%s models = %+v, want one at %s", provider, models, want)
					}
				}
			},
		},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			sb := newToolSandbox(t, tt.tool)
			key := toolBinEnv[tt.tool]

			fromPATH := sb.launch(t, tt.launcher, tt.args)
			piModelsFromPATH, _ := os.ReadFile(filepath.Join(sb.piDir, "models.json"))

			override := filepath.Join(sb.overrideDir, "my-"+tt.tool)
			t.Setenv(key, override)
			fromOverride := sb.launch(t, tt.launcher, tt.args)
			piModelsFromOverride, _ := os.ReadFile(filepath.Join(sb.piDir, "models.json"))

			if len(fromOverride) == 0 || len(fromOverride) != len(fromPATH) {
				t.Fatalf("override ran %d time(s), PATH binary %d", len(fromOverride), len(fromPATH))
			}
			for i := range fromPATH {
				if want := filepath.Join(sb.pathDir, tt.tool); fromPATH[i].bin != want {
					t.Errorf("unset: ran %s, want %s from PATH", fromPATH[i].bin, want)
				}
				if fromOverride[i].bin != override {
					t.Errorf("%s set: ran %s, want %s", key, fromOverride[i].bin, override)
				}
				if !slices.Equal(fromOverride[i].args, fromPATH[i].args) {
					t.Errorf("run %d args =\n  %q\nwant what PATH's %s got\n  %q",
						i, fromOverride[i].args, tt.tool, fromPATH[i].args)
				}
				// The tool inherits the override variable, the one expected
				// difference.
				overrideEnv := slices.DeleteFunc(slices.Clone(fromOverride[i].env), func(kv string) bool {
					return strings.HasPrefix(kv, key+"=")
				})
				if !slices.Equal(overrideEnv, fromPATH[i].env) {
					t.Errorf("run %d env differs from what PATH's %s got:\n  %q\nvs\n  %q",
						i, tt.tool, overrideEnv, fromPATH[i].env)
				}
			}
			if string(piModelsFromOverride) != string(piModelsFromPATH) {
				t.Errorf("models.json =\n%s\nwant what PATH's pi got\n%s", piModelsFromOverride, piModelsFromPATH)
			}
			tt.check(t, sb, fromOverride)
		})
	}
}

func TestToolBinOverrideInvalidStopsLaunch(t *testing.T) {
	for _, tt := range []struct {
		tool     string
		launcher func([]string) error
	}{
		{"claude", cmdClaude},
		{"codex", cmdCodex},
		{"opencode", cmdOpencode},
		{"pi", cmdPi},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			sb := newToolSandbox(t, tt.tool)
			key := toolBinEnv[tt.tool]
			missing := filepath.Join(sb.overrideDir, "missing")
			t.Setenv(key, missing)

			err := tt.launcher([]string{"say hi"})
			if want := key + "=" + missing + ": no such file"; err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if runs := sb.runs(t); len(runs) != 0 {
				t.Errorf("ran %s, want nothing started", runs[0].bin)
			}
			if _, err := os.Stat(filepath.Join(sb.piDir, "models.json")); err == nil {
				t.Error("models.json was written for a launch that never started")
			}
		})
	}
}

// `prism env` decides Claude Code's credential by asking claude whether
// it is logged in, and must ask the claude `prism claude` would run.
// With the override's claude reporting no login, prism hands out its
// token; asking a claude that isn't there would leave the login alone.
func TestEnvProbesClaudeOverride(t *testing.T) {
	if claudeLoginPinned() {
		t.Skip("this machine's managed settings pin sign-ins, so env never probes")
	}
	sb := newSandbox(t)
	sb.running(t)
	// Forward-proxy mode on auto is the one path where env probes.
	if err := config.Save(&config.Config{ClaudeForwardProxyMode: true}); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(t.TempDir(), "my-claude")
	script := "#!/bin/sh\necho '{\"loggedIn\":false}'\nexit 1\n"
	if err := os.WriteFile(override, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name  string
		extra []string
		want  string
	}{
		{name: "override", extra: []string{"PRISM_CLAUDE_BIN=" + override}, want: "export ANTHROPIC_AUTH_TOKEN=" + claudeGatewayToken + "\n"},
		{name: "nothing on PATH", want: "unset ANTHROPIC_AUTH_TOKEN\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run := (&sandbox{bin: sb.bin, env: append(slices.Clone(sb.env), tt.extra...)}).prism(t, false, "env")
			if run.code != 0 {
				t.Fatalf("exit %d; stderr:\n%s", run.code, run.stderr)
			}
			if !strings.Contains(run.stdout, tt.want) {
				t.Errorf("stdout has no %q:\n%s", tt.want, run.stdout)
			}
		})
	}
}

func TestResolveToolBin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	dir := t.TempDir()
	pathDir := filepath.Join(dir, "path")
	if err := os.Mkdir(pathDir, 0o700); err != nil {
		t.Fatal(err)
	}
	onPATH := filepath.Join(pathDir, "claude")
	writeFakeTool(t, onPATH)
	t.Setenv("PATH", pathDir)

	override := filepath.Join(dir, "my-claude")
	writeFakeTool(t, override)
	notExecutable := filepath.Join(dir, "claude-notes")
	if err := os.WriteFile(notExecutable, []byte(fakeToolScript), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing")

	for _, tt := range []struct {
		name    string
		value   *string
		want    string
		wantErr string
	}{
		{name: "unset looks the tool up on PATH", want: onPATH},
		{name: "empty counts as unset", value: ptr(""), want: onPATH},
		{name: "an override wins over PATH", value: &override, want: override},
		{
			name:    "a relative path is refused",
			value:   ptr("my-claude"),
			wantErr: "PRISM_CLAUDE_BIN=my-claude: not an absolute path",
		},
		{name: "a missing file is refused", value: &missing, wantErr: "PRISM_CLAUDE_BIN=" + missing + ": no such file"},
		{name: "a directory is refused", value: &dir, wantErr: "PRISM_CLAUDE_BIN=" + dir + ": is a directory"},
		{
			name:    "a file without the execute bit is refused",
			value:   &notExecutable,
			wantErr: "PRISM_CLAUDE_BIN=" + notExecutable + ": not executable",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.value == nil {
				unsetEnv(t, "PRISM_CLAUDE_BIN")
			} else {
				t.Setenv("PRISM_CLAUDE_BIN", *tt.value)
			}
			got, err := resolveToolBin("claude")
			if tt.wantErr != "" {
				if err == nil || err.Error() != tt.wantErr {
					t.Fatalf("resolveToolBin() = %q, %v, want error %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("resolveToolBin() = %q, %v, want %q", got, err, tt.want)
			}
		})
	}

	// With nothing set, a tool missing from PATH fails as it always has.
	unsetEnv(t, "PRISM_CODEX_BIN")
	if _, err := resolveToolBin("codex"); err == nil || !strings.HasPrefix(err.Error(), "`codex` not found on PATH: ") {
		t.Errorf("resolveToolBin(codex) error = %v, want the not-found-on-PATH error", err)
	}
}

// fakeToolScript exits 0 because a non-zero exit makes the launcher
// os.Exit the test binary. It fills in Pi's catalog on `update --models`
// so Pi's bootstrap runs through the fake too.
const fakeToolScript = `#!/bin/sh
{
	printf 'run %s\n' "$0"
	for arg in "$@"; do printf 'arg %s\n' "$arg"; done
	/usr/bin/env | while IFS= read -r line; do printf 'env %s\n' "$line"; done
} >>"$PRISM_TEST_TOOL_LOG"
if [ "$1" = update ] && [ "$2" = --models ]; then
	printf '%s\n' '{"anthropic":{"models":[{"id":"a1"}]},"openai":{"models":[{"id":"o1"}]}}' \
		>"$PI_CODING_AGENT_DIR/models-store.json"
fi
exit 0
`

const testToolPort = 7399

type toolRun struct {
	bin  string
	args []string
	env  []string
}

type toolSandbox struct {
	pathDir     string
	overrideDir string
	piDir       string
	log         string
}

// newToolSandbox records a live daemon so a launcher goes straight to the
// tool. PATH holds only the fakes, so if that ever failed the fallback
// `prism up` would stop at a missing tsh instead of reaching a cluster.
func newToolSandbox(t *testing.T, tool string) toolSandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	root := t.TempDir()
	sb := toolSandbox{
		pathDir:     filepath.Join(root, "path"),
		overrideDir: filepath.Join(root, "override"),
		piDir:       filepath.Join(root, "pi"),
		log:         filepath.Join(root, "runs.log"),
	}
	for _, dir := range []string{sb.pathDir, sb.overrideDir} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeFakeTool(t, filepath.Join(sb.pathDir, tool))
	writeFakeTool(t, filepath.Join(sb.overrideDir, "my-"+tool))

	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("PI_CODING_AGENT_DIR", sb.piDir)
	t.Setenv("PATH", sb.pathDir)
	t.Setenv("PRISM_TEST_TOOL_LOG", sb.log)
	for _, key := range toolBinEnv {
		unsetEnv(t, key)
	}
	unsetEnv(t, autoModeServerEnv)

	if err := state.Save(&state.State{LocalPort: testToolPort, DaemonPID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	// On auto, claude's credentials would depend on this machine's managed
	// settings.
	if err := config.Save(&config.Config{ClaudeAuth: claudeAuthToken}); err != nil {
		t.Fatal(err)
	}
	return sb
}

func writeFakeTool(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(fakeToolScript), 0o700); err != nil {
		t.Fatal(err)
	}
}

// unsetEnv calls t.Setenv first so the old value comes back after the test.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

// launch empties Pi's directory so every run bootstraps its catalog the
// same way.
func (s toolSandbox) launch(t *testing.T, launcher func([]string) error, args []string) []toolRun {
	t.Helper()
	if err := os.RemoveAll(s.piDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.piDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.log); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := launcher(args); err != nil {
		t.Fatalf("launch: %v", err)
	}
	return s.runs(t)
}

func (s toolSandbox) runs(t *testing.T) []toolRun {
	t.Helper()
	b, err := os.ReadFile(s.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var runs []toolRun
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		kind, value, _ := strings.Cut(line, " ")
		if kind == "run" {
			runs = append(runs, toolRun{bin: value})
			continue
		}
		if len(runs) == 0 {
			t.Fatalf("log line %q before any run", line)
		}
		r := &runs[len(runs)-1]
		switch kind {
		case "arg":
			r.args = append(r.args, value)
		case "env":
			r.env = append(r.env, value)
		}
	}
	return runs
}

func ptr(s string) *string { return &s }
