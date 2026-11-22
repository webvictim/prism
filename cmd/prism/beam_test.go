package main

import (
	"strings"
	"testing"
	"time"

	"github.com/webvictim/prism/internal/beam"
)

func TestWriteBeamPR(t *testing.T) {
	start := time.Date(2026, 11, 21, 4, 32, 0, 0, time.UTC)
	recs := []beam.Record{
		{UUID: "u-1", Alias: "steady-sector", Proxy: "odd-firefly.beams.sh:443",
			Started: start, Ended: start.Add(9 * time.Minute), Requests: 35},
		{UUID: "u-2", Alias: "next-one", Proxy: "odd-firefly.beams.sh:443",
			Started: start.Add(9 * time.Minute), Requests: 4},
	}
	var b strings.Builder
	writeBeamPR(&b, recs, nil, &beam.File{Lanes: map[string]string{"claude:s": "u-2"}})
	out := b.String()
	for _, want := range []string{
		"### AI session recordings",
		"| `steady-sector` | 2026-11-21 04:32–04:41 UTC | 35 | [session](https://odd-firefly.beams.sh/web/cluster/odd-firefly.beams.sh/session/u-1?recordingType=beam) |",
		"| `next-one` (in use) | 2026-11-21 04:41–now UTC | 4 |",
		"summarised about 15 minutes after it ends",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("PR section lacks %q:\n%s", want, out)
		}
	}
	// prism describes the recordings, not the review: that is the author's.
	if strings.Contains(strings.ToLower(out), "reviewed") {
		t.Errorf("PR section claims a review prism can't know about:\n%s", out)
	}
}

func TestPickWorkspace(t *testing.T) {
	recs := []beam.Record{
		{UUID: "a", Workspace: "/repo/a", Requests: 3},
		{UUID: "b", Workspace: "/repo/b", Requests: 3},
		{UUID: "shared", Key: "shared", Requests: 2},
		{UUID: "old-untagged", Requests: 9},
		{UUID: "idle-standby"},
	}
	mine, untagged := pickWorkspace(recs, "/repo/a")
	if len(mine) != 1 || mine[0].UUID != "a" {
		t.Errorf("mine = %+v", mine)
	}
	if len(untagged) != 1 || untagged[0].UUID != "shared" {
		t.Errorf("untagged = %+v, want only the shared beam that carried requests", untagged)
	}
	var b strings.Builder
	writeBeamPR(&b, mine, untagged, &beam.File{})
	if out := b.String(); strings.Contains(out, "/repo/b") || !strings.Contains(out, "couldn't attribute") {
		t.Errorf("PR section:\n%s", out)
	}
}
