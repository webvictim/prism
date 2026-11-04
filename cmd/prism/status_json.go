package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tshwrap"
)

// statusSchema goes up only when a field is removed or changes meaning.
const statusSchema = 1

// The router is local and its health handler does no I/O, so a slower
// answer is as good as none.
const statusHealthTimeout = time.Second

const (
	healthOK      = "ok"
	healthFail    = "fail"
	healthUnknown = "unknown"
)

// statusReport is `prism status --json`. Nothing is omitempty, so a
// reader never has to tell a missing key from an unknown value.
type statusReport struct {
	Schema   int                  `json:"schema"`
	Version  string               `json:"version"`
	Ready    bool                 `json:"ready"`
	Reason   *string              `json:"reason"`
	Daemon   statusDaemon         `json:"daemon"`
	Router   statusRouter         `json:"router"`
	Identity statusIdentity       `json:"identity"`
	Apps     map[string]statusApp `json:"apps"`
}

type statusDaemon struct {
	Running   bool    `json:"running"`
	PID       *int    `json:"pid"`
	ManagedBy *string `json:"managed_by"`
}

type statusRouter struct {
	Port   *int   `json:"port"`
	Health string `json:"health"`
}

type statusIdentity struct {
	Backend   *string `json:"backend"`
	Proxy     *string `json:"proxy"`
	ExpiresAt *string `json:"expires_at"`
}

type statusApp struct {
	Healthy *bool `json:"healthy"`
}

// statusWantsJSON matches the flag by name because status has always
// ignored its arguments, and a flag set would start rejecting some that
// print the report today.
func statusWantsJSON(args []string) bool {
	return slices.Contains(args, "--json") || slices.Contains(args, "-json")
}

func writeStatusJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	// Keep `&&` in the shell commands reasons quote, not \u0026\u0026.
	enc.SetEscapeHTML(false)
	return enc.Encode(buildStatusReport())
}

// Callers run buildStatusReport before every batch, so it stays at one
// health request and, in tsh mode, one `tsh status`. It never asks the
// service manager, because launchd reports a loaded but crash-looping
// job as running.
func buildStatusReport() statusReport {
	r := statusReport{
		Schema:  statusSchema,
		Version: strings.TrimPrefix(version, "v"),
		Router:  statusRouter{Health: healthUnknown},
		Apps: map[string]statusApp{
			router.AnthropicAppName: {},
			router.OpenAIAppName:    {},
		},
	}
	if isServiceManaged() {
		switch runtime.GOOS {
		case "darwin":
			r.Daemon.ManagedBy = new("launchd")
		case "linux":
			r.Daemon.ManagedBy = new("systemd")
		}
	}
	proxy := ""
	if cfg, err := config.Load(); err == nil && cfg.Proxy != "" {
		proxy = cfg.Proxy
		r.Identity.Proxy = new(proxy)
	}

	s, err := state.Load()
	if err != nil {
		return r.notReady("could not read prism's state: " + oneLine(err.Error()))
	}
	if s == nil {
		return r.notReady("no prism session — run `prism up`")
	}
	r.Identity.Backend = new(s.Identity())

	pidAlive := s.DaemonPID != 0 && processAlive(s.DaemonPID)
	if pidAlive {
		r.Daemon.PID = new(s.DaemonPID)
	}
	answered, healthDetail := false, ""
	if s.LocalPort != 0 {
		r.Router.Port = new(s.LocalPort)
		var healthy bool
		answered, healthy, healthDetail = probeRouterHealth(s.LocalPort)
		r.Router.Health = healthFail
		if healthy {
			r.Router.Health = healthOK
		}
		// The health endpoint covers only the Anthropic tunnel, and a
		// router that didn't answer says nothing about it.
		if answered {
			r.Apps[router.AnthropicAppName] = statusApp{Healthy: new(healthy)}
		}
	}
	// The router runs in the daemon, so its answer proves the daemon is up
	// when the pid is stale, or absent as it is under a service manager.
	r.Daemon.Running = pidAlive || answered

	identityProblem := ""
	if s.Identity() == state.IdentitySourceTSH {
		r.Identity.ExpiresAt, identityProblem = tshLoginExpiry(proxy)
	}

	switch {
	case !r.Daemon.Running:
		return r.notReady("prism's daemon is not running — run `prism down && prism up`")
	case s.LocalPort == 0:
		return r.notReady("prism's session records no router port — run `prism down && prism up`")
	case !answered:
		return r.notReady(fmt.Sprintf("router on 127.0.0.1:%d did not answer its health check: %s", s.LocalPort, healthDetail))
	case r.Router.Health != healthOK:
		return r.notReady(fmt.Sprintf("router on 127.0.0.1:%d failed its health check: %s", s.LocalPort, healthDetail))
	case identityProblem != "":
		return r.notReady(identityProblem)
	}
	r.Ready = true
	return r
}

func (r statusReport) notReady(reason string) statusReport {
	r.Reason = &reason
	return r
}

// answered tells a router that is unhealthy from one that isn't there.
func probeRouterHealth(port int) (answered, healthy bool, detail string) {
	client := &http.Client{Timeout: statusHealthTimeout}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/_prism/health", port))
	if err != nil {
		// The reason already names the URL.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return false, false, oneLine(err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return true, true, ""
	}
	// The body says what is wrong, such as an expired tsh login.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return true, false, strings.TrimSpace(fmt.Sprintf("%d %s", resp.StatusCode, oneLine(string(body))))
}

func tshLoginExpiry(proxy string) (expiresAt *string, problem string) {
	login := "`tsh login`"
	if proxy != "" {
		login = "`tsh login --proxy=" + proxy + "`"
	}
	st, err := tshwrap.StatusJSON()
	switch {
	case err != nil:
		return nil, "could not check the Teleport login: " + oneLine(err.Error())
	case !st.LoggedIn():
		// StatusJSON reports a lapsed profile and no profile alike.
		return nil, "no usable Teleport login — run " + login
	}
	at := st.ValidUntil.UTC().Format(time.RFC3339)
	if st.IsExpired() {
		return &at, "Teleport login expired at " + at + " — run " + login
	}
	return &at, ""
}
