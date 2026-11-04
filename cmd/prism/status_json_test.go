package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/logfile"
	"github.com/webvictim/prism/internal/state"
)

func TestStatusJSON(t *testing.T) {
	validUntil := time.Now().Add(8 * time.Hour).UTC().Truncate(time.Second)
	expiredAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	tshIdentity := func(expires *string) statusIdentity {
		return statusIdentity{Backend: new(state.IdentitySourceTSH), Proxy: new(statusTestProxy), ExpiresAt: expires}
	}

	for _, tt := range []struct {
		name string
		// arrange returns the expected report apart from the reason.
		arrange   func(t *testing.T, e statusTestEnv) statusReport
		reasonHas string
	}{
		{
			name: "ready",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				e.tshLoggedIn(t, validUntil)
				port := healthServer(t, http.StatusOK, "ok (app=anthropic)\n")
				saveState(t, state.State{LocalPort: port, DaemonPID: os.Getpid(), IdentitySource: state.IdentitySourceTSH})
				return notReadyReport(func(r *statusReport) {
					r.Ready = true
					r.Daemon = statusDaemon{Running: true, PID: new(os.Getpid())}
					r.Router = statusRouter{Port: new(port), Health: healthOK}
					r.Identity = tshIdentity(new(validUntil.Format(time.RFC3339)))
					r.Apps["anthropic"] = statusApp{Healthy: new(true)}
				})
			},
		},
		{
			name: "daemon dead",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				e.tshLoggedIn(t, validUntil)
				port := closedPort(t)
				saveState(t, state.State{LocalPort: port, DaemonPID: deadPID(t)})
				return notReadyReport(func(r *statusReport) {
					r.Router = statusRouter{Port: new(port), Health: healthFail}
					r.Identity = tshIdentity(new(validUntil.Format(time.RFC3339)))
				})
			},
			reasonHas: "daemon is not running — run `prism down && prism up`",
		},
		{
			// launchd reports a loaded but crash-looping job as active, so
			// an installed service must not count as a running daemon.
			name: "service installed, daemon dead",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				manager := e.installService(t)
				e.tshLoggedIn(t, validUntil)
				port := closedPort(t)
				saveState(t, state.State{LocalPort: port})
				return notReadyReport(func(r *statusReport) {
					r.Daemon.ManagedBy = new(manager)
					r.Router = statusRouter{Port: new(port), Health: healthFail}
					r.Identity = tshIdentity(new(validUntil.Format(time.RFC3339)))
				})
			},
			reasonHas: "daemon is not running",
		},
		{
			name: "router not answering",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				e.tshLoggedIn(t, validUntil)
				port := closedPort(t)
				saveState(t, state.State{LocalPort: port, DaemonPID: os.Getpid()})
				return notReadyReport(func(r *statusReport) {
					r.Daemon = statusDaemon{Running: true, PID: new(os.Getpid())}
					r.Router = statusRouter{Port: new(port), Health: healthFail}
					r.Identity = tshIdentity(new(validUntil.Format(time.RFC3339)))
				})
			},
			reasonHas: "did not answer its health check",
		},
		{
			name: "state missing",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				e.tshLoggedIn(t, validUntil)
				return notReadyReport(func(r *statusReport) {})
			},
			reasonHas: "no prism session — run `prism up`",
		},
		{
			// What makes `prism claude` panic: a service that looks up, and
			// no state to say where.
			name: "state missing, service installed",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				manager := e.installService(t)
				e.tshLoggedIn(t, validUntil)
				return notReadyReport(func(r *statusReport) {
					r.Daemon.ManagedBy = new(manager)
				})
			},
			reasonHas: "no prism session",
		},
		{
			name: "state corrupt",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				e.tshLoggedIn(t, validUntil)
				p, err := state.Path()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(`{"local_port": 7331,`), 0o600); err != nil {
					t.Fatal(err)
				}
				return notReadyReport(func(r *statusReport) {})
			},
			reasonHas: "could not read prism's state",
		},
		{
			// The daemon's identity watcher polls every 30s, so health can
			// still say ok after a login lapses.
			name: "identity expired",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				e.tshLoggedIn(t, expiredAt)
				port := healthServer(t, http.StatusOK, "ok (app=anthropic)\n")
				saveState(t, state.State{LocalPort: port, DaemonPID: os.Getpid()})
				return notReadyReport(func(r *statusReport) {
					r.Daemon = statusDaemon{Running: true, PID: new(os.Getpid())}
					r.Router = statusRouter{Port: new(port), Health: healthOK}
					r.Identity = tshIdentity(new(expiredAt.Format(time.RFC3339)))
					r.Apps["anthropic"] = statusApp{Healthy: new(true)}
				})
			},
			reasonHas: "Teleport login expired at " + expiredAt.Format(time.RFC3339) +
				" — run `tsh login --proxy=" + statusTestProxy + "`",
		},
		{
			// What tsh itself does with a lapsed profile.
			name: "identity refused by tsh",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				e.stub(t, "tsh", "echo 'ERROR: Active profile expired.' >&2\nexit 1\n")
				port := healthServer(t, http.StatusOK, "ok (app=anthropic)\n")
				saveState(t, state.State{LocalPort: port, DaemonPID: os.Getpid()})
				return notReadyReport(func(r *statusReport) {
					r.Daemon = statusDaemon{Running: true, PID: new(os.Getpid())}
					r.Router = statusRouter{Port: new(port), Health: healthOK}
					r.Identity = tshIdentity(nil)
					r.Apps["anthropic"] = statusApp{Healthy: new(true)}
				})
			},
			reasonHas: "no usable Teleport login",
		},
		{
			// tbot renews itself, so it has no expiry and its health shows
			// only through the router.
			name: "health failing",
			arrange: func(t *testing.T, e statusTestEnv) statusReport {
				port := healthServer(t, http.StatusServiceUnavailable,
					"unhealthy: tbot diag unreachable (lastOK=2026-11-03T12:00:00Z)\n")
				saveState(t, state.State{LocalPort: port, DaemonPID: os.Getpid(), IdentitySource: state.IdentitySourceTbot})
				return notReadyReport(func(r *statusReport) {
					r.Daemon = statusDaemon{Running: true, PID: new(os.Getpid())}
					r.Router = statusRouter{Port: new(port), Health: healthFail}
					r.Identity.Backend = new(state.IdentitySourceTbot)
					r.Apps["anthropic"] = statusApp{Healthy: new(false)}
				})
			},
			reasonHas: "failed its health check: 503 unhealthy: tbot diag unreachable",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newStatusTestEnv(t)
			want := tt.arrange(t, e)
			got, out := runStatusJSON(t)

			switch {
			case tt.reasonHas == "" && got.Reason != nil:
				t.Errorf("reason = %q, want null", *got.Reason)
			case tt.reasonHas != "" && got.Reason == nil:
				t.Errorf("reason = null, want one containing %q", tt.reasonHas)
			case tt.reasonHas != "" && !strings.Contains(*got.Reason, tt.reasonHas):
				t.Errorf("reason = %q, want one containing %q", *got.Reason, tt.reasonHas)
			case got.Reason != nil && strings.Contains(*got.Reason, "\n"):
				t.Errorf("reason spans lines: %q", *got.Reason)
			}
			got.Reason = nil
			if !reflect.DeepEqual(got, want) {
				wantJSON, _ := json.Marshal(want)
				t.Errorf("report differs\n got: %s want: %s", out, wantJSON)
			}
		})
	}
}

