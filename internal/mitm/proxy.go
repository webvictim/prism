package mitm

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/webvictim/prism/internal/scrub"
)

const anthropicHost = "api.anthropic.com"

// Handler implements http.Handler for forward-proxy requests. CONNECT
// requests to api.anthropic.com are intercepted (TLS-terminating with a
// locally-generated cert, applying Bedrock scrubbing, and forwarding to
// the Anthropic tunnel) while CONNECTs to other hosts are
// blind-tunneled. Absolute-form plain-HTTP requests (clients like
// axios/undici that use HTTPS_PROXY without CONNECT — e.g. the Claude
// Code Remote Control bridge client) are served the same way: the
// anthropic host gets the scrub/tunnel/upstream split, other hosts get
// a generic forward.
type Handler struct {
	CA            *x509.Certificate
	CAKey         *ecdsa.PrivateKey
	AnthropicPort int
	Logger        *log.Logger
	Debug         bool

	// certCache avoids re-issuing certs on every CONNECT.
	certMu    sync.Mutex
	certCache map[string]*tls.Certificate

	// upstreamProxy forwards non-/v1/messages traffic to the real
	// api.anthropic.com (Remote Control, feature flags, etc.).
	upstreamOnce  sync.Once
	upstreamProxy *httputil.ReverseProxy

	// tunnelProxy forwards /v1/messages to the local Anthropic tunnel.
	tunnelOnce  sync.Once
	tunnelProxy *httputil.ReverseProxy

	// forwardProxy serves absolute-form requests for non-anthropic
	// hosts (the plain-HTTP analogue of the blind tunnel).
	forwardOnce  sync.Once
	forwardProxy *httputil.ReverseProxy
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		if r.URL.IsAbs() {
			h.handleAbsoluteForm(w, r)
			return
		}
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}

	if host == anthropicHost {
		h.handleAnthropicConnect(w, r)
	} else {
		h.handleBlindTunnel(w, r)
	}
}

// handleAbsoluteForm serves a plain-HTTP proxy request (absolute URI in
// the request line, no CONNECT).
func (h *Handler) handleAbsoluteForm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Hostname() == anthropicHost {
		h.scrubAndProxy(w, r)
		return
	}
	if h.Debug && h.Logger != nil {
		h.Logger.Printf("mitm: absolute-form forward to %s: %s %s", r.URL.Host, r.Method, r.URL.Path)
	}
	h.forwardOnce.Do(func() {
		h.forwardProxy = &httputil.ReverseProxy{
			// The inbound URL is already absolute; keep it as the target.
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.Host = pr.In.URL.Host
			},
			FlushInterval: -1,
		}
		h.forwardProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			if h.Logger != nil {
				h.Logger.Printf("mitm: forward %s error: %s %s: %v", r.URL.Host, r.Method, r.URL.Path, err)
			}
			http.Error(w, fmt.Sprintf("prism: %s unavailable: %v", r.URL.Host, err), http.StatusBadGateway)
		}
	})
	h.forwardProxy.ServeHTTP(w, r)
}

func (h *Handler) handleAnthropicConnect(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	conn, _, err := hj.Hijack()
	if err != nil {
		if h.Logger != nil {
			h.Logger.Printf("mitm: hijack failed: %v", err)
		}
		return
	}
	defer conn.Close()

	cert, err := h.getCert(anthropicHost)
	if err != nil {
		if h.Logger != nil {
			h.Logger.Printf("mitm: issue cert: %v", err)
		}
		return
	}

	tlsConn := tls.Server(conn, &tls.Config{
		Certificates: []tls.Certificate{*cert},
	})
	if err := tlsConn.Handshake(); err != nil {
		if h.Logger != nil {
			h.Logger.Printf("mitm: tls handshake: %v", err)
		}
		return
	}
	defer tlsConn.Close()

	h.serveHTTPOverTLS(tlsConn)
}

// serveHTTPOverTLS reads HTTP requests from the TLS connection and
// proxies them to the local Anthropic tunnel with scrubbing applied.
func (h *Handler) serveHTTPOverTLS(conn net.Conn) {
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.scrubAndProxy(w, r)
		}),
		ReadHeaderTimeout: 30 * time.Second,
	}

	// Serve exactly one connection.
	listener := &singleConnListener{conn: conn}
	_ = server.Serve(listener)
}

