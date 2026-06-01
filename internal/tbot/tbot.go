// Package tbot integrates Teleport Machine ID (`tbot`) as an
// alternative identity backend for prism's data-port subprocess.
// Replaces `tsh proxy app` with `tbot start` running
// application-tunnel services against the cluster-wide anthropic and
// openai apps.
//
// Bootstrap happens once via `prism tbot bootstrap` (writes the
// role / bot / token / tbot.yaml files) plus a few `tctl create -f`
// invocations by the user. From then on the daemon owns tbot.yaml,
// rewriting `services[].app_name` and listen ports on each spawn so
// they match the current state.json. Role / bot / token resources
// are owner-scoped and never change after bootstrap.
package tbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gravitational/prism/internal/tunnel"
)

// SidecarFilename is the name of the prism-managed JSON file inside
// the tbot working directory. It carries the values that prism needs
// to rewrite tbot.yaml at daemon startup (token name, registration
// secret, proxy server, role/bot names for diagnostics).
const SidecarFilename = ".prism-tbot.json"

// SidecarConfig is what `prism tbot configure` writes and what the
// daemon reads at startup to construct a Runtime. Kept separate from
// state.json on purpose: this is per-tbot-install, persistent across
// `prism down --rm` cycles, and contains a secret we'd rather not
// duplicate in state files.
type SidecarConfig struct {
	ProxyServer        string `json:"proxy_server"`
	TokenName          string `json:"token_name"`
	RegistrationSecret string `json:"registration_secret"`
	RoleName           string `json:"role_name,omitempty"`
	BotName            string `json:"bot_name,omitempty"`
}

// SidecarPath returns the absolute path to .prism-tbot.json inside the
// given tbot directory.
func SidecarPath(dir string) string { return filepath.Join(dir, SidecarFilename) }

// TbotYAMLPath returns the absolute path to tbot.yaml inside the given
// tbot directory.
func TbotYAMLPath(dir string) string { return filepath.Join(dir, "tbot.yaml") }

// StorageDirPath returns the absolute path to the storage subdirectory
// tbot uses to persist its bound-keypair state.
func StorageDirPath(dir string) string { return filepath.Join(dir, "storage") }

// LoadSidecar reads the sidecar JSON. Returns a wrapped error pointing
// users at `prism tbot bootstrap` when the file is missing.
func LoadSidecar(dir string) (*SidecarConfig, error) {
	p := SidecarPath(dir)
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("tbot sidecar %s not found — run `prism tbot bootstrap` and `prism tbot configure` first", p)
		}
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var c SidecarConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}

// SaveSidecar writes the sidecar JSON atomically (mode 0600 — it
// contains the bound-keypair registration secret).
func SaveSidecar(dir string, c *SidecarConfig) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	p := SidecarPath(dir)
	tmp := p + ".tmp"
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Validate checks that the tbot working directory is in a state where
// the daemon can spawn tbot against it: sidecar + tbot.yaml + storage
// dir all present. Returns a list of human-readable problems (empty
// when all good) so the caller can present them all at once.
func Validate(dir string) []string {
	var problems []string
	if _, err := os.Stat(SidecarPath(dir)); err != nil {
		problems = append(problems, fmt.Sprintf("%s missing — run `prism tbot configure`", SidecarPath(dir)))
	}
	if _, err := os.Stat(TbotYAMLPath(dir)); err != nil {
		problems = append(problems, fmt.Sprintf("%s missing — run `prism tbot configure`", TbotYAMLPath(dir)))
	}
	if fi, err := os.Stat(StorageDirPath(dir)); err != nil || !fi.IsDir() {
		problems = append(problems, fmt.Sprintf("%s missing — run `prism tbot bootstrap` to create it", StorageDirPath(dir)))
	}
	return problems
}

// Runtime is the tunnel.Runtime implementation that drives `tbot`.
// Constructed by the daemon from state.TbotDir + the on-disk sidecar.
type Runtime struct {
	Dir       string
	LocalPort int
	// DiagPort is the address `tbot start --diag-addr` binds to. When
	// non-zero we pass --diag-addr to the subprocess; prism (and the
	// user) can then GET /livez or /readyz to introspect tbot's
	// internal state without going through the data tunnel. 0 means
	// no diag endpoint is wired up.
	DiagPort int
	// OpenAIPort, when non-zero, adds a second application-tunnel
	// service to tbot.yaml targeting the cluster-wide "openai" app.
	OpenAIPort int
	Sidecar    *SidecarConfig
	Logger     *log.Logger
}

