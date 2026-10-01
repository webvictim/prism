package proxyerr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsClientCanceled(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bare context.Canceled", context.Canceled, true},
		{"wrapped once", fmt.Errorf("dial: %w", context.Canceled), true},
		{"wrapped twice", fmt.Errorf("proxy: %w", fmt.Errorf("dial: %w", context.Canceled)), true},
		// A gateway timeout is a real upstream failure, not a client
		// going away: it must keep its 502.
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"wrapped deadline exceeded", fmt.Errorf("dial: %w", context.DeadlineExceeded), false},
		{"ordinary error", errors.New("connection refused"), false},
		// Matching on message text rather than errors.Is would catch this.
		{"error merely mentioning cancellation", errors.New("context canceled"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsClientCanceled(tt.err); got != tt.want {
				t.Errorf("IsClientCanceled(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestHandleClientCanceled(t *testing.T) {
	t.Run("handles a cancel", func(t *testing.T) {
		var buf bytes.Buffer
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

		if !HandleClientCanceled(w, r, context.Canceled, log.New(&buf, "", 0), "router: anthropic") {
			t.Fatal("HandleClientCanceled = false, want true")
		}
		if w.Code != StatusClientClosedRequest {
			t.Errorf("status = %d, want %d", w.Code, StatusClientClosedRequest)
		}
		if got := w.Body.String(); got != "" {
			t.Errorf("body = %q, want empty — the client has already gone", got)
		}
		got := buf.String()
		if !strings.Contains(got, "router: anthropic: client canceled POST /v1/messages") {
			t.Errorf("log line = %q, want the label, method and path", got)
		}
		if strings.Contains(got, "upstream error") {
			t.Errorf("log line reads as an upstream error: %q", got)
		}
	})

	t.Run("declines anything else and writes nothing", func(t *testing.T) {
		var buf bytes.Buffer
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

		if HandleClientCanceled(w, r, errors.New("connection refused"), log.New(&buf, "", 0), "mitm") {
			t.Fatal("HandleClientCanceled = true, want false")
		}
		// The caller still owns the response: the helper must not have
		// committed a status, or http.Error would be too late.
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want it untouched (%d)", w.Code, http.StatusOK)
		}
		if got := buf.String(); got != "" {
			t.Errorf("log = %q, want nothing — the caller logs upstream errors", got)
		}
	})

	t.Run("tolerates a nil logger", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)

		if !HandleClientCanceled(w, r, context.Canceled, nil, "mitm") {
			t.Fatal("HandleClientCanceled = false, want true")
		}
		if w.Code != StatusClientClosedRequest {
			t.Errorf("status = %d, want %d", w.Code, StatusClientClosedRequest)
		}
	})
}
