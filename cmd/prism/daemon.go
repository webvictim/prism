package main

import (
	"cmp"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sync"
	"time"

	"github.com/webvictim/prism/internal/beam"
	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
	"github.com/webvictim/prism/internal/logfile"
	"github.com/webvictim/prism/internal/machineid"
	"github.com/webvictim/prism/internal/mitm"
	"github.com/webvictim/prism/internal/router"
	"github.com/webvictim/prism/internal/scrub"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tbot"
	"github.com/webvictim/prism/internal/tshwrap"
	"github.com/webvictim/prism/internal/tunnel"
	usagepkg "github.com/webvictim/prism/internal/usage"
)

func cmdDaemon(_ []string) error {
	s, err := state.Load()
	if err != nil {
		return fmt.Errorf("daemon: load state: %w", err)
	}
	if s == nil {
		return fmt.Errorf("daemon: no state file — was prism up run?")
	}

	logFlags := log.LstdFlags
	if os.Getenv("JOURNAL_STREAM") != "" {
		logFlags = 0
		logger := log.New(os.Stderr, "", logFlags)
		return runDaemon(s, logger)
	}

	logDir, err := state.DaemonLogDir()
	if err != nil {
		return fmt.Errorf("daemon: log dir: %w", err)
	}
	lw, err := logfile.Open(logDir)
	if err != nil {
		return fmt.Errorf("daemon: open log: %w", err)
	}
	defer lw.Close()

	logger := log.New(lw, "", logFlags)
	return runDaemon(s, logger)
}