// Prepare rewrites <Dir>/tbot.yaml with the current app names and
// local ports. Called by the tunnel supervisor before each spawn so
// every restart re-renders the config first.
func (r *Runtime) Prepare(_ context.Context, info tunnel.AppInfo) error {
	if r.Sidecar == nil {
		return fmt.Errorf("tbot.Runtime: Sidecar is nil")
	}
	services := []AppTunnelService{
		{Name: AppTunnelServiceName, AppName: info.AppName, Port: info.LocalPort},
	}
	if r.OpenAIPort > 0 {
		services = append(services, AppTunnelService{
			Name:    OpenAITunnelServiceName,
			AppName: "openai",
			Port:    r.OpenAIPort,
		})
	}
	body := RenderTbotYAML(TbotYAMLOpts{
		TokenName:          r.Sidecar.TokenName,
		RegistrationSecret: r.Sidecar.RegistrationSecret,
		StoragePath:        StorageDirPath(r.Dir),
		Services:           services,
		ProxyServer:        r.Sidecar.ProxyServer,
	})
	if err := os.WriteFile(TbotYAMLPath(r.Dir), body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", TbotYAMLPath(r.Dir), err)
	}
	if r.Logger != nil {
		r.Logger.Printf("tbot: rewrote %s (app=%s port=%d, openai_port=%d)", TbotYAMLPath(r.Dir), info.AppName, info.LocalPort, r.OpenAIPort)
	}
	return nil
}

// Command returns the `tbot start -c <tbot.yaml>` exec.Cmd that the
// tunnel supervisor will Start() and Wait() on. Includes
// `--diag-addr 127.0.0.1:<DiagPort>` when DiagPort is non-zero so
// the resulting subprocess exposes /livez and /readyz for
// introspection.
func (r *Runtime) Command(ctx context.Context, _ tunnel.AppInfo) *exec.Cmd {
	args := []string{"start", "-c", TbotYAMLPath(r.Dir)}
	if r.DiagPort > 0 {
		args = append(args, "--diag-addr", fmt.Sprintf("127.0.0.1:%d", r.DiagPort))
	}
	return exec.CommandContext(ctx, "tbot", args...)
}

// DiagHealth is the result of probing tbot's --diag-addr endpoints.
type DiagHealth struct {
	// Reachable is true if the diag listener accepted a connection.
	// False means tbot isn't running, hasn't bound the port yet, or
	// was started without --diag-addr.
	Reachable bool
	// Live mirrors /livez: 2xx means tbot's process loop is healthy.
	// Always true when Reachable since tbot binds livez before any
	// service starts.
	Live bool
	// Ready mirrors /readyz: 2xx means all configured services
	// (incl. application-tunnel) are up. False can mean "still joining"
	// or "credential issuance failed" — read the LastError / response
	// body for detail.
	Ready bool
	// Err is the most recent connection / non-2xx error, or nil.
	Err error
	// ReadyBody is the body of /readyz if non-2xx — usually carries a
	// JSON snippet from tbot describing what's wrong.
	ReadyBody string
}

// ProbeDiag fetches /livez and /readyz from a tbot --diag-addr endpoint
// on 127.0.0.1:port. Returns a structured snapshot suitable for
// surfacing in `prism status` / `prism tbot status`.
func ProbeDiag(ctx context.Context, port int) DiagHealth {
	out := DiagHealth{}
	if port <= 0 {
		out.Err = fmt.Errorf("no diag port configured")
		return out
	}
	client := &http.Client{Timeout: 3 * time.Second}
	get := func(path string) (int, string, error) {
		url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, string(body), nil
	}
	if code, _, err := get("/livez"); err != nil {
		out.Err = err
		return out
	} else {
		out.Reachable = true
		out.Live = code/100 == 2
	}
	// Use the per-service /readyz/<name> rather than the rolled-up
	// /readyz so we report on the application-tunnel specifically.
	// /readyz returns 503 if any tbot service is unhealthy (heartbeat,
	// ca-rotation, etc.) even when the tunnel itself is fine; that's
	// not what we want to report to the user.
	code, body, err := get("/readyz/" + AppTunnelServiceName)
	if err != nil {
		// Reachable + livez OK but readyz dropped the connection mid-
		// probe. Treat as not-ready with the err attached.
		out.Err = err
		return out
	}
	out.Ready = code/100 == 2
	if !out.Ready {
		out.ReadyBody = body
	}
	return out
}

// Name identifies the runtime in log prefixes.
func (*Runtime) Name() string { return "tbot" }

// WatchTshIdentity is false for tbot: tbot refreshes its own identity
// continuously and we have no tsh login to poll.
func (*Runtime) WatchTshIdentity() bool { return false }

// Compile-time check that *Runtime implements tunnel.Runtime.
var _ tunnel.Runtime = (*Runtime)(nil)
