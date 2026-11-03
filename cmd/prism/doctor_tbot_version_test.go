package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseTbotVersion(t *testing.T) {
	// Verbatim from tbot 18.11.7.
	got, err := parseTbotVersion("Teleport v18.11.7 git:v18.11.7-0-ga2db690b go1.26.10\n")
	if err != nil || got != "18.11.7" {
		t.Errorf("parseTbotVersion = %q, %v; want 18.11.7", got, err)
	}
	if _, err := parseTbotVersion("command not found"); err == nil {
		t.Error("unrecognised output should be an error, not a version")
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"18.10.0", "18.11.7", true},
		{"18.11.7", "18.11.7", false},
		{"18.11.7", "17.0.0-aa", false}, // the cluster's min_client_version spelling
		{"17.0.0", "17.0.0-aa", false},  // a suffix must not make the release "older"
		{"16.4.2", "17.0.0-aa", true},
		{"9.3.0", "10.0.0", true}, // numeric, not lexical
	} {
		if got := versionLess(c.a, c.b); got != c.want {
			t.Errorf("versionLess(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestJudgeTbotVersion(t *testing.T) {
	const bin = "/usr/local/bin/tbot"
	for _, c := range []struct {
		name                               string
		installed, cluster, minClient, ran string
		want                               checkStatus
	}{
		{"all in step", "18.11.7", "18.11.7", "17.0.0-aa", "18.11.7", statusOK},
		{"older minor is supported", "18.10.0", "18.11.7", "17.0.0-aa", "", statusOK},
		{"below the cluster's minimum", "16.4.2", "18.11.7", "17.0.0-aa", "", statusFail},
		{"newer major than the cluster", "19.0.1", "18.11.7", "17.0.0-aa", "", statusWarn},
		// The case the July lock log shows: an upgrade that never reached
		// the tbot already running.
		{"running an older tbot than installed", "18.11.7", "18.11.7", "17.0.0-aa", "18.10.0", statusWarn},
		{"cluster unknown", "18.11.7", "", "", "", statusOK},
	} {
		got, detail, fix := judgeTbotVersion(c.installed, bin, c.cluster, c.minClient, c.ran)
		if got != c.want {
			t.Errorf("%s: status = %v (%s), want %v", c.name, got, detail, c.want)
		}
		if got != statusOK && fix == "" {
			t.Errorf("%s: a %v needs a fix line", c.name, got)
		}
	}
}

func TestRunningTbotVersion(t *testing.T) {
	log := filepath.Join(t.TempDir(), "daemon.log")
	// Two starts in one day: the later one is what's running.
	body := "2026/07/20 06:48:59 tbot: 2026-07-20T06:48:59.583-03:00 INFO [TBOT]      Initializing tbot version.teleport:18.9.2 version.teleport_git:v18.9.2 tbot/tbot.go:127\n" +
		"2026/07/20 12:54:40 tbot: 2026-07-20T12:54:40.238-03:00 INFO [TBOT]      Initializing tbot version.teleport:18.10.0 version.teleport_git:v18.10.0-0-gddaa46b8 version.go:go1.25.11 tbot/tbot.go:127\n"
	if err := os.WriteFile(log, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runningTbotVersion(log); got != "18.10.0" {
		t.Errorf("runningTbotVersion = %q, want the latest start, 18.10.0", got)
	}
	if got := runningTbotVersion(filepath.Join(t.TempDir(), "missing.log")); got != "" {
		t.Errorf("no log should mean unknown, got %q", got)
	}
}
