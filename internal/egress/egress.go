// Package egress decides where prism's tunnel traffic leaves from.
//
// The router, the chat/completions shim and the forward proxy all send
// their upstream requests to plain HTTP on 127.0.0.1:<tunnel port>, where
// tsh or tbot listens. A Switch is the http.RoundTripper they share. With
// nothing activated it passes every request through untouched. With a
// beam activated it sends each tunnel's requests to that tunnel's Teleport
// app instead, over the beam's `tsh ssh -L` forward, so that they leave
// the cluster from inside the beam and are recorded under it — which is
// what makes a local agent session a Beams session.
//
// Switching is per request and never interrupts one in flight: a request
// keeps the route it started with, so a beam can be swapped for the next
// one while long streams are still running on the old.
package egress

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Route sends one tunnel's traffic to its app through a local forward.
type Route struct {
	// TunnelPort is the 127.0.0.1 port the tunnel normally listens on,
	// which is how requests for this route are recognised.
	TunnelPort int
	// AppHost is the app's public address, e.g. anthropic.example.com.
	// It is the TLS server name and the Host header, exactly as a client
	// inside the beam would send them.
	AppHost string
	// ForwardAddr is the local end of the forward, e.g. 127.0.0.1:17400.
	ForwardAddr string
	// RootCAs verifies the app's certificate; nil means the system roots,
	// which is right for a real cluster. Tests supply their own.
	RootCAs *x509.CertPool
}

// Target is a set of routes that are activated together — one beam.
type Target struct {
	// Name identifies the target in logs, e.g. the beam's alias.
	Name   string
	routes map[string]*route

	// inflight counts requests sent through the target whose response
	// bodies are still open, so a replaced target can be torn down once
	// its last stream finishes rather than in the middle of it.
	inflight atomic.Int64
	served   atomic.Int64
}

// InFlight reports how many requests are still using the target.
func (t *Target) InFlight() int64 { return t.inflight.Load() }

// Served reports how many requests the target has carried.
func (t *Target) Served() int64 { return t.served.Load() }

// WaitIdle blocks until no request is using the target, ctx ends, or max
// passes, and reports whether it went idle.
func (t *Target) WaitIdle(ctx context.Context, max time.Duration) bool {
	deadline := time.NewTimer(max)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for t.InFlight() > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
	return true
}

type route struct {
	Route
	transport *http.Transport
}

// NewTarget builds a Target. Each route gets its own transport, since the
// TLS server name and the dial address both depend on the route.
func NewTarget(name string, routes ...Route) (*Target, error) {
	t := &Target{Name: name, routes: make(map[string]*route, len(routes))}
	for _, r := range routes {
		if r.TunnelPort <= 0 || r.AppHost == "" || r.ForwardAddr == "" {
			return nil, fmt.Errorf("egress: incomplete route %+v", r)
		}
		key := strconv.Itoa(r.TunnelPort)
		if _, dup := t.routes[key]; dup {
			return nil, fmt.Errorf("egress: two routes for tunnel port %d", r.TunnelPort)
		}
		t.routes[key] = &route{Route: r, transport: newTransport(r)}
	}
	return t, nil
}

