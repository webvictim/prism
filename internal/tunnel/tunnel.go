// Package tunnel supervises a `tsh proxy app` or `tbot start` subprocess
// bound to 127.0.0.1, restarting it on crash with exponential backoff.
package tunnel

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/webvictim/prism/internal/identity"
	"github.com/webvictim/prism/internal/tshwrap"
)

// AppInfo is what the Runtime needs to know to spawn its subprocess.
type AppInfo struct {
	AppName   string // Teleport app name (e.g. "anthropic").
	LocalPort int    // 127.0.0.1 port the subprocess should listen on.
}

// Runtime abstracts the subprocess the tunnel supervises.
type Runtime interface {
	Prepare(ctx context.Context, info AppInfo) error
	Command(ctx context.Context, info AppInfo) *exec.Cmd
	Name() string
	WatchTshIdentity() bool
}

// Config bundles the supervisor's startup parameters.
type Config struct {
	AppName        string
	LocalPort      int
	Logger         *log.Logger
	Runtime        Runtime // nil = default tsh runtime.
	HealthProbeURL string  // empty = skip health probing.
}

// Service is the running supervisor.
type Service struct {
	cfg      Config
	job      *processJob
	identity *identity.Watcher

	mu        sync.Mutex
	proc      *exec.Cmd
	lastError error
	lastOK    time.Time
}

// New constructs a Service.
func New(cfg Config) (*Service, error) {
	if cfg.AppName == "" {
		return nil, fmt.Errorf("tunnel.New: AppName required")
	}
	if cfg.LocalPort <= 0 {
		return nil, fmt.Errorf("tunnel.New: LocalPort required")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(io.Discard, "", 0)
	}
	if cfg.Runtime == nil {
		cfg.Runtime = newTshRuntime()
	}
	job, err := newProcessJob()
	if err != nil {
		cfg.Logger.Printf("tunnel: warning: could not create subprocess job: %v", err)
		job = nil
	}
	s := &Service{cfg: cfg, job: job}
	if cfg.Runtime.WatchTshIdentity() {
		s.identity = identity.New(identity.Config{
			Logger: cfg.Logger,
			OnExpired: func() {
				cfg.Logger.Printf("tunnel: tsh session expired — subprocess will restart on re-login")
			},
			OnRecovered: func() {
				cfg.Logger.Printf("tunnel: restarting subprocess to pick up refreshed identity")
				s.killSubprocess("identity restored")
			},
		})
	}
	return s, nil
}

// Serve runs the supervisor until ctx is cancelled.
func (s *Service) Serve(ctx context.Context) error {
	if s.identity != nil {
		go s.identity.Run(ctx)
	}

	supervisorDone := make(chan struct{})
	go func() {
		s.superviseSubprocess(ctx)
		close(supervisorDone)
	}()

	if s.cfg.HealthProbeURL != "" {
		go s.healthLoop(ctx)
	}

	<-ctx.Done()
	s.cfg.Logger.Printf("tunnel(%s): shutdown signal received", s.cfg.AppName)

	s.killSubprocess("shutting down")
	<-supervisorDone
	if s.job != nil {
		_ = s.job.close()
	}
	return nil
}