func runDaemon(s *state.State, logger *log.Logger) error {
	logger.Printf("daemon starting (identity=%s): router=127.0.0.1:%d anthropic=127.0.0.1:%d openai=127.0.0.1:%d",
		s.Identity(), s.LocalPort, s.AnthropicPort, s.OpenAIPort)

	usageDir, err := usagepkg.Dir()
	if err != nil {
		logger.Printf("warning: usage tracking disabled: %v", err)
	}
	var uw *usagepkg.Writer
	if usageDir != "" {
		uw, err = usagepkg.NewWriter(usageDir)
		if err != nil {
			logger.Printf("warning: usage tracking disabled: %v", err)
		} else {
			defer uw.Close()
		}
	}

	proxy := daemonProxy()

	// Every upstream request — router, chat/completions shim, forward
	// proxy — goes through one switch, so that a beam, when there is
	// one, carries all of it (see internal/egress and internal/beam).
	egressSwitch := egress.NewSwitch(nil)

	// Build the forward-proxy handler if enabled. The variable is a
	// plain http.Handler so a disabled forward proxy stays a true nil
	// interface — assigning a nil *mitm.Handler would make router.New
	// mount a proxy branch that panics on use.
	var proxyHandler http.Handler
	cfg, _ := config.Load()
	if cfg != nil && cfg.ClaudeForwardProxyMode {
		configDir, err := config.Dir()
		if err != nil {
			return fmt.Errorf("daemon: config dir: %w", err)
		}
		if _, err := mitm.EnsureCA(configDir); err != nil {
			return fmt.Errorf("daemon: ensure CA: %w", err)
		}
		ca, caKey, err := mitm.LoadCA(configDir)
		if err != nil {
			return fmt.Errorf("daemon: load CA: %w", err)
		}
		proxyHandler = &mitm.Handler{
			CA:            ca,
			CAKey:         caKey,
			AnthropicPort: s.AnthropicPort,
			Transport:     egressSwitch,
			Logger:        logger,
			Debug:         s.Debug,
			UsageWriter:   uw,
			Proxy:         proxy,
		}
		logger.Printf("daemon: forward-proxy mode enabled (MITM for api.anthropic.com)")
	}

	// Config-supplied strip lists: the no-release escape hatch for a new
	// client field or tool the gateway rejects.
	if cfg != nil {
		scrub.SetExtra(scrub.Extra{
			AnthropicFields:    cfg.AnthropicStripFields,
			AnthropicToolTypes: cfg.AnthropicStripToolTypes,
			OpenAIFields:       cfg.OpenAIStripFields,
			OpenAIToolTypes:    cfg.OpenAIStripToolTypes,
		})
		for _, l := range []struct {
			key  string
			vals []string
		}{
			{"anthropic_strip_fields", cfg.AnthropicStripFields},
			{"anthropic_strip_tool_types", cfg.AnthropicStripToolTypes},
			{"openai_strip_fields", cfg.OpenAIStripFields},
			{"openai_strip_tool_types", cfg.OpenAIStripToolTypes},
		} {
			if len(l.vals) > 0 {
				logger.Printf("daemon: %s=%v", l.key, l.vals)
			}
		}
	}

	// Enabled unless the config turns it off; see
	// config.ChatCompletionsShimEnabled.
	chatShim := cfg.ChatCompletionsShimEnabled()
	if !chatShim {
		logger.Printf("daemon: chat/completions shim disabled — relaying /v1/chat/completions unchanged")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), forwardedSignals()...)
	defer cancel()

	// Keeps the cluster's model list cached, so launches never wait on it.
	go runModelsRefresher(ctx, logger)
	// And the tenant's token quota, for prism usage / status / statusline.
	go runQuotaRefresher(ctx, logger)

	liveCfg := liveBeamConfig(cfg, logger)
	beams, err := beam.New(beam.Config{
		Switch:              egressSwitch,
		Logger:              logger,
		AnthropicTunnelPort: s.AnthropicPort,
		OpenAITunnelPort:    s.OpenAIPort,
		Proxy:               proxy,
		Mode:                func() string { return liveCfg().Beam() },
		AutoLogin:           func() bool { return liveCfg().BeamAutoLogin },
		Ops:                 beam.NewTshOps(logger, func() []string { return liveCfg().BeamRoles }),
		Scope:               func() string { return cmp.Or(liveCfg().BeamScope, beam.ScopeSession) },
		Max:                 func() int { return liveCfg().BeamMax },
		Notify:              func(msg string) { desktopNotify(logger, msg) },
		RotateBefore:        beamRotateOverride(logger),
		Version:             version,
	})
	if err != nil {
		return fmt.Errorf("daemon: beams: %w", err)
	}
	logger.Printf("daemon: beam_mode=%s — %s", cfg.Beam(), beamModeMeaning(cfg.Beam()))
	// With no beam in use, a request asks the Manager for one.
	egressSwitch.SetProvider(beams)
	beamsDone := make(chan struct{})
	go func() {
		beams.Run(ctx)
		close(beamsDone)
	}()
	// Run closes the forwards when ctx ends; wait for that so no tsh ssh
	// outlives the daemon. Cancel first: a tunnel mode that returns an
	// error does so with ctx still live.
	defer func() {
		cancel()
		<-beamsDone
	}()

	d := daemonDeps{egress: egressSwitch, beams: beams.Handler()}

	// In tbot mode, a single tbot process manages both tunnels via
	// multi-service yaml. In tsh mode, we run two tsh proxy app subprocesses.
	if s.Identity() == state.IdentitySourceTbot {
		return runTbotDaemon(ctx, s, logger, uw, proxy, proxyHandler, chatShim, d)
	}
	return runTshDaemon(ctx, s, logger, uw, proxy, proxyHandler, chatShim, d)
}

// daemonDeps are what both identity modes hand the router unchanged.
type daemonDeps struct {
	egress http.RoundTripper
	beams  http.Handler
}