// newTransport dials the forward but speaks TLS to the app, so the app's
// real certificate is verified end to end: the forward only carries bytes.
func newTransport(r Route) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	addr := r.ForwardAddr
	return &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
		TLSClientConfig:       &tls.Config{ServerName: r.AppHost, RootCAs: r.RootCAs, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// Close drops the target's idle connections. Requests still in flight
// keep theirs.
func (t *Target) Close() {
	if t == nil {
		return
	}
	for _, r := range t.routes {
		r.transport.CloseIdleConnections()
	}
}

// Routes returns the target's routes, for status reporting.
func (t *Target) Routes() []Route {
	if t == nil {
		return nil
	}
	out := make([]Route, 0, len(t.routes))
	for _, r := range t.routes {
		out = append(out, r.Route)
	}
	return out
}

// InternalHeader marks a request prism makes for its own purposes — the
// per-model route probes `prism setup` sends — that should not be recorded
// as part of an agent's session. The Switch sends such requests to the
// tunnels whatever is active, and strips the header either way. It is not
// a security boundary: anyone who can reach the router can stop using a
// beam with `prism beam end` too.
const InternalHeader = "X-Prism-Internal"

// NoStartHeader marks a request that should use the beam in use, if
// there is one, and otherwise the tunnels: it must never cause a beam to
// be started. prism's own probes (`prism test`, `prism doctor`) send it,
// since they report on the path in use and doctor changes nothing. The
// Switch strips it.
const NoStartHeader = "X-Prism-No-Start"

// BeamHeader overrides the beam mode for one request, so that one launch
// can differ from the configured beam_mode: `prism --no-beam claude …`
// sends BeamOff, which goes straight to the tunnels, unrecorded and
// without waiting for a beam to start; `prism --beam …` sends
// BeamRequired, which goes only through a beam even when beam_mode is off
// or preferred. The launchers add it to every request of the launch, the
// way they add WorkspaceHeader. The Switch strips it from everything it
// sends. Like InternalHeader, it is not a security boundary: anyone who
// can reach the router can change beam_mode too.
const BeamHeader = "X-Prism-Beam"

// The values BeamHeader takes. Anything else is ignored.
const (
	BeamOff      = "off"
	BeamRequired = "required"
)

// Switch is an http.RoundTripper that sends tunnel traffic either to the
// tunnels (the default) or through the active Target.
type Switch struct {
	base     http.RoundTripper
	active   atomic.Pointer[Target]
	provider atomic.Pointer[providerBox]
	// inflight counts every request the Switch carries, beam or tunnel,
	// until its response body closes: whether a daemon restart would cut
	// anything off.
	inflight atomic.Int64
	// unrecorded counts the requests BeamHeader sent past the beams.
	unrecorded atomic.Int64
}

// Unrecorded reports how many requests were sent past the beams because
// their launch asked for it (BeamHeader: off).
func (s *Switch) Unrecorded() int64 { return s.unrecorded.Load() }

// InFlight reports how many requests the Switch is carrying, by any route.
func (s *Switch) InFlight() int64 { return s.inflight.Load() }

// Provider is asked for a target when a tunnel request arrives with none
// active. Acquire may start one (blocking the request meanwhile), answer
// nil to send the request to the tunnels, or refuse it with a *Refusal;
// it sees the request's headers, which is how it tells sessions apart.
// Peek answers only with a target already running, never starting one.
type Provider interface {
	Acquire(req *http.Request) (*Target, error)
	Peek(req *http.Request) *Target
}

// WorkspaceHeader names the repository a request is for. prism's
// launchers add it (Claude Code through ANTHROPIC_CUSTOM_HEADERS, Codex
// through its provider's http_headers) so that sessions in one repository
// can share a beam when beams run short. The Switch removes it from every
// request: a path on this machine has no business in a recording.
const WorkspaceHeader = "X-Prism-Workspace"

type providerBox struct{ p Provider }

// SetProvider installs p, or removes the provider when p is nil.
func (s *Switch) SetProvider(p Provider) {
	if p == nil {
		s.provider.Store(nil)
		return
	}
	s.provider.Store(&providerBox{p})
}

// Refusal is a Provider's answer when a request must not be sent at all.
// The client gets it as an API error in the shape its API uses, so an
// agent shows the message to its user rather than a bare 502.
type Refusal struct {
	Status  int
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// NewSwitch returns a Switch that passes through to base, or to
// http.DefaultTransport when base is nil.
func NewSwitch(base http.RoundTripper) *Switch {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Switch{base: base}
}

// Activate makes t the route for new requests and returns the target it
// replaced, if any. A nil t returns traffic to the tunnels. The caller
// owns the old target and closes it once its requests have drained.
func (s *Switch) Activate(t *Target) (previous *Target) {
	return s.active.Swap(t)
}

// Active returns the target new requests use, or nil.
func (s *Switch) Active() *Target { return s.active.Load() }

// RoundTrip implements http.RoundTripper.
func (s *Switch) RoundTrip(req *http.Request) (*http.Response, error) {
	s.inflight.Add(1)
	resp, err := s.route(req)
	if err != nil || resp == nil {
		s.inflight.Add(-1)
		return resp, err
	}
	tb := &trackedBody{ReadCloser: resp.Body, done: func() { s.inflight.Add(-1) }}
	if w, ok := resp.Body.(io.Writer); ok {
		resp.Body = &trackedRWBody{trackedBody: tb, w: w}
	} else {
		resp.Body = tb
	}
	return resp, nil
}

func (s *Switch) route(req *http.Request) (*http.Response, error) {
	internal := req.Header.Get(InternalHeader) != ""
	noBeam := req.Header.Get(BeamHeader) == BeamOff
	if internal || noBeam {
		out := req.Clone(req.Context())
		out.Header.Del(InternalHeader)
		out.Header.Del(WorkspaceHeader)
		out.Header.Del(BeamHeader)
		if noBeam && !internal && isLoopback(req.URL.Hostname()) {
			s.unrecorded.Add(1)
		}
		return s.base.RoundTrip(out)
	}
	if req.Header.Get(WorkspaceHeader) != "" || req.Header.Get(BeamHeader) != "" {
		// The provider reads them from the original; they are never sent on.
		out := req.Clone(req.Context())
		out.Header.Del(WorkspaceHeader)
		out.Header.Del(BeamHeader)
		return s.roundTrip(req, out)
	}
	return s.roundTrip(req, req)
}

// roundTrip sends out, choosing its route from orig's headers.
func (s *Switch) roundTrip(orig, req *http.Request) (*http.Response, error) {
	if !isLoopback(req.URL.Hostname()) {
		return s.base.RoundTrip(req)
	}
	noStart := req.Header.Get(NoStartHeader) != ""
	if noStart {
		req = req.Clone(req.Context())
		req.Header.Del(NoStartHeader)
	}
	t := s.active.Load()
	box := s.provider.Load()
	if t == nil && box != nil && noStart {
		t = box.p.Peek(orig)
	}
	if t == nil && noStart {
		return s.base.RoundTrip(req)
	}
	if t == nil {
		if box == nil {
			return s.base.RoundTrip(req)
		}
		var err error
		t, err = box.p.Acquire(orig)
		var refusal *Refusal
		switch {
		case errors.As(err, &refusal):
			return refuse(req, refusal), nil
		case err != nil:
			return nil, err
		case t == nil:
			return s.base.RoundTrip(req)
		}
	}
	r, ok := t.routes[req.URL.Port()]
	if !ok {
		return s.base.RoundTrip(req)
	}
	out := req.Clone(req.Context())
	out.URL.Scheme = "https"
	out.URL.Host = r.AppHost
	out.Host = r.AppHost

	t.inflight.Add(1)
	t.served.Add(1)
	resp, err := r.transport.RoundTrip(out)
	if err != nil {
		t.inflight.Add(-1)
		return nil, err
	}
	tb := &trackedBody{ReadCloser: resp.Body, done: func() { t.inflight.Add(-1) }}
	if w, ok := resp.Body.(io.Writer); ok {
		// An upgraded connection (101) must stay writable, or
		// httputil.ReverseProxy refuses to switch protocols.
		resp.Body = &trackedRWBody{trackedBody: tb, w: w}
	} else {
		resp.Body = tb
	}
	return resp, nil
}

// trackedBody releases its target's in-flight count exactly once, when
// the response body is closed.
type trackedBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *trackedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

type trackedRWBody struct {
	*trackedBody
	w io.Writer
}

func (b *trackedRWBody) Write(p []byte) (int, error) { return b.w.Write(p) }

// refuse answers req with refusal in the error shape of the API it was
// for: Anthropic's for the Messages API, OpenAI's for everything else.
func refuse(req *http.Request, refusal *Refusal) *http.Response {
	var body []byte
	if strings.HasPrefix(req.URL.Path, "/v1/messages") {
		body, _ = json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": refusal.Message},
		})
	} else {
		body, _ = json.Marshal(map[string]any{
			"error": map[string]any{"message": refusal.Message, "type": "server_error", "code": "prism_beam_unavailable"},
		})
	}
	status := refusal.Status
	if status == 0 {
		status = http.StatusServiceUnavailable
	}
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
