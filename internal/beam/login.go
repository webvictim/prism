package beam

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
	"github.com/webvictim/prism/internal/tshwrap"
)

// unavailable answers a request no beam can be had for.
func (m *Manager) unavailable(mode string, cause error) (*egress.Target, error) {
	m.mu.Lock()
	first := !m.outage
	m.outage = true
	m.lastErr = cause.Error()
	m.mu.Unlock()
	noLogin := errors.Is(cause, tshwrap.ErrNoLogin)
	if mode == config.BeamPreferred {
		if first {
			m.cfg.Logger.Printf("beam: WARNING: no beam (%v) — inference goes through the tunnels and is NOT recorded under a beam", cause)
			if noLogin {
				m.cfg.Notify("Inference isn't being recorded in Beams: your Teleport login has expired. Run `prism login`.")
			} else {
				m.cfg.Notify("Inference isn't being recorded in Beams: no beam could be started. `prism doctor` explains.")
			}
		}
		return nil, nil
	}
	if first {
		m.cfg.Logger.Printf("beam: refusing inference until a beam can be started: %v", cause)
		if noLogin {
			m.cfg.Notify("Inference is paused: recording it in Beams needs your Teleport login. Open the link your agent shows, or run `prism login`.")
		} else {
			m.cfg.Notify("Inference is paused: no beam could be started to record it. `prism doctor` explains.")
		}
	}
	return nil, &egress.Refusal{Status: RefusalStatus(cause), Message: RefusalMessage(cause)}
}

// RefusalStatus is the HTTP status of a refused request, and what decides
// how an agent reacts. Measured with Claude Code: a 503 is retried, 11
// times over three minutes, before the message is shown at all; a 400 is
// shown at once. So a missing login, which only the user can fix, is a
// 400, and a beam that failed to start, which may well start on a retry
// (failures are remembered for retryFailedStart), is a 503.
func RefusalStatus(cause error) int {
	if errors.Is(cause, tshwrap.ErrNoLogin) {
		return http.StatusBadRequest
	}
	return http.StatusServiceUnavailable
}

// RefusalMessage is what an agent shows its user when required mode
// refuses a request. It is the only thing they may see, so it says what
// happened, why, and how to fix it. It never suggests sending inference
// unrecorded instead: recording is the policy, and prism stays compliant
// by default. (beam_mode off exists, for power users, in the docs.)
func RefusalMessage(cause error) string {
	const why = "prism sends inference only through a Teleport beam, so that the session is recorded in Beams"
	if errors.Is(cause, tshwrap.ErrNoLogin) {
		var need *LoginNeeded
		if errors.As(cause, &need) && need.Link != "" {
			return "prism: request not sent. " + why + ", and starting a beam needs your own Teleport login, which has expired. " +
				"Log in by opening " + need.Link + " (or run `prism login` in a terminal), then retry."
		}
		return "prism: request not sent. " + why + ", and starting a beam needs your own Teleport login, which has expired. " +
			"Run `prism login` in a terminal, then retry."
	}
	return "prism: request not sent. " + why + ", and none could be started: " + cause.Error() +
		". Retry in a moment; `prism doctor` explains if it persists."
}

// autoLoginEvery bounds how often the daemon runs a login on its own.
const autoLoginEvery = 5 * time.Minute

// autoLoginTimeout bounds one: long enough to finish an SSO flow by hand.
const autoLoginTimeout = 3 * time.Minute

