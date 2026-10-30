package main

import (
	"fmt"
	"strings"
	"time"

	usagepkg "github.com/webvictim/prism/internal/usage"
)

// substitutionLookback is how far back `prism claude` and `prism env`
// look for a swapped model. A day is long enough to catch "I ran this
// yesterday and it was fine" without dragging up a model the cluster
// has since started serving.
const substitutionLookback = 24 * time.Hour

// substitutionNote returns a line for stderr when the gateway has
// recently served a model other than the one asked for, or "" when it
// hasn't. Any failure to read the usage log is silent: this is a
// courtesy warning and must never stand between the user and their
// tool.
func substitutionNote() string {
	dir, err := usagepkg.Dir()
	if err != nil {
		return ""
	}
	records, err := usagepkg.Load(dir, time.Now().Add(-substitutionLookback))
	if err != nil {
		return ""
	}
	return recentSubstitutionNote(records)
}

// recentSubstitutionNote formats the warning for already-loaded records.
//
// It names at most two pairs. The point is to make someone doubt the
// model their client claims to be using, which one example does; a full
// list is what `prism usage` is for.
func recentSubstitutionNote(records []usagepkg.Record) string {
	subs := usagepkg.Substitutions(records)
	if len(subs) == 0 {
		return ""
	}

	const named = 2
	var pairs []string
	total := 0
	for i, sub := range subs {
		total += sub.Requests
		if i < named {
			pairs = append(pairs, fmt.Sprintf("%s → %s (%d)", sub.Requested, sub.Served, sub.Requests))
		}
	}
	list := strings.Join(pairs, ", ")
	if extra := len(subs) - named; extra > 0 {
		list += fmt.Sprintf(", and %d more pair(s)", extra)
	}

	return fmt.Sprintf("prism: the cluster gateway served a different model than requested on %d request(s) "+
		"in the last 24h: %s. Your client still reports — and costs — the model it asked for; "+
		"run `prism usage` for the full list.", total, list)
}
