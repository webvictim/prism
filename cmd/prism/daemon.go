package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/gravitational/prism/internal/router"
	"github.com/gravitational/prism/internal/state"
	"github.com/gravitational/prism/internal/tbot"
	"github.com/gravitational/prism/internal/tunnel"
	usagepkg "github.com/gravitational/prism/internal/usage"
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
	}
	logger := log.New(os.Stderr, "", logFlags)
	return runDaemon(s, logger)
}

func runDaemon(s *state.State, logger *log.Logger) error {
	logger.Printf("daemon starting (identity=%s): router=127.0.0.1:%d anthropic=127.0.0.1:%d openai=127.0.0.1:%d",
		s.Identity(), s.LocalPort, s.AnthropicPort, s.OpenAIPort)

	usagePath, err := usagepkg.Path()
	if err != nil {
		logger.Printf("warning: usage tracking disabled: %v", err)
	}
	var uw *usagepkg.Writer
	if usagePath != "" {
		uw, err = usagepkg.NewWriter(usagePath)
		if err != nil {
			logger.Printf("warning: usage tracking disabled: %v", err)
		} else {
			defer uw.Close()
		}
	}

	proxy := os.Getenv("TELEPORT_PROXY")

	ctx, cancel := signal.NotifyContext(context.Background(), forwardedSignals()...)
	defer cancel()

	// In tbot mode, a single tbot process manages both tunnels via
	// multi-service yaml. In tsh mode, we run two tsh proxy app subprocesses.
	if s.Identity() == state.IdentitySourceTbot {
		return runTbotDaemon(ctx, s, logger, uw, proxy)
	}
	return runTshDaemon(ctx, s, logger, uw, proxy)
}

func runTshDaemon(ctx context.Context, s *state.State, logger *log.Logger, uw *usagepkg.Writer, proxy string) error {
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
		UsageWriter:   uw,
		Proxy:         proxy,
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

func runTbotDaemon(ctx context.Context, s *state.State, logger *log.Logger, uw *usagepkg.Writer, proxy string) error {
	if s.TbotDir == "" {
		return fmt.Errorf("daemon: tbot mode but TbotDir is empty")
	}
	sidecar, err := tbot.LoadSidecar(s.TbotDir)
	if err != nil {
		return fmt.Errorf("daemon: load tbot sidecar: %w", err)
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
		AppName:        router.AnthropicAppName,
		LocalPort:      s.AnthropicPort,
		Logger:         logger,
		Runtime:        runtime,
		HealthProbeURL: tbotHealthProbeURL(s.TbotDiagPort),
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
		UsageWriter:   uw,
		Proxy:         proxy,
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

func tbotHealthProbeURL(diagPort int) string {
	if diagPort <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/readyz", diagPort)
}
