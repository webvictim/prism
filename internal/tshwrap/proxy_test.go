package tshwrap

import (
	"slices"
	"testing"
	"time"
)

// Every tsh invocation prism makes has to name the cluster, or tsh acts
// on whichever profile the user's shell last logged into.
func TestProxyArgs(t *testing.T) {
	t.Cleanup(func() { SetProxy("") })

	SetProxy("")
	if got := proxyArgs([]string{"status", "--format=json"}); !slices.Equal(got, []string{"status", "--format=json"}) {
		t.Errorf("proxyArgs with no proxy = %v, want the args untouched", got)
	}

	SetProxy("cluster.example.com:443")
	if got := Proxy(); got != "cluster.example.com:443" {
		t.Errorf("Proxy() = %q", got)
	}
	// The flag is global to tsh, so it has to come before the
	// subcommand.
	want := []string{"--proxy=cluster.example.com:443", "apps", "login", "anthropic"}
	if got := proxyArgs([]string{"apps", "login", "anthropic"}); !slices.Equal(got, want) {
		t.Errorf("proxyArgs = %v, want %v", got, want)
	}
}

// StatusJSON reports "nobody is logged in" as a zero Status rather than
// an error, and a zero expiry reads as expired — so without LoggedIn a
// machine that never logged in gets told its login expired in year 1.
func TestStatusLoggedIn(t *testing.T) {
	var nobody *Status
	if nobody.LoggedIn() {
		t.Error("a nil Status reports as logged in")
	}
	if (&Status{}).LoggedIn() {
		t.Error("a zero Status reports as logged in")
	}
	if (&Status{Cluster: "c"}).LoggedIn() {
		t.Error("a Status with no expiry reports as logged in")
	}
	live := &Status{Cluster: "c", ValidUntil: time.Now().Add(time.Hour)}
	if !live.LoggedIn() || live.IsExpired() {
		t.Errorf("a live profile reports LoggedIn=%v IsExpired=%v", live.LoggedIn(), live.IsExpired())
	}
	stale := &Status{Cluster: "c", ValidUntil: time.Now().Add(-time.Hour)}
	if !stale.LoggedIn() || !stale.IsExpired() {
		t.Errorf("a lapsed profile reports LoggedIn=%v IsExpired=%v", stale.LoggedIn(), stale.IsExpired())
	}
}
