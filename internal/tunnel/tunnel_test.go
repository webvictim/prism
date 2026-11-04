package tunnel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every probe URL has to pass: in tbot mode they are the two tunnel
// services, and either one being down is a tunnel that's down.
func TestProbeHealthNeedsEveryURL(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ready.Close()
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()

	s := &Service{cfg: Config{HealthProbeURLs: []string{ready.URL, ready.URL}}}
	if err := s.probeHealth(context.Background()); err != nil {
		t.Errorf("all ready: %v", err)
	}
	s.cfg.HealthProbeURLs = []string{ready.URL, down.URL}
	err := s.probeHealth(context.Background())
	if err == nil || !strings.Contains(err.Error(), down.URL) {
		t.Errorf("one down: err = %v, want a failure naming %s", err, down.URL)
	}
}
