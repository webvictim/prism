package main

import (
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/tshwrap"
)

func TestTshVerdict(t *testing.T) {
	for _, tt := range []struct {
		name         string
		v            tshwrap.ClientVersion
		clusterTools string
		old          bool
		detail, fix  string // substrings; fix "" means none
	}{
		{name: "new enough", v: tshwrap.ClientVersion{Ran: "18.11.9", Installed: "18.11.9"}, detail: "v18.11.9"},
		{name: "swapped for a new enough one", v: tshwrap.ClientVersion{Ran: "18.11.7", Installed: "18.11.9"},
			detail: "in place of the installed v18.11.9"},
		// The machine this was written for: a stale pin from an old login.
		{name: "stale pin", v: tshwrap.ClientVersion{Ran: "18.8.1", Installed: "18.11.9"}, clusterTools: "18.11.7",
			old: true, detail: "in place of the installed v18.11.9", fix: "records the cluster's current v18.11.7"},
		{name: "cluster pins an old one", v: tshwrap.ClientVersion{Ran: "18.8.1", Installed: "18.11.9"}, clusterTools: "18.8.1",
			old: true, fix: "the cluster pins tsh to v18.8.1"},
		{name: "cluster unknown", v: tshwrap.ClientVersion{Ran: "18.8.1", Installed: "18.11.9"},
			old: true, fix: "TELEPORT_TOOLS_VERSION=off"},
		{name: "installed one is old", v: tshwrap.ClientVersion{Ran: "18.8.1", Installed: "18.8.1"}, clusterTools: "18.11.7",
			old: true, fix: "upgrade Teleport's client tools"},
		{name: "installed version unknown", v: tshwrap.ClientVersion{Ran: "18.8.1"},
			old: true, fix: "upgrade Teleport's client tools"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old, detail, fix := tshVerdict(tt.v, tt.clusterTools, "proxy.example:443")
			if old != tt.old {
				t.Errorf("old = %v, want %v", old, tt.old)
			}
			if !strings.Contains(detail, tt.detail) {
				t.Errorf("detail %q lacks %q", detail, tt.detail)
			}
			if (tt.fix == "") != (fix == "") || !strings.Contains(fix, tt.fix) {
				t.Errorf("fix %q, want one containing %q", fix, tt.fix)
			}
			if old && !strings.Contains(detail, minTshForModels) {
				t.Errorf("an old tsh's detail should name %s: %q", minTshForModels, detail)
			}
		})
	}
}
