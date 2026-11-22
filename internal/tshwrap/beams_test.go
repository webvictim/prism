package tshwrap

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// Captured from `tsh beams ls --format json` on odd-firefly, 2026-11-20.
const beamsLsOutput = `[
  {
    "id": "starry-satellite",
    "uuid": "1510d6b9-b4fa-4976-aef7-495111c449f9",
    "owner": "webvictim",
    "expires": "2026-11-21T06:17:02.727136408Z",
    "requested_region": "us-east-1",
    "region": "us-west-2",
    "phase": "default:v1"
  }
]`

func TestParseBeams(t *testing.T) {
	beams, err := parseBeams([]byte(beamsLsOutput))
	if err != nil {
		t.Fatal(err)
	}
	if len(beams) != 1 {
		t.Fatalf("got %d beams", len(beams))
	}
	b := beams[0]
	if b.ID != "starry-satellite" || b.UUID != "1510d6b9-b4fa-4976-aef7-495111c449f9" || b.Owner != "webvictim" {
		t.Errorf("parsed %+v", b)
	}
	if b.Expires.IsZero() || b.Expires.Format("15:04") != "06:17" {
		t.Errorf("expires = %v", b.Expires)
	}
}

func TestParseBeamAcceptsObjectOrOneEntryList(t *testing.T) {
	obj := `{"id":"a","uuid":"u-1","expires":"2026-11-21T06:17:02Z"}`
	for _, in := range []string{obj, "[" + obj + "]"} {
		b, err := parseBeam([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if b.UUID != "u-1" || b.ID != "a" {
			t.Errorf("%s: parsed %+v", in, b)
		}
	}
	if _, err := parseBeam([]byte(`{"id":"a"}`)); err == nil {
		t.Error("a beam with no uuid was accepted")
	}
	if _, err := parseBeam([]byte(`[]`)); err == nil {
		t.Error("an empty list was accepted")
	}
}

func TestNodeListed(t *testing.T) {
	// The empty answer, verbatim: a JSON list followed by a hint.
	empty := "[]\n\n  Not seeing nodes? Either no nodes are available or your user's roles do not match.\n"
	if ok, err := nodeListed([]byte(empty), "u"); err != nil || ok {
		t.Errorf("empty list: ok=%v err=%v", ok, err)
	}
	one := `[{"kind":"node","sub_kind":"openssh","metadata":{"name":"fe3ebc46"},"spec":{"hostname":"beam-fe3ebc46"}}]`
	if ok, err := nodeListed([]byte(one), "fe3ebc46"); err != nil || !ok {
		t.Errorf("listed node: ok=%v err=%v", ok, err)
	}
	if ok, _ := nodeListed([]byte(one), "other"); ok {
		t.Error("a search hit on another node counted")
	}
}

func TestBeamsAddRetriesWithoutNoShareOnOldTsh(t *testing.T) {
	old := runAdd
	t.Cleanup(func() { runAdd = old })
	oldLogin := requireLogin
	t.Cleanup(func() { requireLogin = oldLogin })
	requireLogin = func() error { return nil }
	var calls [][]string
	runAdd = func(_ context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if slices.Contains(args, noShareFlag) {
			return nil, errors.New("tsh [beams add]: exit status 1 (stderr: tsh: error: unknown long flag '--no-share-connected-accounts')")
		}
		return []byte(`{"id":"steady-dial","uuid":"u-1","owner":"me"}`), nil
	}
	b, err := BeamsAdd(t.Context(), []string{"beam-user", "mcp-user"})
	if err != nil {
		t.Fatal(err)
	}
	if b.UUID != "u-1" || len(calls) != 2 || slices.Contains(calls[1], noShareFlag) {
		t.Errorf("beam %+v after calls %q", b, calls)
	}
	// The roles survive the retry.
	for _, c := range calls {
		if !slices.Contains(c, "--roles=beam-user,mcp-user") {
			t.Errorf("call %q lost the roles", c)
		}
	}
}