// Unknown arguments are still ignored rather than rejected.
func TestStatusWithoutJSONIsUnchanged(t *testing.T) {
	e := newStatusTestEnv(t)
	e.tshLoggedIn(t, time.Now().Add(8*time.Hour))
	created := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	s := state.State{LocalPort: closedPort(t), AnthropicPort: 7333, OpenAIPort: 7334, DaemonPID: deadPID(t), CreatedAt: created}
	saveState(t, s)
	statePath, err := state.Path()
	if err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(e.cfg, "prism", "logs")

	// As v0.2.1 printed it. Created ends in a moving age, so only its
	// fixed parts are compared.
	want := []string{
		"prism status",
		"------------",
		fmt.Sprintf("Router:      127.0.0.1:%d  [DEAD]", s.LocalPort),
		fmt.Sprintf("Health:      http://127.0.0.1:%d/_prism/health", s.LocalPort),
		"Anthropic:   127.0.0.1:7333 (app=anthropic)",
		"OpenAI:      127.0.0.1:7334 (app=openai)",
		"Identity:    ok as alice on example.teleport.sh (expires in 8h0m0s)",
		"Created:     " + created.Local().Format(time.RFC3339) + " (",
		"State file:  " + statePath,
		"Daemon log:  " + logfile.LatestPath(logDir),
		"",
		"Something above isn't healthy — `prism doctor` checks it all and says what to do.",
	}
	for _, args := range [][]string{nil, {"--help"}, {"--verbose"}} {
		out := captureStdout(t, func() error { return cmdStatus(args) })
		got := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(got) != len(want) {
			t.Fatalf("cmdStatus(%q) printed %d lines, want %d:\n%s", args, len(got), len(want), out)
		}
		for i := range want {
			if strings.HasPrefix(want[i], "Created:") {
				if !strings.HasPrefix(got[i], want[i]) || !strings.HasSuffix(got[i], " ago)") {
					t.Errorf("cmdStatus(%q) line %d = %q, want %q…ago)", args, i, got[i], want[i])
				}
				continue
			}
			if got[i] != want[i] {
				t.Errorf("cmdStatus(%q) line %d = %q, want %q", args, i, got[i], want[i])
			}
		}
	}
}