// ensureLogin makes sure there's a login to start a beam with. With
// AutoLogin it runs one — a request waiting meanwhile — at most every
// autoLoginEvery; otherwise a missing login is ErrNoLogin.
func (m *Manager) ensureLogin(ctx context.Context) error {
	if _, err := m.login(); err == nil {
		return nil
	} else if !m.cfg.AutoLogin() {
		return err
	}
	m.loginMu.Lock()
	if !m.autoLoginAt.IsZero() && m.cfg.Now().Sub(m.autoLoginAt) < autoLoginEvery {
		m.loginMu.Unlock()
		return fmt.Errorf("%w (an automatic login was tried at %s; run `prism login`)",
			tshwrap.ErrNoLogin, m.autoLoginAt.Local().Format("15:04"))
	}
	m.autoLoginAt = m.cfg.Now()
	m.loginMu.Unlock()
	m.cfg.Logger.Printf("beam: no Teleport login; logging in (beam_auto_login)")
	lctx, cancel := context.WithTimeout(ctx, autoLoginTimeout)
	defer cancel()
	err := m.cfg.Ops.LogIn(lctx, true, nil)
	m.loginMu.Lock()
	m.loginAt = time.Time{} // look again
	m.loginMu.Unlock()
	if err != nil {
		m.cfg.Logger.Printf("beam: automatic login failed: %v", err)
		return fmt.Errorf("%w (the automatic login failed: %v)", tshwrap.ErrNoLogin, err)
	}
	if _, err := m.login(); err != nil {
		return err
	}
	m.cfg.Logger.Printf("beam: logged in")
	return nil
}

// linkWait bounds how long a refused request waits for tsh to print its
// login link. A first login on a machine can download tsh's managed
// client tools before it gets that far.
const linkWait = 45 * time.Second

// linkLoginTTL is how long a link login stays open for the user to finish.
const linkLoginTTL = 10 * time.Minute

// LoginNeeded is ErrNoLogin with the link that finishes a login the
// daemon has already started, when there is one.
type LoginNeeded struct{ Link string }

func (e *LoginNeeded) Error() string { return tshwrap.ErrNoLogin.Error() }
func (e *LoginNeeded) Unwrap() error { return tshwrap.ErrNoLogin }

// loginLink starts a link login if none is waiting, and returns a
// LoginNeeded carrying its link — or without one if tsh couldn't produce
// one in time.
func (m *Manager) loginLink(ctx context.Context) error {
	m.loginMu.Lock()
	p := m.pending
	if p == nil {
		p = &pendingLogin{ready: make(chan struct{}), done: make(chan struct{})}
		m.pending = p
		go m.runLinkLogin(p)
	}
	m.loginMu.Unlock()
	select {
	case <-p.ready:
	case <-p.done:
	case <-ctx.Done():
	case <-time.After(linkWait):
	}
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	return &LoginNeeded{Link: p.link}
}

func (m *Manager) runLinkLogin(p *pendingLogin) {
	ctx, cancel := context.WithTimeout(context.Background(), linkLoginTTL)
	defer cancel()
	m.cfg.Logger.Printf("beam: no Teleport login; waiting up to %s for one through its link", linkLoginTTL)
	var once sync.Once
	err := m.cfg.Ops.LogIn(ctx, false, func(link string) {
		once.Do(func() {
			m.loginMu.Lock()
			p.link = link
			m.loginMu.Unlock()
			close(p.ready)
		})
	})
	m.loginMu.Lock()
	if m.pending == p {
		m.pending = nil
	}
	m.loginAt = time.Time{} // look again
	m.loginMu.Unlock()
	close(p.done)
	if err != nil {
		m.cfg.Logger.Printf("beam: the login link went unused or failed: %v", err)
		return
	}
	m.cfg.Logger.Printf("beam: logged in through the link; the next request starts a beam")
}

// login is Ops.Login, cached briefly: Acquire asks on every request that
// finds no beam.
func (m *Manager) login() (time.Time, error) {
	m.loginMu.Lock()
	defer m.loginMu.Unlock()
	now := m.cfg.Now()
	if !m.loginAt.IsZero() && now.Sub(m.loginAt) < loginCacheFor && now.After(m.loginAt) {
		return m.loginUntil, m.loginErr
	}
	m.loginUntil, m.loginErr = m.cfg.Ops.Login()
	if m.loginErr == nil && !m.loginUntil.After(now) {
		m.loginErr = tshwrap.ErrNoLogin
	}
	m.loginAt = now
	return m.loginUntil, m.loginErr
}
