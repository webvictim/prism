package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/tbot"
)

// teleportGenerationMessage is Teleport's own wording, built the way
// lib/auth/bot.go tryLockBotDueToGenerationMismatch builds it. The match
// must hold against exactly this, or a reset leaves the lock in force.
func teleportGenerationMessage(token, bot, instance string) string {
	return fmt.Sprintf(
		"Bot joins via the token %q have been locked due to a "+
			"certificate generation mismatch by %s/%s, possibly "+
			"indicating a stolen certificate.",
		token, bot, instance)
}

// lockJSON renders one lock as `tctl get locks --format=json` would.
func lockJSON(name, target, message, expires string) string {
	exp := ""
	if expires != "" {
		exp = `,"expires":"` + expires + `"`
	}
	return `{"kind":"lock","version":"v2","metadata":{"name":"` + name + `"},` +
		`"spec":{"target":{` + target + `},"message":` + fmt.Sprintf("%q", message) + exp +
		`,"created_at":"2026-11-03T13:20:00Z"}}`
}

func TestBlockingLocks(t *testing.T) {
	want := tbot.NamesForHost("laptop")
	other := tbot.NamesForHost("someone-else")
	now := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)

	locks := []string{
		// Teleport's lock on this machine's token: the one to clear.
		lockJSON("gen", `"join_token":"`+want.Token+`"`,
			teleportGenerationMessage(want.Token, want.Bot, "6b7ab6c7-acc6-4bcb-bdbb-fb0e408e8b2c"), ""),
		// The same lock on another machine's token: not this reset's business.
		lockJSON("gen-other", `"join_token":"`+other.Token+`"`,
			teleportGenerationMessage(other.Token, other.Bot, "x"), ""),
		// Someone locked this machine's token by hand.
		lockJSON("manual-token", `"join_token":"`+want.Token+`"`, "laptop reported stolen", ""),
		// ...or its bot user, or its role.
		lockJSON("manual-user", `"user":"bot-`+want.Bot+`"`, "", ""),
		lockJSON("manual-role", `"role":"`+want.Role+`"`, "", ""),
		// Teleport's wording, but aimed wider than the token: a person
		// wrote this, not the generation check.
		lockJSON("lookalike", `"join_token":"`+want.Token+`","user":"bot-`+want.Bot+`"`,
			teleportGenerationMessage(want.Token, want.Bot, "x"), ""),
		// An expired lock is in force for nobody.
		lockJSON("expired", `"join_token":"`+want.Token+`"`, "for a while", "2026-11-03T13:00:00Z"),
		// One that hasn't expired yet still is.
		lockJSON("expiring", `"role":"`+want.Role+`"`, "until tonight", "2026-11-03T23:00:00Z"),
	}
	parsed, err := parseLocks([]byte("[" + strings.Join(locks, ",") + "]"))
	if err != nil {
		t.Fatal(err)
	}

	clear, keep := blockingLocks(parsed, want, now)
	if got := names(clear); got != "gen" {
		t.Errorf("locks to clear = %s, want gen", got)
	}
	if got := names(keep); got != "expiring,lookalike,manual-role,manual-token,manual-user" {
		t.Errorf("locks that block = %s", got)
	}
}

func TestParseLocksEmpty(t *testing.T) {
	// `[]` is what tctl printed live for a cluster with no locks.
	for _, in := range []string{"[]", "", "\n"} {
		locks, err := parseLocks([]byte(in))
		if err != nil || len(locks) != 0 {
			t.Errorf("parseLocks(%q) = %v, %v; want none", in, locks, err)
		}
	}
}

// The plan is what someone reads back to whoever told them to run this,
// so a lock it will remove has to be in it.
func TestResetPlanNamesLocks(t *testing.T) {
	want := tbot.NamesForHost("laptop")
	plan := resetPlan("/tmp/tbot", "c:443", proxyFromConfig, want, nil, []clusterLock{{Name: "gen"}})
	for _, s := range []string{"remove               lock/gen", "Teleport locked token/" + want.Token} {
		if !strings.Contains(plan, s) {
			t.Errorf("plan is missing %q:\n%s", s, plan)
		}
	}
	if strings.Contains(resetPlan("/tmp/tbot", "c:443", proxyFromConfig, want, nil, nil), "lock/") {
		t.Error("plan mentions a lock when there is none")
	}
}

func names(ls []clusterLock) string {
	var n []string
	for _, l := range ls {
		n = append(n, l.Name)
	}
	return strings.Join(n, ",")
}