func runTshDaemon(ctx context.Context, s *state.State, logger *log.Logger, uw *usagepkg.Writer, proxy string, proxyHandler http.Handler, chatShim bool, d daemonDeps) error {
	// Anthropic tunnel — also exposes the health endpoint we hang off the router.
	anthropicSvc, err := tunnel.New(tunnel.Config{
		AppName:   router.AnthropicAppName,
		LocalPort: s.AnthropicPort,
		Logger:    logger,
	})
	if err != nil {
		return fmt.Errorf("anthropic tunnel: %w", err)
	}

	openaiSvc, err := tunnel.New(tunnel.Config{
		AppName:   router.OpenAIAppName,
		LocalPort: s.OpenAIPort,
		Logger:    logger,
	})
	if err != nil {
		return fmt.Errorf("openai tunnel: %w", err)
	}

	// Local HTTP router with scrubbing — also serves /_prism/health.
	rtr, err := router.New(router.Config{
		ListenPort:    s.LocalPort,
		AnthropicPort: s.AnthropicPort,
		OpenAIPort:    s.OpenAIPort,
		Logger:        logger,
		Debug:         s.Debug,
		HealthHandler: anthropicSvc.HealthHandler(),
		ProxyHandler:  proxyHandler,
		UsageWriter:   uw,
		Proxy:         proxy,

		ChatCompletionsShim: chatShim,
		Egress:              d.egress,
		BeamHandler:         d.beams,
	})
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}

	errCh := make(chan error, 3)
	go func() { errCh <- anthropicSvc.Serve(ctx) }()
	go func() { errCh <- openaiSvc.Serve(ctx) }()
	go func() { errCh <- rtr.Serve(ctx) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		cancel := func() {} // already cancelled by signal
		_ = cancel
		return err
	}
}

func runTbotDaemon(ctx context.Context, s *state.State, logger *log.Logger, uw *usagepkg.Writer, proxy string, proxyHandler http.Handler, chatShim bool, d daemonDeps) error {
	if s.TbotDir == "" {
		return fmt.Errorf("daemon: tbot mode but TbotDir is empty")
	}
	sidecar, err := tbot.LoadSidecar(s.TbotDir)
	if err != nil {
		return fmt.Errorf("daemon: load tbot sidecar: %w", err)
	}

	// Refuse to spawn tbot against an identity issued to another
	// machine. This is the backstop that matters: a cloned Mac starts
	// the daemon from its LaunchAgent at first login, before anyone
	// runs a prism command, and two machines renewing one bound
	// keypair can lock the bot out from under the original. Refusing is
	// the whole intervention — the daemon never touches storage/.
	current, idErr := machineid.Current()
	if idErr != nil {
		logger.Printf("tbot: could not read this machine's id (%v); skipping the clone check", idErr)
	}
	if err := tbot.CheckMachine(s.TbotDir, current); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	// And refuse to run alongside a tbot an earlier daemon left on this
	// identity, for the same reason one machine level down: two renewers
	// get the bot locked. See tbot_orphans.go.
	if err := stopOrphanedTbots(s.TbotDir, logger); err != nil {
		return fmt.Errorf("daemon: %w", err)
	}

	// tbot manages both tunnels in a single process. We use the
	// anthropic port as the "primary" for the tunnel.Service supervisor.
	runtime := &tbot.Runtime{
		Dir:        s.TbotDir,
		LocalPort:  s.AnthropicPort,
		OpenAIPort: s.OpenAIPort,
		DiagPort:   s.TbotDiagPort,
		Sidecar:    sidecar,
		Logger:     logger,
	}

	tbotSvc, err := tunnel.New(tunnel.Config{
		AppName:         router.AnthropicAppName,
		LocalPort:       s.AnthropicPort,
		Logger:          logger,
		Runtime:         runtime,
		HealthProbeURLs: tbotHealthProbeURLs(s.TbotDiagPort, s.OpenAIPort),
	})
	if err != nil {
		return fmt.Errorf("tbot tunnel: %w", err)
	}

	rtr, err := router.New(router.Config{
		ListenPort:    s.LocalPort,
		AnthropicPort: s.AnthropicPort,
		OpenAIPort:    s.OpenAIPort,
		Logger:        logger,
		Debug:         s.Debug,
		HealthHandler: tbotSvc.HealthHandler(),
		ProxyHandler:  proxyHandler,
		UsageWriter:   uw,
		Proxy:         proxy,

		ChatCompletionsShim: chatShim,
		Egress:              d.egress,
		BeamHandler:         d.beams,
	})
	if err != nil {
		return fmt.Errorf("router: %w", err)
	}

	errCh := make(chan error, 2)
	go func() { errCh <- tbotSvc.Serve(ctx) }()
	go func() { errCh <- rtr.Serve(ctx) }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}

