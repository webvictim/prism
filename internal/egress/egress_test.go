package egress

import (
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// appServer stands in for a Teleport app: TLS, answering with the Host it
// was asked for so tests can see what reached it.
func appServer(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Seen-Host", r.Host)
		w.Header().Set("X-Seen-Session", r.Header.Get("X-Claude-Code-Session-Id"))
		_, _ = io.WriteString(w, "app:"+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

// tunnelServer stands in for tsh/tbot's plain-HTTP listener.
func tunnelServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tunnel:"+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func port(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var p int
	for _, c := range u.Port() {
		p = p*10 + int(c-'0')
	}
	return p
}

func get(t *testing.T, c *http.Client, raw string, hdr map[string]string) (string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, raw, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", raw, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.Header
}

func TestSwitchPassesThroughWithNothingActive(t *testing.T) {
	tun := tunnelServer(t)
	sw := NewSwitch(nil)
	body, _ := get(t, &http.Client{Transport: sw}, tun.URL+"/v1/messages", nil)
	if body != "tunnel:/v1/messages" {
		t.Fatalf("got %q, want the tunnel", body)
	}
}

func TestSwitchSendsTunnelPortThroughTarget(t *testing.T) {
	tun := tunnelServer(t)
	app, pool := appServer(t)
	appAddr := strings.TrimPrefix(app.URL, "https://")

	sw := NewSwitch(nil)
	target, err := NewTarget("test-beam", Route{
		TunnelPort:  port(t, tun.URL),
		AppHost:     "example.com", // httptest's certificate is valid for it
		ForwardAddr: appAddr,
		RootCAs:     pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prev := sw.Activate(target); prev != nil {
		t.Fatalf("Activate returned %v for a fresh switch", prev)
	}

	c := &http.Client{Transport: sw}
	body, hdr := get(t, c, tun.URL+"/v1/messages", map[string]string{"X-Claude-Code-Session-Id": "s-1"})
	if body != "app:/v1/messages" {
		t.Fatalf("got %q, want the app", body)
	}
	if got := hdr.Get("X-Seen-Host"); got != "example.com" {
		t.Errorf("app saw Host %q, want example.com", got)
	}
	if got := hdr.Get("X-Seen-Session"); got != "s-1" {
		t.Errorf("client headers must reach the app unchanged; session header was %q", got)
	}

	// Deactivating returns traffic to the tunnel, and hands back the target.
	if prev := sw.Activate(nil); prev != target {
		t.Fatalf("Activate(nil) returned %v, want the active target", prev)
	}
	if body, _ := get(t, c, tun.URL+"/v1/messages", nil); body != "tunnel:/v1/messages" {
		t.Fatalf("after deactivation got %q, want the tunnel", body)
	}
}

func TestSwitchLeavesOtherPortsAlone(t *testing.T) {
	tun := tunnelServer(t)
	other := tunnelServer(t)
	app, pool := appServer(t)

	sw := NewSwitch(nil)
	target, err := NewTarget("b", Route{
		TunnelPort:  port(t, tun.URL),
		AppHost:     "example.com",
		ForwardAddr: strings.TrimPrefix(app.URL, "https://"),
		RootCAs:     pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	sw.Activate(target)
	if body, _ := get(t, &http.Client{Transport: sw}, other.URL+"/x", nil); body != "tunnel:/x" {
		t.Fatalf("a port with no route went to %q", body)
	}
}

func TestSwitchRefusesUnverifiableApp(t *testing.T) {
	tun := tunnelServer(t)
	app, _ := appServer(t)

	sw := NewSwitch(nil)
	// No RootCAs: the system roots don't trust httptest's certificate, and
	// the forward must not be a way around verification.
	target, err := NewTarget("b", Route{
		TunnelPort:  port(t, tun.URL),
		AppHost:     "example.com",
		ForwardAddr: strings.TrimPrefix(app.URL, "https://"),
	})
	if err != nil {
		t.Fatal(err)
	}
	sw.Activate(target)
	req, _ := http.NewRequest(http.MethodGet, tun.URL+"/v1/messages", nil)
	if resp, err := (&http.Client{Transport: sw}).Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("request succeeded against an untrusted certificate")
	}
}

func TestNewTargetRejectsBadRoutes(t *testing.T) {
	if _, err := NewTarget("b", Route{TunnelPort: 1, AppHost: "a"}); err == nil {
		t.Error("route without a forward address accepted")
	}
	r := Route{TunnelPort: 1, AppHost: "a", ForwardAddr: "127.0.0.1:2"}
	if _, err := NewTarget("b", r, r); err == nil {
		t.Error("two routes for one tunnel port accepted")
	}
}

func TestTargetCountsRequestsUntilBodyClosed(t *testing.T) {
	tun := tunnelServer(t)
	app, pool := appServer(t)
	sw := NewSwitch(nil)
	target, err := NewTarget("b", Route{
		TunnelPort:  port(t, tun.URL),
		AppHost:     "example.com",
		ForwardAddr: strings.TrimPrefix(app.URL, "https://"),
		RootCAs:     pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	sw.Activate(target)

	req, _ := http.NewRequest(http.MethodGet, tun.URL+"/v1/messages", nil)
	resp, err := (&http.Client{Transport: sw}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if n := target.InFlight(); n != 1 {
		t.Errorf("in flight with body open = %d, want 1", n)
	}
	// Replacing the target doesn't touch a request already using it.
	sw.Activate(nil)
	if n := target.InFlight(); n != 1 {
		t.Errorf("deactivation changed the in-flight count to %d", n)
	}
	_, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body.Close() // a second close must not count twice
	if n := target.InFlight(); n != 0 {
		t.Errorf("in flight after close = %d, want 0", n)
	}
	if n := target.Served(); n != 1 {
		t.Errorf("served = %d, want 1", n)
	}
	if !target.WaitIdle(t.Context(), time.Second) {
		t.Error("WaitIdle reported busy for an idle target")
	}
}

func TestInternalRequestsBypassTheBeam(t *testing.T) {
	var sawHeader string
	tun := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get(InternalHeader)
		_, _ = io.WriteString(w, "tunnel")
	}))
	t.Cleanup(tun.Close)
	app, pool := appServer(t)
	sw := NewSwitch(nil)
	target, err := NewTarget("b", Route{
		TunnelPort:  port(t, tun.URL),
		AppHost:     "example.com",
		ForwardAddr: strings.TrimPrefix(app.URL, "https://"),
		RootCAs:     pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	sw.Activate(target)
	body, _ := get(t, &http.Client{Transport: sw}, tun.URL+"/v1/responses", map[string]string{InternalHeader: "1"})
	if body != "tunnel" {
		t.Fatalf("internal request went to %q, want the tunnel", body)
	}
	if sawHeader != "" {
		t.Error("the internal marker was forwarded upstream")
	}
	if target.Served() != 0 {
		t.Error("internal request counted against the beam")
	}
}

type providerFunc func() (*Target, error)

func (f providerFunc) Acquire(*http.Request) (*Target, error) { return f() }
func (f providerFunc) Peek(*http.Request) *Target             { return nil }

func TestSwitchAsksProviderWhenNothingActive(t *testing.T) {
	tun := tunnelServer(t)
	app, pool := appServer(t)
	target, err := NewTarget("beam", Route{
		TunnelPort: port(t, tun.URL), AppHost: "anthropic.example.com",
		ForwardAddr: strings.TrimPrefix(app.URL, "https://"), RootCAs: pool,
	})
	if err != nil {
		t.Fatal(err)
	}
	sw := NewSwitch(nil)
	c := &http.Client{Transport: sw}

	sw.SetProvider(providerFunc(func() (*Target, error) { return target, nil }))
	if body, _ := get(t, c, tun.URL+"/v1/messages", nil); body != "app:/v1/messages" {
		t.Errorf("with a target from the provider got %q, want the app", body)
	}
	if target.Served() != 1 {
		t.Errorf("served = %d", target.Served())
	}

	sw.SetProvider(providerFunc(func() (*Target, error) { return nil, nil }))
	if body, _ := get(t, c, tun.URL+"/v1/messages", nil); body != "tunnel:/v1/messages" {
		t.Errorf("with no target from the provider got %q, want the tunnel", body)
	}
}

func TestSwitchRefusalIsAnAPIError(t *testing.T) {
	tun := tunnelServer(t)
	sw := NewSwitch(nil)
	sw.SetProvider(providerFunc(func() (*Target, error) {
		return nil, &Refusal{Status: http.StatusServiceUnavailable, Message: "run `prism login`"}
	}))
	c := &http.Client{Transport: sw}
	for path, want := range map[string]string{
		"/v1/messages":  `{"error":{"message":"run ` + "`prism login`" + `","type":"api_error"},"type":"error"}`,
		"/v1/responses": `{"error":{"code":"prism_beam_unavailable","message":"run ` + "`prism login`" + `","type":"server_error"}}`,
	} {
		resp, err := c.Post(tun.URL+path, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable || string(b) != want {
			t.Errorf("%s: %d %s, want 503 %s", path, resp.StatusCode, b, want)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content type %q", path, ct)
		}
	}
}

func TestNoStartHeaderNeverAsksProvider(t *testing.T) {
	tun := tunnelServer(t)
	sw := NewSwitch(nil)
	asked := 0
	sw.SetProvider(providerFunc(func() (*Target, error) { asked++; return nil, &Refusal{Message: "no"} }))
	body, _ := get(t, &http.Client{Transport: sw}, tun.URL+"/v1/messages", map[string]string{NoStartHeader: "1"})
	if body != "tunnel:/v1/messages" || asked != 0 {
		t.Errorf("got %q after asking the provider %d times; want the tunnel, unasked", body, asked)
	}
}

func TestWorkspaceHeaderReachesProviderButNotUpstream(t *testing.T) {
	var upstream string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream = r.Header.Get(WorkspaceHeader)
	}))
	defer srv.Close()
	sw := NewSwitch(nil)
	var seen string
	sw.SetProvider(providerFuncReq(func(r *http.Request) (*Target, error) {
		seen = r.Header.Get(WorkspaceHeader)
		return nil, nil
	}))
	get(t, &http.Client{Transport: sw}, srv.URL+"/v1/messages", map[string]string{WorkspaceHeader: "/Users/me/secret-repo"})
	if seen != "/Users/me/secret-repo" {
		t.Errorf("provider saw %q", seen)
	}
	if upstream != "" {
		t.Errorf("the workspace path went upstream: %q", upstream)
	}
}

type providerFuncReq func(*http.Request) (*Target, error)

func (f providerFuncReq) Acquire(r *http.Request) (*Target, error) { return f(r) }
func (f providerFuncReq) Peek(*http.Request) *Target               { return nil }
