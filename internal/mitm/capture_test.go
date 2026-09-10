package mitm

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/usage"
)

// logWriter is a concurrency-safe log sink: the proxy logs from the
// server's goroutine while the test reads from its own.
type logWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *logWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *logWriter) logger() *log.Logger { return log.New(w, "", 0) }

// mitmUsageHarness stands up the forward proxy in front of a fake
// Anthropic tunnel and returns a function that sends one absolute-form
// request through it, plus the recorded usage.
func mitmUsageHarness(t *testing.T, logger *logWriter, respond http.HandlerFunc) (send func(body string), records func() []usage.Record) {
	t.Helper()

	tunnel := httptest.NewServer(respond)
	t.Cleanup(tunnel.Close)

	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(tunnel.URL, "http://"))
	var tunnelPort int
	fmt.Sscanf(portStr, "%d", &tunnelPort)

	dir := t.TempDir()
	uw, err := usage.NewWriter(dir)
	if err != nil {
		t.Fatal(err)
	}

	handler := &Handler{
		AnthropicPort: tunnelPort,
		UsageWriter:   uw,
		Proxy:         "teleport.example.com:443",
	}
	if logger != nil {
		handler.Logger = logger.logger()
	}

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })
	proxySrv := &http.Server{Handler: handler}
	go func() { _ = proxySrv.Serve(proxyLn) }()
	t.Cleanup(func() { _ = proxySrv.Close() })

	send = func(body string) {
		t.Helper()
		conn, err := net.Dial("tcp", proxyLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		const target = "https://api.anthropic.com/v1/messages"
		fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: api.anthropic.com\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
			target, len(body), body)
		if _, err := io.ReadAll(conn); err != nil {
			t.Fatal(err)
		}
	}

	records = func() []usage.Record {
		t.Helper()
		_ = uw.Close()
		recs, err := usage.Load(dir, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return recs
	}
	return send, records
}

// The forward proxy used to overwrite the model with whatever the
// request named, so a request that omits the field — or names an alias
// the gateway resolves to something else — was recorded with no model
// at all and logged as "usage: ?".
func TestForwardProxyRecordsResponseModel(t *testing.T) {
	for _, tt := range []struct {
		name        string
		requestBody string
		want        string
	}{
		{
			name:        "request omits model",
			requestBody: `{"max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
			want:        "claude-opus-5",
		},
		{
			name:        "gateway aliases the requested model",
			requestBody: `{"model":"claude-opus-latest","max_tokens":8,"messages":[]}`,
			want:        "claude-opus-5",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			send, records := mitmUsageHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"model":"claude-opus-5","usage":{"input_tokens":9,"output_tokens":4}}`))
			})
			send(tt.requestBody)

			recs := records()
			if len(recs) != 1 {
				t.Fatalf("got %d usage records, want 1", len(recs))
			}
			if recs[0].Model != tt.want {
				t.Errorf("model = %q, want %q", recs[0].Model, tt.want)
			}
			if recs[0].Backend != "anthropic" {
				t.Errorf("backend = %q, want anthropic", recs[0].Backend)
			}
			if recs[0].InputTokens != 9 || recs[0].OutputTokens != 4 {
				t.Errorf("tokens = in %d/out %d, want 9/4", recs[0].InputTokens, recs[0].OutputTokens)
			}
		})
	}
}

// Streaming responses carry the model on message_start.
func TestForwardProxyRecordsStreamingUsage(t *testing.T) {
	send, records := mitmUsageHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: message_start\n"+
			`data: {"type":"message_start","message":{"model":"claude-opus-5","usage":{"input_tokens":30,"output_tokens":1,"cache_read_input_tokens":12}}}`+"\n\n")
		_, _ = io.WriteString(w, "event: message_delta\n"+
			`data: {"type":"message_delta","usage":{"output_tokens":17}}`+"\n\n")
	})
	send(`{"max_tokens":64,"stream":true,"messages":[]}`)

	recs := records()
	if len(recs) != 1 {
		t.Fatalf("got %d usage records, want 1", len(recs))
	}
	if recs[0].Model != "claude-opus-5" {
		t.Errorf("model = %q, want claude-opus-5", recs[0].Model)
	}
	if recs[0].InputTokens != 30 || recs[0].OutputTokens != 17 {
		t.Errorf("tokens = in %d/out %d, want 30/17", recs[0].InputTokens, recs[0].OutputTokens)
	}
	if recs[0].CacheRead != 12 {
		t.Errorf("cacheRead = %d, want 12", recs[0].CacheRead)
	}
}

// A Handler with a usage writer but no logger must not panic: the usage
// summary was the one log call in this package that was not nil-guarded.
func TestForwardProxyUsageWithNilLogger(t *testing.T) {
	send, records := mitmUsageHarness(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-opus-5","usage":{"input_tokens":1,"output_tokens":1}}`))
	})
	send(`{"max_tokens":8,"messages":[]}`)

	if len(records()) != 1 {
		t.Error("no usage record written with a nil logger")
	}
}

// The request line and the usage figures are one line, so they can be
// paired reliably even when requests overlap.
func TestForwardProxyLogsOneCombinedLine(t *testing.T) {
	lw := &logWriter{}
	send, _ := mitmUsageHarness(t, lw, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-opus-5","usage":{"input_tokens":9,"output_tokens":4,` +
			`"cache_read_input_tokens":18807,"cache_creation_input_tokens":209741}}`))
	})
	send(`{"max_tokens":8,"messages":[]}`)

	got := strings.TrimSpace(lw.String())
	if strings.Count(got, "\n") != 0 {
		t.Errorf("expected a single log line, got:\n%s", got)
	}
	for _, want := range []string{
		"POST /v1/messages 200",
		"model=claude-opus-5",
		"in=9 out=4",
		"cache_read=18807 cache_write=209741",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log line missing %q; line was:\n%s", want, got)
		}
	}
	if strings.Contains(got, "usage: ") {
		t.Errorf("separate usage line still emitted:\n%s", got)
	}
}
