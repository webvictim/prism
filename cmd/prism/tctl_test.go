package main

import (
	"slices"
	"testing"
)

// Resources landing on the wrong cluster is the expensive mistake here,
// so every tctl call prism makes names the cluster.
func TestTctlArgs(t *testing.T) {
	want := []string{"--auth-server=cluster.example.com:443", "bots", "rm", "prism-bot-gaia"}
	if got := tctlArgs("cluster.example.com:443", "bots", "rm", "prism-bot-gaia"); !slices.Equal(got, want) {
		t.Errorf("tctlArgs = %v, want %v", got, want)
	}
	// A proxy written without a port is a normal thing to find in
	// prism's config, and not something tctl accepts.
	if got := tctlArgs("cluster.example.com", "status"); got[0] != "--auth-server=cluster.example.com:443" {
		t.Errorf("tctlArgs = %v, want the default port added", got)
	}
	// With nothing configured, tctl keeps its own default rather than
	// being pointed at an empty address.
	if got := tctlArgs("", "status"); !slices.Equal(got, []string{"status"}) {
		t.Errorf("tctlArgs with no proxy = %v, want the args untouched", got)
	}
}

func TestWithDefaultPort(t *testing.T) {
	for in, want := range map[string]string{
		"cluster.example.com":      "cluster.example.com:443",
		"cluster.example.com:443":  "cluster.example.com:443",
		"cluster.example.com:3080": "cluster.example.com:3080",
		"":                         "",
	} {
		if got := withDefaultPort(in); got != want {
			t.Errorf("withDefaultPort(%q) = %q, want %q", in, got, want)
		}
	}
}
