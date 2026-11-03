package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/tbot"
)

// clusterLock is the part of a Teleport lock resource that `prism tbot
// reset` reads.
type clusterLock struct {
	Name    string
	Target  map[string]string // non-empty LockTarget fields, by JSON name
	Message string
	Expires time.Time // zero when the lock never expires
}

// parseLocks reads `tctl get locks --format=json`.
func parseLocks(data []byte) ([]clusterLock, error) {
	if strings.TrimSpace(string(data)) == "" {
		return nil, nil
	}
	var raw []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Target  map[string]any `json:"target"`
			Message string         `json:"message"`
			Expires *time.Time     `json:"expires"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse tctl's lock list: %w", err)
	}
	locks := make([]clusterLock, 0, len(raw))
	for _, r := range raw {
		l := clusterLock{Name: r.Metadata.Name, Message: r.Spec.Message, Target: map[string]string{}}
		for k, v := range r.Spec.Target {
			if s, ok := v.(string); ok && s != "" {
				l.Target[k] = s
			}
		}
		if r.Spec.Expires != nil {
			l.Expires = *r.Spec.Expires
		}
		locks = append(locks, l)
	}
	return locks, nil
}

// blockingLocks picks out the locks that would stop a rebuilt bot from
// joining, split into the ones a reset may remove and the ones it must
// not.
//
// A reset recreates the bot, role and token under the same names, and a
// lock targets a name, not a resource. So a lock on any of them outlives
// the reset and refuses the new bot exactly as it refused the old one.
//
// Only one kind is prism's to clear: the lock Teleport places by itself
// on a bound-keypair bot's join token when it sees a certificate
// generation mismatch (lib/auth/bot.go, tryLockBotDueToGenerationMismatch).
// It has no expiry, and it means "this identity was renewed twice" — the
// very state a reset exists to rebuild from. Recognised by its exact
// target and wording, because a lock someone placed on purpose may target
// the same token and must not be quietly undone.
func blockingLocks(locks []clusterLock, want tbot.Names, now time.Time) (generation, other []clusterLock) {
	for _, l := range locks {
		if !l.Expires.IsZero() && !l.Expires.After(now) {
			continue // no longer in force
		}
		if !targetsIdentity(l, want) {
			continue
		}
		if isGenerationLock(l, want.Token) {
			generation = append(generation, l)
		} else {
			other = append(other, l)
		}
	}
	byName := func(ls []clusterLock) {
		sort.Slice(ls, func(i, j int) bool { return ls[i].Name < ls[j].Name })
	}
	byName(generation)
	byName(other)
	return generation, other
}

// targetsIdentity reports whether l applies to the bot a reset rebuilds:
// its join token, its user (Teleport names a bot's user bot-<name>) or
// its role.
func targetsIdentity(l clusterLock, want tbot.Names) bool {
	return l.Target["join_token"] == want.Token ||
		l.Target["user"] == "bot-"+want.Bot ||
		l.Target["role"] == want.Role
}

// isGenerationLock reports whether l is Teleport's own lock for a
// generation mismatch on token: it targets the join token and nothing
// else, and carries the message Teleport writes for it.
func isGenerationLock(l clusterLock, token string) bool {
	prefix := "Bot joins via the token " + strconv.Quote(token) +
		" have been locked due to a certificate generation mismatch"
	return len(l.Target) == 1 && l.Target["join_token"] == token &&
		strings.HasPrefix(l.Message, prefix)
}

// listLocks reads every lock in the cluster through tctl.
func listLocks(proxy string) ([]clusterLock, error) {
	out, err := captureTctl(proxy, "get", "locks", "--format=json")
	if err != nil {
		return nil, err
	}
	return parseLocks(out)
}

// describeLock is a one-line summary of a lock for the plan and errors.
func describeLock(l clusterLock) string {
	var targets []string
	for k, v := range l.Target {
		targets = append(targets, k+"="+v)
	}
	sort.Strings(targets)
	s := "lock/" + l.Name + " (" + strings.Join(targets, ", ") + ")"
	if l.Message != "" {
		s += ": " + oneLine(l.Message)
	}
	return s
}
