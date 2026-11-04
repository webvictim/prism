package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tbot"
	"github.com/webvictim/prism/internal/tshwrap"
)

// The health loop must watch only the tunnels: tbot's overall /readyz
// also goes unhealthy for the identity output, and restarting the
// tunnels for that would be the cure that hurts.
func TestTbotHealthProbeURLs(t *testing.T) {
	got := tbotHealthProbeURLs(7332, 7334)
	want := []string{
		"http://127.0.0.1:7332/readyz/" + tbot.AppTunnelServiceName,
		"http://127.0.0.1:7332/readyz/" + tbot.OpenAITunnelServiceName,
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("probe URLs = %v, want %v", got, want)
	}
	if got := tbotHealthProbeURLs(0, 7334); got != nil {
		t.Errorf("no diag port should mean no probing, got %v", got)
	}
}

// fakeTsh answers `apps ls` as the tbot identity (-i), as the login, or
// not at all, and records which it was asked as. `tsh status` reports a
// live login exactly when loginOK.
func fakeTsh(t *testing.T, dir string, identityOK, loginOK bool) string {
	t.Helper()
	log := filepath.Join(dir, "tsh.log")
	// Each call starts a fresh record, so a subtest sees only its own calls.
	if err := os.WriteFile(log, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ok := func(b bool) string {
		if b {
			return `printf '%s' '[{"metadata":{"name":"openai"},"spec":{"inference":{"format":"openai","models":[{"name":"m"}]}}}]'`
		}
		return "echo refused >&2; exit 1"
	}
	status := `printf '%s' '{"active":null}'`
	if loginOK {
		status = `printf '%s' '{"active":{"cluster":"c","valid_until":"2999-01-01T00:00:00Z"}}'`
	}
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n" +
		"case \"$*\" in\n" +
		"  *\" status \"*) " + status + " ;;\n" +
		"  *\" -i \"*) " + ok(identityOK) + " ;;\n" +
		"  *) " + ok(loginOK) + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(dir, "tsh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return log
}

func TestClusterAppsPrefersTbotIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell fixtures")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	tbotDir := filepath.Join(root, "tbot")
	for _, d := range []string{bin, tbot.IdentityDirPath(tbotDir)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "cfg"))
	prev := tshwrap.Proxy()
	tshwrap.SetProxy("cluster.example.com:443")
	t.Cleanup(func() { tshwrap.SetProxy(prev) })

	if err := config.Save(&config.Config{Identity: state.IdentitySourceTbot, TbotDir: tbotDir}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	identity := tbot.IdentityFilePath(tbotDir)

	calls := func(log string) string { b, _ := os.ReadFile(log); return string(b) }

	t.Run("no identity file yet: the login", func(t *testing.T) {
		log := fakeTsh(t, bin, true, true)
		if _, err := clusterApps(cfg); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(calls(log), "-i") {
			t.Errorf("tried an identity that doesn't exist:\n%s", calls(log))
		}
	})

	if err := os.WriteFile(identity, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("identity first, no login needed", func(t *testing.T) {
		log := fakeTsh(t, bin, true, false)
		apps, err := clusterApps(cfg)
		if err != nil || len(apps) != 1 {
			t.Fatalf("apps = %v, %v", apps, err)
		}
		if got := calls(log); !strings.Contains(got, "--proxy=cluster.example.com:443 -i "+identity+" apps ls") {
			t.Errorf("not asked as the identity, on prism's cluster:\n%s", got)
		}
	})

	t.Run("a stale identity falls back to the login", func(t *testing.T) {
		fakeTsh(t, bin, false, true)
		if _, err := clusterApps(cfg); err != nil {
			t.Errorf("fallback to the login failed: %v", err)
		}
	})

	t.Run("both failing reports both", func(t *testing.T) {
		fakeTsh(t, bin, false, false)
		_, err := clusterApps(cfg)
		if err == nil || !strings.Contains(err.Error(), "tbot identity") || !strings.Contains(err.Error(), "tsh login") {
			t.Errorf("err = %v, want both reasons", err)
		}
	})

	// With no live login, `tsh apps ls` would start an interactive SSO
	// login and wait for a browser. It must never be run that way.
	t.Run("no live login: apps ls is never run as the login", func(t *testing.T) {
		log := fakeTsh(t, bin, false, false)
		if _, err := clusterApps(cfg); err == nil || !strings.Contains(err.Error(), "no live one") {
			t.Errorf("err = %v, want it to say there's no live login", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(calls(log)), "\n") {
			if strings.Contains(line, "apps ls") && !strings.Contains(line, " -i ") {
				t.Errorf("ran `tsh apps ls` without a login: %q", line)
			}
		}
	})

	t.Run("not on tbot: the login only", func(t *testing.T) {
		log := fakeTsh(t, bin, true, true)
		if _, err := clusterApps(&config.Config{Identity: "tsh", TbotDir: tbotDir}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(calls(log), "-i") {
			t.Errorf("used a tbot identity on a tsh machine:\n%s", calls(log))
		}
	})
}
