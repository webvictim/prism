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