// tbotHealthProbeURLs are the readiness endpoints of the tunnel services
// alone. tbot's overall /readyz goes unhealthy when any service does, and
// a restart is the wrong answer to anything but a tunnel that's down: it
// takes the tunnels with it, and SIGKILLing tbot mid-renewal is itself a
// way to get the bot locked. The identity output in particular is
// none of the health loop's business.
func tbotHealthProbeURLs(diagPort, openaiPort int) []string {
	if diagPort <= 0 {
		return nil
	}
	base := fmt.Sprintf("http://127.0.0.1:%d/readyz/", diagPort)
	urls := []string{base + tbot.AppTunnelServiceName}
	if openaiPort > 0 {
		urls = append(urls, base+tbot.OpenAITunnelServiceName)
	}
	return urls
}

// daemonProxy is the cluster this daemon serves: the configured proxy,
// which applyConfigEnv has already aimed every tsh call at. Only with none
// configured does an inherited TELEPORT_PROXY count. The other way round,
// a shell still exporting another cluster's TELEPORT_PROXY — easily left
// over from working on it — labelled this daemon's usage records, beams
// and recording links with a cluster they never touched, while every
// request went to the configured one.
func daemonProxy() string {
	if p := tshwrap.Proxy(); p != "" {
		return p
	}
	return os.Getenv("TELEPORT_PROXY")
}

// beamRotateOverride reads PRISM_BEAM_ROTATE_BEFORE, a test hook that moves
// a beam's rollover earlier, so a soak test can see one inside a tsh login
// shorter than a beam's 11 hours. Unset (the normal case) means the default.
// liveBeamConfig returns the config as it is now, re-reading the file at
// most every few seconds, so the beam settings (beam_mode,
// beam_auto_login, beam_roles) apply to a running daemon. A config that
// can't be read keeps the last one.
func liveBeamConfig(initial *config.Config, logger *log.Logger) func() *config.Config {
	var (
		mu   sync.Mutex
		cur  = cmp.Or(initial, &config.Config{})
		read = time.Now()
	)
	return func() *config.Config {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(read) < 3*time.Second {
			return cur
		}
		read = time.Now()
		next, err := config.Load()
		if err != nil || next == nil {
			return cur
		}
		if next.Beam() != cur.Beam() {
			logger.Printf("daemon: beam_mode is now %s — %s", next.Beam(), beamModeMeaning(next.Beam()))
		}
		if next.BeamAutoLogin != cur.BeamAutoLogin {
			logger.Printf("daemon: beam_auto_login is now %v", next.BeamAutoLogin)
		}
		if !slices.Equal(next.BeamRoles, cur.BeamRoles) {
			logger.Printf("daemon: beam_roles is now %v (for beams started from now on)", next.BeamRoles)
		}
		cur = next
		return cur
	}
}

func beamRotateOverride(logger *log.Logger) time.Duration {
	v := os.Getenv("PRISM_BEAM_ROTATE_BEFORE")
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		logger.Printf("daemon: ignoring PRISM_BEAM_ROTATE_BEFORE=%q: not a positive duration", v)
		return 0
	}
	logger.Printf("daemon: TEST OVERRIDE: beams are replaced %s before they expire (PRISM_BEAM_ROTATE_BEFORE)", d)
	return d
}