// tsh is a shell stub here, so a real one adds its own start-up.
func BenchmarkStatusJSON(b *testing.B) {
	e := newStatusTestEnv(b)
	e.tshLoggedIn(b, time.Now().Add(8*time.Hour))
	port := healthServer(b, http.StatusOK, "ok (app=anthropic)\n")
	saveState(b, state.State{LocalPort: port, DaemonPID: os.Getpid()})
	for b.Loop() {
		if r := buildStatusReport(); !r.Ready {
			b.Fatalf("not ready: %s", *r.Reason)
		}
	}
}

// statusJSONKeys is schema 1. Removing or renaming a key bumps the schema.
var statusJSONKeys = []string{
	"apps", "apps.anthropic", "apps.anthropic.healthy", "apps.openai", "apps.openai.healthy",
	"daemon", "daemon.managed_by", "daemon.pid", "daemon.running",
	"identity", "identity.backend", "identity.expires_at", "identity.proxy",
	"ready", "reason",
	"router", "router.health", "router.port",
	"schema", "version",
}

const statusTestProxy = "example.teleport.sh:443"

type statusTestEnv struct {
	home, cfg, bin string
}

// newStatusTestEnv's PATH holds only the stubs a case writes, so no real
// tsh, launchctl or systemctl can answer.
func newStatusTestEnv(tb testing.TB) statusTestEnv {
	tb.Helper()
	if runtime.GOOS == "windows" {
		tb.Skip("shell stubs")
	}
	e := statusTestEnv{home: tb.TempDir(), cfg: tb.TempDir(), bin: tb.TempDir()}
	tb.Setenv("HOME", e.home)
	tb.Setenv("XDG_CONFIG_HOME", e.cfg)
	tb.Setenv("PATH", e.bin)
	if err := config.Save(&config.Config{Proxy: statusTestProxy}); err != nil {
		tb.Fatal(err)
	}
	return e
}

func (e statusTestEnv) stub(tb testing.TB, name, script string) {
	tb.Helper()
	if err := os.WriteFile(filepath.Join(e.bin, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil {
		tb.Fatal(err)
	}
}

// tshLoggedIn uses echo because it is a shell builtin, and nothing else
// is on PATH.
func (e statusTestEnv) tshLoggedIn(tb testing.TB, validUntil time.Time) {
	e.stub(tb, "tsh", fmt.Sprintf(
		`echo '{"active":{"profile_url":"https://example.teleport.sh:443","username":"alice","cluster":"example.teleport.sh","valid_until":%q}}'`+"\n",
		validUntil.Format(time.RFC3339)))
}

// installService writes a file where isServiceManaged looks, which is all
// it checks. The file isn't a usable definition, so plain status also
// calls it stale.
func (e statusTestEnv) installService(t *testing.T) string {
	t.Helper()
	var path, manager string
	switch runtime.GOOS {
	case "darwin":
		path, manager = filepath.Join(e.home, "Library", "LaunchAgents", "com.prism.daemon.plist"), "launchd"
	case "linux":
		path, manager = filepath.Join(e.home, ".config", "systemd", "user", "prism.service"), "systemd"
		e.stub(t, "systemctl", "exit 0\n")
	default:
		t.Skip("no service manager on " + runtime.GOOS)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("installed by a test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return manager
}

func saveState(tb testing.TB, s state.State) {
	tb.Helper()
	if err := state.Save(&s); err != nil {
		tb.Fatal(err)
	}
}

func healthServer(tb testing.TB, code int, body string) int {
	tb.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_prism/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(code)
		fmt.Fprint(w, body)
	}))
	tb.Cleanup(srv.Close)
	return srv.Listener.Addr().(*net.TCPAddr).Port
}

func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func captureStdout(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	read := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	ferr := f()
	os.Stdout = orig
	_ = w.Close()
	out := <-read
	_ = r.Close()
	if ferr != nil {
		t.Fatalf("returned %v, having printed:\n%s", ferr, out)
	}
	return out
}

func flattenKeys(prefix string, m map[string]any) []string {
	var keys []string
	for k, v := range m {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		keys = append(keys, path)
		if child, ok := v.(map[string]any); ok {
			keys = append(keys, flattenKeys(path, child)...)
		}
	}
	slices.Sort(keys)
	return keys
}

// runStatusJSON checks the shape callers rely on: one object on one line,
// with every key.
func runStatusJSON(t *testing.T) (statusReport, string) {
	t.Helper()
	out := captureStdout(t, func() error { return cmdStatus([]string{"--json"}) })
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Fatalf("want one line ending in a newline, got %q", out)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, out)
	}
	if got := flattenKeys("", raw); !slices.Equal(got, statusJSONKeys) {
		t.Errorf("keys differ from schema 1\n got: %v\nwant: %v", got, statusJSONKeys)
	}
	var r statusReport
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	return r, out
}

func notReadyReport(mod func(*statusReport)) statusReport {
	r := statusReport{
		Schema:  statusSchema,
		Version: version,
		Router:  statusRouter{Health: healthUnknown},
		Apps:    map[string]statusApp{"anthropic": {}, "openai": {}},
		Identity: statusIdentity{
			Proxy: new(statusTestProxy),
		},
	}
	mod(&r)
	return r
}