func (s *Service) superviseSubprocess(ctx context.Context) {
	const (
		minBackoff = 500 * time.Millisecond
		maxBackoff = 30 * time.Second
	)
	backoff := minBackoff
	logPrefix := s.cfg.Runtime.Name() + ": "
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		info := AppInfo{AppName: s.cfg.AppName, LocalPort: s.cfg.LocalPort}

		if err := s.cfg.Runtime.Prepare(ctx, info); err != nil {
			s.cfg.Logger.Printf("tunnel: %s prepare failed: %v (retry in %s)", s.cfg.Runtime.Name(), err, backoff)
			s.sleepCtx(ctx, backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		s.cfg.Logger.Printf("tunnel: starting %s subprocess (app=%s port=%d)", s.cfg.Runtime.Name(), info.AppName, info.LocalPort)
		cmd := s.cfg.Runtime.Command(ctx, info)
		cmd.Stdout = newPrefixedWriter(s.cfg.Logger, logPrefix)
		cmd.Stderr = newPrefixedWriter(s.cfg.Logger, logPrefix)
		tshwrap.HideWindow(cmd)

		if err := cmd.Start(); err != nil {
			s.cfg.Logger.Printf("tunnel: failed to start %s: %v (retry in %s)", s.cfg.Runtime.Name(), err, backoff)
			s.sleepCtx(ctx, backoff)
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		if s.job != nil {
			if err := s.job.assign(cmd); err != nil {
				s.cfg.Logger.Printf("tunnel: warning: could not assign %s to job: %v", s.cfg.Runtime.Name(), err)
			}
		}

		s.mu.Lock()
		s.proc = cmd
		s.mu.Unlock()

		if !waitPortListening(ctx, s.cfg.LocalPort, 60*time.Second) {
			s.cfg.Logger.Printf("tunnel: 127.0.0.1:%d did not become reachable in 60s; killing subprocess", s.cfg.LocalPort)
			_ = cmd.Process.Kill()
		} else {
			s.cfg.Logger.Printf("tunnel: 127.0.0.1:%d up", s.cfg.LocalPort)
			backoff = minBackoff
		}

		err := cmd.Wait()
		s.mu.Lock()
		s.proc = nil
		s.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		s.cfg.Logger.Printf("tunnel: %s subprocess exited: %v (retry in %s)", s.cfg.Runtime.Name(), err, backoff)
		s.sleepCtx(ctx, backoff)
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

func (s *Service) killSubprocess(reason string) {
	s.mu.Lock()
	proc := s.proc
	s.mu.Unlock()
	if proc == nil || proc.Process == nil {
		return
	}
	s.cfg.Logger.Printf("tunnel: killing %s subprocess (%s)", s.cfg.Runtime.Name(), reason)
	_ = proc.Process.Kill()
}

// --- health loop ---

// restartThreshold is how many consecutive health probe failures trigger
// a subprocess kill (the supervisor will restart it with backoff).
const restartThreshold = 6 // 6 × 10s = ~60s of sustained failure

func (s *Service) healthLoop(ctx context.Context) {
	const interval = 10 * time.Second
	t := time.NewTicker(interval)
	defer t.Stop()
	consecutiveFailures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := s.probeHealth(ctx); err != nil {
			consecutiveFailures++
			if consecutiveFailures%6 == 1 {
				s.cfg.Logger.Printf("tunnel(%s): health probe failed: %v (failures=%d)", s.cfg.AppName, err, consecutiveFailures)
			}
			s.mu.Lock()
			s.lastError = err
			s.mu.Unlock()
			if consecutiveFailures == restartThreshold {
				s.cfg.Logger.Printf("tunnel(%s): %d consecutive health failures — restarting subprocess", s.cfg.AppName, consecutiveFailures)
				s.killSubprocess("sustained health failure")
			}
		} else {
			if consecutiveFailures > 0 {
				s.cfg.Logger.Printf("tunnel(%s): health recovered after %d failures", s.cfg.AppName, consecutiveFailures)
			}
			consecutiveFailures = 0
			s.mu.Lock()
			s.lastOK = time.Now()
			s.lastError = nil
			s.mu.Unlock()
		}
	}
}

func (s *Service) probeHealth(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, s.cfg.HealthProbeURL, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("health %d", resp.StatusCode)
	}
	return nil
}

// --- health endpoint ---

// HealthHandler returns an http.Handler that reports this tunnel's
// health. Intended to be mounted by the local router so we don't need a
// separate listener.
func (s *Service) HealthHandler() http.Handler {
	return http.HandlerFunc(s.handleHealth)
}

func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	lastOK := s.lastOK
	lastErr := s.lastError
	s.mu.Unlock()

	if s.identity != nil {
		snap := s.identity.Snapshot()
		if snap.State == identity.StateExpired {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "identity expired — run `tsh login`\n")
			return
		}
	}
	if lastErr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "unhealthy: %v (lastOK=%s)\n", lastErr, lastOK.Format(time.RFC3339))
		return
	}
	fmt.Fprintf(w, "ok (app=%s)\n", s.cfg.AppName)
}

// --- helpers ---

func waitPortListening(ctx context.Context, port int, timeout time.Duration) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

func (s *Service) sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	n := cur * 2
	if n > max {
		return max
	}
	return n
}

type prefixedWriter struct {
	logger *log.Logger
	prefix string
	buf    []byte
}

func newPrefixedWriter(l *log.Logger, prefix string) *prefixedWriter {
	return &prefixedWriter{logger: l, prefix: prefix}
}

func (w *prefixedWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		nl := -1
		for i, x := range w.buf {
			if x == '\n' {
				nl = i
				break
			}
		}
		if nl < 0 {
			break
		}
		line := string(w.buf[:nl])
		w.buf = w.buf[nl+1:]
		w.logger.Printf("%s%s", w.prefix, line)
	}
	return len(p), nil
}
