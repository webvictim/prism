package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

func TestClaudeManagedSettingsPinsLogin(t *testing.T) {
	for _, tt := range []struct {
		name string
		raw  string
		want bool
	}{
		{
			// The profile on the machine that reported this: an MDM
			// payload pinning sign-ins to one organization.
			name: "an organization pin",
			raw: `{"$schema":"https://json.schemastore.org/claude-code-settings.json",
			       "companyAnnouncements":["Managed settings are currently enforced."],
			       "forceLoginOrgUUID":"27824741-1061-41b3-b6d0-b9a7fb2cf84d",
			       "permissions":{"disableAutoMode":"disable"}}`,
			want: true,
		},
		{
			name: "a method pin",
			raw:  `{"forceLoginMethod":"claudeai"}`,
			want: true,
		},
		{
			name: "a gateway pin",
			raw:  `{"forceLoginMethod":"gateway"}`,
			want: true,
		},
		{
			// Claude Code accepts a list of organizations as well as one
			// id, and prism must not read only the string shape.
			name: "a list of organizations",
			raw:  `{"forceLoginOrgUUID":["27824741-1061-41b3-b6d0-b9a7fb2cf84d"]}`,
			want: true,
		},
		{
			// A pin nothing can satisfy is still a pin — prism's token
			// is rejected under it just the same.
			name: "an empty list of organizations",
			raw:  `{"forceLoginOrgUUID":[]}`,
			want: true,
		},
		{
			// Managed settings exist for plenty of other reasons; none
			// of them should cost the user prism's credential.
			name: "managed settings with no sign-in pin",
			raw:  `{"permissions":{"disableBypassPermissionsMode":"disable"},"env":{"FOO":"bar"}}`,
			want: false,
		},
		{
			name: "nothing configured",
			raw:  `{}`,
			want: false,
		},
		{
			name: "keys present but empty",
			raw:  `{"forceLoginOrgUUID":null,"forceLoginMethod":""}`,
			want: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var settings claudeManagedSettings
			if err := json.Unmarshal([]byte(tt.raw), &settings); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := settings.pinsLogin(); got != tt.want {
				t.Errorf("pinsLogin() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadClaudeManagedSettingsJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed-settings.json")
	if err := os.WriteFile(path, []byte(`{"forceLoginOrgUUID":"an-org"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	settings, err := readClaudeManagedSettings(claudeManagedSource{path: path})
	if err != nil {
		t.Fatalf("readClaudeManagedSettings() failed: %v", err)
	}
	if !settings.pinsLogin() {
		t.Errorf("pin not read from %q", path)
	}

	// A machine with no managed settings is the common case and must
	// read as "no pin", not as an error prism acts on.
	if _, err := readClaudeManagedSettings(claudeManagedSource{path: path + ".absent"}); err == nil {
		t.Error("a missing source read without error")
	}
}

// The managed profile Claude Code reads on macOS is a *binary* plist, so
// prism converts it with plutil rather than parsing it directly. That
// conversion is the part worth testing — a profile prism can't read
// looks exactly like a machine with no policy.
func TestReadClaudeManagedPlist(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("plutil is macOS-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, claudeManagedPrefs+".plist")
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>companyAnnouncements</key>
	<array><string>Managed settings are currently enforced.</string></array>
	<key>forceLoginOrgUUID</key>
	<string>27824741-1061-41b3-b6d0-b9a7fb2cf84d</string>
	<key>permissions</key>
	<dict><key>disableAutoMode</key><string>disable</string></dict>
</dict>
</plist>
`
	if err := os.WriteFile(path, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("/usr/bin/plutil", "-convert", "binary1", "--", path).CombinedOutput(); err != nil {
		t.Fatalf("plutil -convert binary1: %v: %s", err, out)
	}

	settings, err := readClaudeManagedSettings(claudeManagedSource{path: path, plist: true})
	if err != nil {
		t.Fatalf("readClaudeManagedSettings() failed: %v", err)
	}
	if !settings.pinsLogin() {
		t.Errorf("pin not read from the converted profile: %+v", settings)
	}
}

// The device-level profile is where an MDM lands the pin, so it has to
// be on the list and marked for conversion.
func TestClaudeManagedSourcesDeviceProfile(t *testing.T) {
	sources := claudeManagedSources()
	if len(sources) == 0 {
		t.Fatal("no managed-settings sources for this platform")
	}
	if runtime.GOOS != "darwin" {
		return
	}
	want := "/Library/Managed Preferences/" + claudeManagedPrefs + ".plist"
	i := slices.IndexFunc(sources, func(s claudeManagedSource) bool { return s.path == want })
	if i < 0 {
		t.Fatalf("%q is not among %+v", want, sources)
	}
	if !sources[i].plist {
		t.Errorf("%q is not marked as a plist", want)
	}
}

func TestClaudeLoginPinnedIn(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) claudeManagedSource {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return claudeManagedSource{path: path}
	}

	absent := claudeManagedSource{path: filepath.Join(dir, "not-installed.json")}
	unreadable := write("garbage.json", "not json at all")
	plain := write("plain.json", `{"permissions":{"disableAutoMode":"disable"}}`)
	pinned := write("pinned.json", `{"forceLoginOrgUUID":"an-org"}`)

	for _, tt := range []struct {
		name    string
		sources []claudeManagedSource
		want    bool
	}{
		{
			name: "a machine with no managed settings",
		},
		{
			// The common managed machine: policy, but nothing about
			// sign-ins. prism's token is what makes it work without an
			// Anthropic account, so it must survive this.
			name:    "managed but unpinned",
			sources: []claudeManagedSource{absent, plain},
		},
		{
			// A profile prism can't parse must not look like a pin, or a
			// stray file would cost every user their credential.
			name:    "an unparseable source",
			sources: []claudeManagedSource{unreadable},
		},
		{
			// …and must not stop the search either.
			name:    "a pin below an unparseable source",
			sources: []claudeManagedSource{absent, unreadable, pinned},
			want:    true,
		},
		{
			name:    "a pin in the first source",
			sources: []claudeManagedSource{pinned, plain},
			want:    true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := claudeLoginPinnedIn(tt.sources); got != tt.want {
				t.Errorf("claudeLoginPinnedIn() = %v, want %v", got, tt.want)
			}
		})
	}
}
