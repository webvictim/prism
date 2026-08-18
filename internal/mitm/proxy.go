package mitm

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
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
)

const anthropicHost = "api.anthropic.com"

// Handler implements http.Handler for HTTP CONNECT requests. It
// intercepts connections to api.anthropic.com (TLS-terminating with a
// locally-generated cert, applying Bedrock scrubbing, and forwarding
// to the Anthropic tunnel), and blind-tunnels everything else.
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
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
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
	target, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", h.AnthropicPort))
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		FlushInterval: -1,
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		if h.Logger != nil {
			h.Logger.Printf("mitm: upstream error: %s %s: %v", r.Method, r.URL.Path, err)
		}
		http.Error(w, fmt.Sprintf("prism: anthropic gateway unavailable: %v", err), http.StatusBadGateway)
	}

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.scrubAndProxy(w, r, proxy)
		}),
		ReadHeaderTimeout: 30 * time.Second,
	}

	// Serve exactly one connection.
	listener := &singleConnListener{conn: conn}
	_ = server.Serve(listener)
}

// scrubAndProxy applies Bedrock scrubbing (same as the router's scrub
// middleware) then proxies to the Anthropic tunnel. Only /v1/messages
// requests are routed to the local tunnel; all other paths are
// forwarded to the real api.anthropic.com with credentials intact
// (needed for Remote Control, feature flags, telemetry, etc.).
func (h *Handler) scrubAndProxy(w http.ResponseWriter, r *http.Request, proxy *httputil.ReverseProxy) {
	if !strings.HasPrefix(r.URL.Path, "/v1/messages") {
		h.forwardUpstream(w, r)
		return
	}

	// Strip auth headers — tunnel provides auth via mTLS.
	r.Header.Del("Authorization")
	r.Header.Del("X-Api-Key")

	if r.Method != http.MethodPost {
		proxy.ServeHTTP(w, r)
		return
	}
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		proxy.ServeHTTP(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		http.Error(w, "prism: read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if shouldShortCircuit(body) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prism: output_config.format is not supported by the cluster gateway"}}`+"\n")
		return
	}

	body = h.scrubBody(body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", fmt.Sprint(len(body)))
	proxy.ServeHTTP(w, r)
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

var stripFields = []string{
	"metadata",
	"context_management",
	"thinking",
}

const nonStreamMaxTokensCap = 8192

func (h *Handler) scrubBody(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	changed := false
	for _, k := range stripFields {
		if _, ok := obj[k]; ok {
			delete(obj, k)
			changed = true
		}
	}
	streaming, _ := obj["stream"].(bool)
	if !streaming {
		if mt, ok := numberAsFloat(obj["max_tokens"]); ok && mt > nonStreamMaxTokensCap {
			if h.Debug && h.Logger != nil {
				h.Logger.Printf("mitm-scrub: capping max_tokens %.0f → %d (non-streaming)", mt, nonStreamMaxTokensCap)
			}
			obj["max_tokens"] = nonStreamMaxTokensCap
			changed = true
		}
	}
	if !changed {
		return body
	}
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return rewritten
}

func shouldShortCircuit(body []byte) bool {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	oc, _ := obj["output_config"].(map[string]any)
	if oc == nil {
		return false
	}
	_, hasFormat := oc["format"]
	return hasFormat
}

func numberAsFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	}
	return 0, false
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
