package quota

import (
	"strings"
	"testing"
	"time"
)

func TestStatuslineLabelNoteThenMark(t *testing.T) {
	now := time.Now()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	b, err := Statusline([]byte(`{"version":1}`), "fresh-beans.beams.sh:443", "", now.Add(-time.Minute),
		[]StatuslineEntry{{App: "anthropic", Window: Window{Interval: "daily", StartsAt: &start, EndsAt: &end}, Approximate: true, Note: "tsh login expired"}}, now)
	if err != nil || !strings.Contains(string(b), `"label":"Claude (tsh login expired) ≈"`) {
		t.Fatalf("%s %v", b, err)
	}
}

func TestStatuslineAccountCarriesTheBeam(t *testing.T) {
	b, err := Statusline([]byte(`{"version":1,"key":"k","session_id":"s1"}`), "odd-firefly.beams.sh:443", "beam: quiet-otter", time.Time{}, nil, time.Now())
	if err != nil || !strings.Contains(string(b), `"label":"beam: quiet-otter"`) {
		t.Fatalf("%s %v", b, err)
	}
	if got := StatuslineSessionID([]byte(`{"version":1,"session_id":"s1"}`)); got != "s1" {
		t.Errorf("session id = %q", got)
	}
}
