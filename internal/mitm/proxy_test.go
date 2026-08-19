package mitm

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureCA(t *testing.T) {
	dir := t.TempDir()
	certPath, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if certPath != filepath.Join(dir, caCertFile) {
		t.Fatalf("unexpected cert path: %s", certPath)
	}
	if _, err := os.Stat(filepath.Join(dir, caCertFile)); err != nil {
		t.Fatalf("ca.pem not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, caKeyFile)); err != nil {
		t.Fatalf("ca-key.pem not written: %v", err)
	}

	// Second call should be a no-op (idempotent).
	certPath2, err := EnsureCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if certPath2 != certPath {
		t.Fatalf("expected same path, got %s vs %s", certPath, certPath2)
	}
}

func TestLoadCA(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCA(dir); err != nil {
		t.Fatal(err)
	}
	cert, key, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cert == nil || key == nil {
		t.Fatal("cert or key is nil")
	}
	if !cert.IsCA {
		t.Fatal("cert is not a CA")
	}
}

func TestIssueCert(t *testing.T) {
	dir := t.TempDir()
	if _, err := EnsureCA(dir); err != nil {
		t.Fatal(err)
	}
	ca, caKey, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	leaf, err := IssueCert(ca, caKey, "api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	if leaf == nil {
		t.Fatal("leaf cert is nil")
	}
	if len(leaf.Certificate) == 0 {
		t.Fatal("no certificate in chain")
	}

	// Verify the leaf can be parsed and validates against the CA.
	parsed, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := parsed.Verify(x509.VerifyOptions{
		Roots:   pool,
		DNSName: "api.anthropic.com",
	}); err != nil {
		t.Fatalf("leaf cert does not verify: %v", err)
	}
}

func TestHandlerBlindTunnel(t *testing.T) {
	// Start a TCP echo server to simulate an upstream host.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()

	dir := t.TempDir()
	if _, err := EnsureCA(dir); err != nil {
		t.Fatal(err)
	}
	ca, caKey, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	handler := &Handler{
		CA:            ca,
		CAKey:         caKey,
		AnthropicPort: 0, // not used for blind tunnel
	}

	// Start an HTTP server with the handler.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()
	proxySrv := &http.Server{Handler: handler}
	go proxySrv.Serve(proxyLn)
	defer proxySrv.Close()

	// Issue a CONNECT to the echo server's address (not api.anthropic.com).
	conn, err := net.Dial("tcp", proxyLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n",
		echoLn.Addr().String(), echoLn.Addr().String())
	if _, err := conn.Write([]byte(connectReq)); err != nil {
		t.Fatal(err)
	}

	// Read 200 response.
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	resp := string(buf[:n])
	if !strings.Contains(resp, "200") {
		t.Fatalf("expected 200, got: %s", resp)
	}

	// Send data through the tunnel and verify echo.
	msg := "hello from blind tunnel"
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	n, err = conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != msg {
		t.Fatalf("echo mismatch: got %q, want %q", string(buf[:n]), msg)
	}
}

func TestHandlerAnthropicIntercept(t *testing.T) {
	// Start a fake "anthropic tunnel" HTTP server.
	anthropicBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"path":"%s","auth":"%s"}`, r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer anthropicBackend.Close()

	// Parse backend port.
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(anthropicBackend.URL, "http://"))
	var backendPort int
	fmt.Sscanf(portStr, "%d", &backendPort)

	dir := t.TempDir()
	if _, err := EnsureCA(dir); err != nil {
		t.Fatal(err)
	}
	ca, caKey, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	handler := &Handler{
		CA:            ca,
		CAKey:         caKey,
		AnthropicPort: backendPort,
	}

	// Start proxy server.
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()
	proxySrv := &http.Server{Handler: handler}
	go proxySrv.Serve(proxyLn)
	defer proxySrv.Close()

	// Configure an HTTP client that uses our proxy and trusts our CA.
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	transport := &http.Transport{
		Proxy: http.ProxyURL(&url.URL{
			Scheme: "http",
			Host:   proxyLn.Addr().String(),
		}),
		TLSClientConfig: &tls.Config{
			RootCAs: pool,
		},
	}
	client := &http.Client{Transport: transport}

	// Make an HTTPS request to api.anthropic.com/v1/messages (intercepted by our proxy).
	// Use POST since that's what gets routed to the tunnel.
	reqBody := strings.NewReader(`{"model":"claude-3","max_tokens":100,"messages":[]}`)
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", reqBody)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-test-should-be-stripped")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	// The fake backend should see the request at /v1/messages with auth stripped.
	if !strings.Contains(string(body), `"path":"/v1/messages"`) {
		t.Fatalf("unexpected body: %s", body)
	}
	if strings.Contains(string(body), `"auth":"Bearer`) {
		t.Fatal("Authorization header was not stripped")
	}
}

