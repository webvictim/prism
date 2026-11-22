package beam

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/webvictim/prism/internal/tshwrap"
	"github.com/webvictim/prism/internal/tunnel"
)

// freePorts asks the kernel for n distinct free loopback ports. They are
// released before tsh binds them, so a collision is possible but brief:
// the forward then fails to come up and the next attempt picks again.
func freePorts(n int) ([]int, error) {
	var ls []net.Listener
	defer func() {
		for _, l := range ls {
			_ = l.Close()
		}
	}()
	ports := make([]int, 0, n)
	for range n {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("pick a local port: %w", err)
		}
		ls = append(ls, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	return ports, nil
}

// --- tsh-backed Ops ---

type tshOps struct {
	logger *log.Logger
	roles  func() []string
}

// NewTshOps returns the Ops that talk to the cluster with tsh. roles
// supplies `tsh beams add --roles`; nil means the owner's standing roles.
func NewTshOps(logger *log.Logger, roles func() []string) Ops {
	if roles == nil {
		roles = func() []string { return nil }
	}
	return &tshOps{logger: logger, roles: roles}
}

func (o *tshOps) Add(ctx context.Context) (*tshwrap.Beam, error) {
	return tshwrap.BeamsAdd(ctx, o.roles())
}
func (*tshOps) List(ctx context.Context) ([]tshwrap.Beam, error) {
	return tshwrap.BeamsList(ctx)
}
func (*tshOps) Remove(ctx context.Context, uuid string) error { return tshwrap.BeamsRemove(ctx, uuid) }

// LogIn runs `tsh login` with no terminal; see tshwrap.LoginUnattended.
func (*tshOps) LogIn(ctx context.Context, openBrowser bool, onLink func(string)) error {
	return tshwrap.LoginUnattended(ctx, openBrowser, onLink)
}

// Login reads `tsh status`, which is local: it never asks the cluster.
func (*tshOps) Login() (time.Time, error) {
	st, err := tshwrap.StatusJSON()
	if err != nil {
		return time.Time{}, err
	}
	if !st.LoggedIn() || st.IsExpired() {
		return time.Time{}, tshwrap.ErrNoLogin
	}
	return st.ValidUntil, nil
}

// AppHosts reads the apps' public addresses from `tsh apps ls`, falling
// back to <app>.<proxy host>, which is how a beam's own environment names
// them (ANTHROPIC_BASE_URL=https://anthropic.<cluster>).
func (o *tshOps) AppHosts(context.Context) (string, string, error) {
	fallback := func(app string) string {
		host, _, _ := cutPort(tshwrap.Proxy())
		if host == "" {
			return ""
		}
		return app + "." + host
	}
	anth, oai := fallback("anthropic"), fallback("openai")
	if apps, err := tshwrap.AppsList(); err == nil {
		for _, a := range apps {
			switch a.Metadata.Name {
			case "anthropic":
				if a.Spec.PublicAddr != "" {
					anth = a.Spec.PublicAddr
				}
			case "openai":
				if a.Spec.PublicAddr != "" {
					oai = a.Spec.PublicAddr
				}
			}
		}
	} else {
		o.logger.Printf("beam: could not list apps (%v); assuming %s and %s", err, anth, oai)
	}
	if anth == "" || oai == "" {
		return "", "", errors.New("no Teleport proxy configured, so the LLM apps' addresses are unknown")
	}
	return anth, oai, nil
}

// Connect runs the forward under the tunnel supervisor and waits for a
// TLS handshake with the Anthropic app to succeed through it — proof that
// the beam is reachable, the forward is open, and the far end is the app.
// No HTTP request is sent, so nothing extra lands in the beam's recording.
func (o *tshOps) Connect(ctx context.Context, f Forward) (Connection, error) {
	svc, err := tunnel.New(tunnel.Config{
		AppName:   "beam " + f.UUID,
		LocalPort: f.AnthropicPort,
		Logger:    o.logger,
		Runtime:   &runtime{fwd: f},
	})
	if err != nil {
		return nil, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = svc.Serve(runCtx)
		close(done)
	}()
	c := &tunnelConn{cancel: cancel, done: done}
	for _, target := range []struct {
		host string
		port int
	}{{f.AnthropicHost, f.AnthropicPort}, {f.OpenAIHost, f.OpenAIPort}} {
		if err := waitTLS(ctx, target.port, target.host); err != nil {
			c.Close()
			return nil, err
		}
	}
	return c, nil
}

type tunnelConn struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (c *tunnelConn) Close() {
	c.once.Do(func() {
		c.cancel()
		<-c.done
	})
}

// waitTLS retries a TLS handshake with host through 127.0.0.1:port until
// it succeeds or ctx ends.
func waitTLS(ctx context.Context, port int, host string) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var last error
	for {
		d := tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 10 * time.Second},
			Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not answer through the beam: %w", host, last)
		case <-time.After(time.Second):
		}
	}
}
