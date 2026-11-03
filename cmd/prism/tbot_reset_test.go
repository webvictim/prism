package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/machineid"
	"github.com/webvictim/prism/internal/tbot"
)

// The safety property of `prism tbot reset`: the resources it deletes
// are derived from the local hostname, never from whatever the sidecar
// happens to name. A directory copied from another machine must not
// turn a reset into "delete the other machine's bot".
func TestResetPlanTouchesOnlyThisHost(t *testing.T) {
	want := tbot.NamesForHost("gaia")
	foreign := &tbot.SidecarConfig{
		TokenName: "prism-bot-token-athena",
		RoleName:  "prism-bot-role-athena",
		BotName:   "prism-bot-athena",
		MachineID: "MACHINE-A",
	}

	plan := resetPlan("/home/gus/.config/prism/tbot", "cluster.example.com:443", proxyFromConfig, want, foreign, nil)

	for _, mustSay := range []string{
		"bot/prism-bot-gaia",
		"role/prism-bot-role-gaia",
		"token/prism-bot-token-gaia",
		"cluster.example.com:443",
		"/home/gus/.config/prism/tbot",
		// The other machine's bot is named, but as something left alone.
		"currently names bot/prism-bot-athena",
		"will be left alone",
	} {
		if !strings.Contains(plan, mustSay) {
			t.Errorf("plan does not mention %q:\n%s", mustSay, plan)
		}
	}

	// Nothing in the delete list may name the other machine.
	for _, line := range strings.Split(plan, "\n") {
		if strings.Contains(line, "delete and recreate") && strings.Contains(line, "athena") {
			t.Errorf("plan would delete another machine's resource: %s", line)
		}
	}
}

func TestResetPlanWithoutAnExistingSidecar(t *testing.T) {
	plan := resetPlan("/tmp/tbot", "c:443", proxyFromConfig, tbot.NamesForHost("gaia"), nil, nil)
	if strings.Contains(plan, "left alone") {
		t.Errorf("plan mentions a previous bot when there isn't one:\n%s", plan)
	}
	if !strings.Contains(plan, "scoped to this hostname") {
		t.Errorf("plan drops the scoping reassurance:\n%s", plan)
	}
}

// A reset deletes cluster resources, so a run with nobody there to
// answer must stop rather than assume consent.
func TestConfirmReset(t *testing.T) {
	err := confirmReset(strings.NewReader(""), io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("non-interactive confirm = %v, want it to point at --yes", err)
	}

	if err := confirmReset(strings.NewReader("reset\n"), io.Discard, true); err != nil {
		t.Errorf("typing reset = %v, want nil", err)
	}
	for _, answer := range []string{"\n", "y\n", "yes\n", "RESET\n", ""} {
		if err := confirmReset(strings.NewReader(answer), io.Discard, true); err == nil {
			t.Errorf("answer %q was accepted, want only an exact `reset`", answer)
		}
	}
}

// clearLocalTbotState removes what prism wrote and nothing else: the
// directory is user-chosen, so a stray RemoveAll there would be
// unforgivable.
func TestClearLocalTbotState(t *testing.T) {
	dir := t.TempDir()
	mine := []string{tbot.SidecarFilename, "tbot.yaml", "role.yaml", "bot.yaml", "token.yaml"}
	for _, name := range mine {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "storage", "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "storage", "bkp_state"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	const stranger = "notes-i-left-here.txt"
	if err := os.WriteFile(filepath.Join(dir, stranger), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := clearLocalTbotState(dir)
	if err != nil {
		t.Fatalf("clearLocalTbotState: %v", err)
	}
	if len(removed) != len(mine)+1 { // +1 for storage
		t.Errorf("removed %v, want every file prism writes", removed)
	}
	for _, name := range append(mine, filepath.Join("storage", "bkp_state")) {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s survived the reset", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, stranger)); err != nil {
		t.Errorf("%s was removed, but prism did not write it", stranger)
	}
	// tbot needs the storage directory to exist before it joins.
	if fi, err := os.Stat(tbot.StorageDirPath(dir)); err != nil || !fi.IsDir() {
		t.Errorf("storage/ was not recreated: %v", err)
	}

	// Running it again is not an error: the only thing left to clear is
	// the empty storage directory the first run recreated.
	again, err := clearLocalTbotState(dir)
	if err != nil {
		t.Fatalf("second clearLocalTbotState: %v", err)
	}
	if len(again) != 1 || again[0] != "storage" {
		t.Errorf("second run removed %v, want just the empty storage dir", again)
	}

	// And on a directory prism has never written to, there is nothing
	// to say.
	empty, err := clearLocalTbotState(t.TempDir())
	if err != nil {
		t.Fatalf("clearLocalTbotState on an empty dir: %v", err)
	}
	if len(empty) != 1 || empty[0] != "nothing to remove" {
		t.Errorf("empty dir removed %v, want nothing", empty)
	}
}

// The one case where a reset would hurt somebody else: an identity
// copied from another machine, onto a machine answering to the same
// hostname. The names would collide, so resetting would take over the
// other machine's bot.
func TestResetPreflightRefusesSameHostnameClone(t *testing.T) {
	current, err := machineid.Current()
	if err != nil {
		t.Skipf("no machine id on this platform: %v", err)
	}
	want := tbot.NamesForHost("gaia")
	foreign := &tbot.SidecarConfig{
		TokenName:          want.Token,
		RoleName:           want.Role,
		BotName:            want.Bot,
		MachineID:          current + "-SOMEONE-ELSE",
		RegistrationSecret: "s",
	}

	problems := resetPreflight("cluster.example.com:443", want, foreign)
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "came from another machine") {
		t.Errorf("preflight allowed a same-hostname clone to reset:\n%s", joined)
	}
	if !strings.Contains(joined, "Rename this machine") {
		t.Errorf("preflight does not say how to get unstuck:\n%s", joined)
	}

	// The same directory on the machine it was issued to is fine, so
	// the only problems left are about the environment (tctl, login),
	// never about the machine binding.
	own := *foreign
	own.MachineID = current
	problems = resetPreflight("cluster.example.com:443", want, &own)
	if joined := strings.Join(problems, "\n"); strings.Contains(joined, "another machine") {
		t.Errorf("preflight blocked a reset on the machine that owns the identity:\n%s", joined)
	}
}
