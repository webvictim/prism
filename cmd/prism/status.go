package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/beam"
	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/logfile"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tbot"
	"github.com/webvictim/prism/internal/tshwrap"
)

func cmdStatus(args []string) error {
	// First, because everything below prints to stdout and --json
	// promises a single object.
	if statusWantsJSON(args) {
		return writeStatusJSON(os.Stdout)
	}

	// Before the state file, because a service definition pointing at a
	// binary that is gone is exactly why there may be no session to
	// report.
	staleness := serviceStale()
	if staleness.stale() {
		fmt.Fprintf(os.Stdout, "prism: service definition is stale (%s)\n", staleness.Reason)
		fmt.Fprintln(os.Stdout, "       `prism up` repairs it; `prism install` rewrites it now")
	}

	s, err := state.Load()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Println("prism: no active session")
		return nil
	}
	logDir, _ := state.DaemonLogDir()
	statePath, _ := state.Path()

	alive := "DEAD"
	if isServiceManaged() && serviceIsActive() {
		alive = fmt.Sprintf("alive (%s)", serviceManagedLabel())
	} else if s.DaemonPID != 0 && processAlive(s.DaemonPID) {
		alive = fmt.Sprintf("alive (pid %d)", s.DaemonPID)
	} else if portResponds(s.LocalPort) {
		alive = "alive (port responding)"
	}

	fmt.Fprintln(os.Stdout, "prism status")
	fmt.Fprintln(os.Stdout, "------------")
	fmt.Fprintf(os.Stdout, "Router:      127.0.0.1:%d  [%s]\n", s.LocalPort, alive)
	fmt.Fprintf(os.Stdout, "Health:      http://127.0.0.1:%d/_prism/health\n", s.LocalPort)
	fmt.Fprintf(os.Stdout, "Anthropic:   127.0.0.1:%d (app=%s)\n", s.AnthropicPort, "anthropic")
	fmt.Fprintf(os.Stdout, "OpenAI:      127.0.0.1:%d (app=%s)\n", s.OpenAIPort, "openai")
	if s.TbotDiagPort != 0 {
		fmt.Fprintf(os.Stdout, "Bot diag:    127.0.0.1:%d\n", s.TbotDiagPort)
	}
	// `prism status` reports; `prism doctor` explains. Anything here
	// that isn't healthy earns a pointer at the command that says what
	// to do about it, since status itself offers no fixes.
	unhealthy := staleness.stale() || alive == "DEAD"
	if s.Identity() == state.IdentitySourceTbot {
		health := tbotHealthLine(s.TbotDiagPort)
		fmt.Fprintf(os.Stdout, "Identity:    tbot (managed, dir=%s)\n", s.TbotDir)
		fmt.Fprintf(os.Stdout, "Bot health:  %s\n", health)
		if !strings.HasPrefix(health, "ok") {
			unhealthy = true
		}
		if msg := tbotStaleTTLWarning(s.TbotDir); msg != "" {
			fmt.Fprintf(os.Stdout, "\n  WARNING: %s\n", msg)
			unhealthy = true
		}
	} else {
		identity := identityLine()
		fmt.Fprintf(os.Stdout, "Identity:    %s\n", identity)
		if !strings.HasPrefix(identity, "ok") {
			unhealthy = true
		}
	}
	beamLine, beamOK := statusBeamLine(s.LocalPort)
	fmt.Fprintf(os.Stdout, "Beams:       %s\n", beamLine)
	if !beamOK {
		unhealthy = true
	}
	// Information, never a verdict: a full quota is the tenant's
	// configuration, and prism doctor is where it's judged.
	if cfg, err := config.Load(); err == nil {
		if line := quotaStatusLine(cfg, time.Now()); line != "" {
			fmt.Fprintf(os.Stdout, "Quota:       %s\n", line)
		}
	}
	fmt.Fprintf(os.Stdout, "Created:     %s (%s ago)\n", s.CreatedAt.Local().Format(time.RFC3339), time.Since(s.CreatedAt).Round(time.Second))
	fmt.Fprintf(os.Stdout, "State file:  %s\n", statePath)
	if isServiceManaged() && serviceManagedLabel() == "systemd-managed" {
		fmt.Fprintln(os.Stdout, "Daemon log:  journalctl --user -u prism")
	} else {
		fmt.Fprintf(os.Stdout, "Daemon log:  %s\n", logfile.LatestPath(logDir))
	}
	if cfg, _ := config.Load(); beamMigrationNotice(cfg) != "" {
		fmt.Fprintln(os.Stdout, "\n"+beamMigrationSteps)
	}
	if unhealthy {
		fmt.Fprintln(os.Stdout, "\nSomething above isn't healthy — `prism doctor` checks it all and says what to do.")
	}
	return nil
}

