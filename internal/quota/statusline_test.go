package quota

import (
	"strings"
	"testing"
	"time"
)

func TestStatuslineLabelMark(t *testing.T) {
	now := time.Now()
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	b, err := Statusline([]byte(`{"version":1}`), "fresh-beans.beams.sh:443", "", now.Add(-time.Minute),
		[]StatuslineEntry{{App: "anthropic", Window: Window{Interval: "daily", StartsAt: &start, EndsAt: &end}, Approximate: true}}, now)
	if err != nil || !strings.Contains(string(b), `"label":"Claude ≈"`) {
		t.Fatalf("%s %v", b, err)
	}
	// An empty account label stays empty: statusline then shows nothing.
	if !strings.Contains(string(b), `"account":{"id":"teleport:fresh-beans.beams.sh:443","label":""}`) {
		t.Errorf("account: %s", b)
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