// TestHandlerAnthropicScrub verifies the intercept path applies the same
// Bedrock scrubbing as the router: cache_control scope and unsupported
// top-level fields are removed, while API headers and unknown tool keys
// (which the gateway ignores) pass through untouched.
func TestHandlerAnthropicScrub(t *testing.T) {
	type seen struct {
		body    []byte
		version string
		beta    string
	}
	seenCh := make(chan seen, 1)
	anthropicBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenCh <- seen{
			body:    b,
			version: r.Header.Get("Anthropic-Version"),
			beta:    r.Header.Get("Anthropic-Beta"),
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer anthropicBackend.Close()

	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(anthropicBackend.URL, "http://"))
	var backendPort int
	fmt.Sscanf(portStr, "%d", &backendPort)

	dir := t.TempDir()
	if _, err := EnsureCA(dir); err != nil {
		t.Fatal(err)
	}
	ca, caKey, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	handler := &Handler{
		CA:            ca,
		CAKey:         caKey,
		AnthropicPort: backendPort,
	}

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()
	proxySrv := &http.Server{Handler: handler}
	go proxySrv.Serve(proxyLn)
	defer proxySrv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(&url.URL{Scheme: "http", Host: proxyLn.Addr().String()}),
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}}

	reqBody := `{"model":"claude-3","max_tokens":100,"stream":true,"thinking":{"type":"enabled"},"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}],"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"},"eager_input_streaming":true},{"name":"DeferredToolPlaceholder","description":"d","input_schema":{"type":"object"},"defer_loading":true}],"messages":[]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, b)
	}

	got := <-seenCh
	for _, field := range []string{"thinking", "scope"} {
		if strings.Contains(string(got.body), field) {
			t.Errorf("backend body still contains %q: %s", field, got.body)
		}
	}
	for _, keep := range []string{"Bash", "DeferredToolPlaceholder", "eager_input_streaming", "defer_loading"} {
		if !strings.Contains(string(got.body), keep) {
			t.Errorf("backend body lost %q: %s", keep, got.body)
		}
	}
	if got.version != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want it forwarded intact", got.version)
	}
	if got.beta != "oauth-2025-04-20" {
		t.Errorf("Anthropic-Beta = %q, want it forwarded intact", got.beta)
	}
}

// TestHandlerAbsoluteForm verifies plain-HTTP proxy requests (absolute
// URI in the request line, no CONNECT — how axios-style clients such as
// the Remote Control bridge use HTTPS_PROXY). Anthropic-host
// /v1/messages requests must be scrubbed and sent to the tunnel; other
// hosts must be forwarded as-is.
func TestHandlerAbsoluteForm(t *testing.T) {
	tunnelBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"backend":"tunnel","path":"%s","body":%q}`, r.URL.Path, b)
	}))
	defer tunnelBackend.Close()

	otherBackend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"backend":"other","path":"%s","auth":"%s"}`, r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer otherBackend.Close()

	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(tunnelBackend.URL, "http://"))
	var tunnelPort int
	fmt.Sscanf(portStr, "%d", &tunnelPort)

	handler := &Handler{AnthropicPort: tunnelPort}

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxyLn.Close()
	proxySrv := &http.Server{Handler: handler}
	go proxySrv.Serve(proxyLn)
	defer proxySrv.Close()

	// Write an absolute-form request by hand, the way the bridge
	// client does: no CONNECT, https:// URL in the request line.
	sendAbsolute := func(rawURL, body string) string {
		t.Helper()
		conn, err := net.Dial("tcp", proxyLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		u, _ := url.Parse(rawURL)
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nAuthorization: Bearer tok\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			rawURL, u.Host, len(body), body)
		resp, err := io.ReadAll(conn)
		if err != nil {
			t.Fatal(err)
		}
		return string(resp)
	}

	t.Run("anthropic messages goes to tunnel scrubbed", func(t *testing.T) {
		resp := sendAbsolute("https://api.anthropic.com/v1/messages",
			`{"model":"claude-3","max_tokens":10,"thinking":{"type":"enabled"},"messages":[]}`)
		if !strings.Contains(resp, `"backend":"tunnel"`) {
			t.Fatalf("request did not reach the tunnel: %s", resp)
		}
		if strings.Contains(resp, "thinking") {
			t.Errorf("body was not scrubbed: %s", resp)
		}
	})

	t.Run("other host is forwarded with credentials", func(t *testing.T) {
		resp := sendAbsolute(otherBackend.URL+"/v1/environments/bridge", `{}`)
		if !strings.Contains(resp, `"backend":"other"`) {
			t.Fatalf("request did not reach the other host: %s", resp)
		}
		if !strings.Contains(resp, `"path":"/v1/environments/bridge"`) {
			t.Errorf("path not preserved: %s", resp)
		}
		if !strings.Contains(resp, `"auth":"Bearer tok"`) {
			t.Errorf("credentials not preserved for non-anthropic host: %s", resp)
		}
	})
}