// statusBeamLine says whether inference is being recorded in Beams, and
// reports false when that is something to fix.
func statusBeamLine(port int) (string, bool) {
	cfg, _ := config.Load()
	mode := cfg.Beam()
	if cfg.BeamUnmigrated() {
		return "NOT recorded — this tbot machine hasn't switched yet: `prism setup beams` (steps below)", false
	}
	if mode == config.BeamOff {
		return "off — inference isn't recorded in Beams", true
	}
	_, loggedIn := loginUsable()
	b, _, err := beamRequest(http.MethodGet, "", 3*time.Second)
	var st beam.Status
	switch {
	case err != nil || json.Unmarshal(b, &st) != nil:
		return fmt.Sprintf("mode %s; daemon status unavailable", mode), false
	case st.Active && st.Beam != nil:
		return fmt.Sprintf("recording in %s (mode %s, %d requests)", st.Beam.Alias, mode, st.Beam.Requests), true
	case !loggedIn && !cfg.BeamAutoLogin:
		if mode == config.BeamRequired {
			return "no Teleport login, so no beam: inference is refused until `prism login`", false
		}
		return "no Teleport login, so no beam: inference is NOT recorded until `prism login`", false
	}
	return fmt.Sprintf("mode %s; no beam yet — the next request starts one", mode), true
}

// beamMigrationSteps is the full switch for an upgraded tbot machine, for
// `prism status` and the release notes alike.
const beamMigrationSteps = `Recording in Beams — this machine (tbot) hasn't switched yet:
  AI-assisted PRs need a session recorded in Teleport Beams. prism records
  every session once you switch, which is one command:

    prism setup beams

  It logs you in to Teleport with your own account (beams belong to a
  person, so tbot can't start them), checks you can use Beams, and switches
  prism to send inference only through a beam. Nothing restarts.
  From then on, when your login runs out prism pauses inference and says
  so: ` + "`prism login`" + ` resumes it.`

func tbotStaleTTLWarning(tbotDir string) string {
	if tbotDir == "" {
		return ""
	}
	yamlPath := tbot.TbotYAMLPath(tbotDir)
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return ""
	}
	contents := string(data)
	if strings.Contains(contents, "renewal_interval: 23h") || strings.Contains(contents, "renewal_interval: 24h") {
		return "tbot.yaml has a renewal_interval that exceeds the server's 12h cert lifetime.\n" +
			"           Certs will expire before renewal. Run `prism down && prism up` to fix."
	}
	return ""
}

func tbotHealthLine(diagPort int) string {
	if diagPort == 0 {
		return "unknown (no --diag-addr)"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h := tbot.ProbeDiag(ctx, diagPort)
	switch {
	case !h.Reachable:
		return fmt.Sprintf("unreachable on 127.0.0.1:%d (%v)", diagPort, h.Err)
	case !h.Live:
		return fmt.Sprintf("LIVE FAIL on 127.0.0.1:%d/livez", diagPort)
	case !h.Ready:
		// The body is JSON tbot pretty-prints, and this is a one-line
		// field.
		return fmt.Sprintf("not ready: %s", oneLine(h.ReadyBody))
	case h.Degraded:
		return fmt.Sprintf("degraded (tunnel ok, but: %s)", h.DegradedReason)
	default:
		return fmt.Sprintf("ok (live + ready on 127.0.0.1:%d)", diagPort)
	}
}

func identityLine() string {
	st, err := tshwrap.StatusJSON()
	if err != nil {
		return fmt.Sprintf("unknown (tsh status failed: %v)", err)
	}
	login := "`tsh login`"
	if p := tshwrap.Proxy(); p != "" {
		login = "`tsh login --proxy=" + p + "`"
	}
	if !st.LoggedIn() {
		return "not logged in — run " + login
	}
	if st.IsExpired() {
		return fmt.Sprintf("EXPIRED at %s — run %s", st.ValidUntil.Local().Format(time.RFC3339), login)
	}
	remaining := time.Until(st.ValidUntil).Round(time.Minute)
	return fmt.Sprintf("ok as %s on %s (expires in %s)", st.Username, st.Cluster, remaining)
}

func portResponds(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}