// getTunnelProxy lazily builds the ReverseProxy for the local Anthropic
// tunnel, shared by the TLS-intercept and absolute-form paths.
func (h *Handler) getTunnelProxy() *httputil.ReverseProxy {
	h.tunnelOnce.Do(func() {
		target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", h.AnthropicPort))
		h.tunnelProxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = target.Host
			},
			FlushInterval: -1,
			ModifyResponse: func(resp *http.Response) error {
				if h.Debug && h.Logger != nil {
					h.Logger.Printf("mitm: tunnel response: %d %s", resp.StatusCode, resp.Status)
				}
				return nil
			},
		}
		h.tunnelProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			if h.Logger != nil {
				h.Logger.Printf("mitm: upstream error: %s %s: %v", r.Method, r.URL.Path, err)
			}
			http.Error(w, fmt.Sprintf("prism: anthropic gateway unavailable: %v", err), http.StatusBadGateway)
		}
	})
	return h.tunnelProxy
}

// scrubAndProxy applies the shared Bedrock scrubbing (identical to the
// router's middleware) then proxies to the Anthropic tunnel. Only
// /v1/messages requests are routed to the local tunnel; all other paths
// are forwarded to the real api.anthropic.com with credentials intact
// (needed for Remote Control, feature flags, telemetry, etc.).
func (h *Handler) scrubAndProxy(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
		h.forwardUpstream(w, r)
		return
	}
	if scrub.AnthropicRequest(w, r, h.Logger, h.Debug) {
		return
	}
	h.getTunnelProxy().ServeHTTP(w, r)
}

// forwardUpstream proxies the request to the real api.anthropic.com
// over TLS with original credentials intact. Used for non-model paths
// (Remote Control, feature flags, telemetry, etc.).
func (h *Handler) forwardUpstream(w http.ResponseWriter, r *http.Request) {
	h.upstreamOnce.Do(func() {
		target, _ := url.Parse("https://api.anthropic.com")
		h.upstreamProxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.Host = "api.anthropic.com"
			},
			FlushInterval: -1,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					ServerName: "api.anthropic.com",
				},
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		}
		h.upstreamProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			if h.Logger != nil {
				h.Logger.Printf("mitm: upstream api.anthropic.com error: %s %s: %v", r.Method, r.URL.Path, err)
			}
			http.Error(w, fmt.Sprintf("prism: api.anthropic.com unavailable: %v", err), http.StatusBadGateway)
		}
	})
	if h.Debug && h.Logger != nil {
		h.Logger.Printf("mitm: forwarding to real api.anthropic.com: %s %s", r.Method, r.URL.Path)
	}
	h.upstreamProxy.ServeHTTP(w, r)
}

func (h *Handler) handleBlindTunnel(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	target := r.Host
	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		http.Error(w, fmt.Sprintf("prism: connect to %s: %v", target, err), http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	w.WriteHeader(http.StatusOK)
	conn, _, err := hj.Hijack()
	if err != nil {
		if h.Logger != nil {
			h.Logger.Printf("mitm: hijack failed for blind tunnel to %s: %v", target, err)
		}
		return
	}
	defer conn.Close()

	if h.Debug && h.Logger != nil {
		h.Logger.Printf("mitm: blind tunnel to %s", target)
	}

	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, conn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, upstream); done <- struct{}{} }()
	<-done
}

func (h *Handler) getCert(host string) (*tls.Certificate, error) {
	h.certMu.Lock()
	defer h.certMu.Unlock()

	if h.certCache == nil {
		h.certCache = make(map[string]*tls.Certificate)
	}
	if cert, ok := h.certCache[host]; ok {
		return cert, nil
	}

	cert, err := IssueCert(h.CA, h.CAKey, host)
	if err != nil {
		return nil, err
	}
	h.certCache[host] = cert
	return cert, nil
}

// singleConnListener adapts a net.Conn into a net.Listener that
// returns the conn exactly once, then blocks until the conn closes.
type singleConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	accepted := false
	l.once.Do(func() {
		if l.done == nil {
			l.done = make(chan struct{})
		}
		accepted = true
	})
	if accepted {
		return &notifyCloseConn{Conn: l.conn, done: l.done}, nil
	}
	<-l.done
	return nil, fmt.Errorf("listener closed")
}

func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

type notifyCloseConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *notifyCloseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}
