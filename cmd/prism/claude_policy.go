package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Claude Code refuses to start at all when its managed settings pin
// Anthropic sign-ins and the session carries a credential of its own:
//
//	This machine's managed settings require a first-party login: they pin
//	Anthropic sign-ins to your organization (forceLoginOrgUUID), and this
//	session is configured with a bearer token from ANTHROPIC_AUTH_TOKEN,
//	which cannot be checked against that policy (requests go through
//	127.0.0.1:7331, per ANTHROPIC_BASE_URL).
//
// prism's dummy token is precisely that credential, and dropping it
// doesn't help either: with nothing configured Claude Code reports "no
// usable Anthropic sign-in or credential" under the same policy and
// demands `claude auth login`. The pin polices *first-party* sign-ins,
// so the way out is not to be a first-party client at all — see
// claude_auth.go, which switches Claude Code onto a third-party platform
// pointed at prism's router, where no Anthropic sign-in is involved.
//
// A pin only counts when an administrator set it. Claude Code reads, in
// descending priority: settings its server delivers, the MDM policy (a
// managed plist on macOS, the HKLM registry key on Windows), and a
// managed-settings.json file. prism reads the local files, which is
// enough for the case that matters: server-delivered settings arrive
// over an authenticated connection, so a machine with nobody signed in
// cannot have one yet. `prism config set claude_auth platform` covers
// whatever is left (a policyHelper, or Windows' registry source).

// claudeManagedPrefs is the macOS MDM preference domain Claude Code
// reads its policy from.
const claudeManagedPrefs = "com.anthropic.claudecode"

// claudeManagedSource is one place Claude Code looks for
// administrator-controlled settings.
type claudeManagedSource struct {
	path string
	// plist marks an Apple property list, which has to be converted
	// before it can be parsed as JSON — the managed copy is binary.
	plist bool
}

// claudeManagedSources lists the local managed-settings files for this
// platform, highest priority first. The order is Claude Code's own;
// prism only asks whether *any* of them pins sign-ins, so it matters
// only for which file is read first.
func claudeManagedSources() []claudeManagedSource {
	switch runtime.GOOS {
	case "darwin":
		const prefsDir = "/Library/Managed Preferences"
		var sources []claudeManagedSource
		// A per-user profile shadows the device-level one.
		if u, err := user.Current(); err == nil && u.Username != "" {
			sources = append(sources, claudeManagedSource{
				path:  filepath.Join(prefsDir, u.Username, claudeManagedPrefs+".plist"),
				plist: true,
			})
		}
		return append(sources,
			claudeManagedSource{path: filepath.Join(prefsDir, claudeManagedPrefs+".plist"), plist: true},
			claudeManagedSource{path: "/Library/Application Support/ClaudeCode/managed-settings.json"},
		)
	case "windows":
		return []claudeManagedSource{
			{path: `C:\Program Files\ClaudeCode\managed-settings.json`},
		}
	default:
		return []claudeManagedSource{
			{path: "/etc/claude-code/managed-settings.json"},
		}
	}
}

// claudeManagedSettings is the part of a managed-settings source prism
// reads. Both keys are sign-in pins, and either one makes a first-party
// session unusable without the right login.
type claudeManagedSettings struct {
	// ForceLoginOrgUUID names the organization every sign-in must belong
	// to. It is a string in the profiles seen so far, but Claude Code
	// also accepts a list, so it stays raw.
	ForceLoginOrgUUID json.RawMessage `json:"forceLoginOrgUUID"`
	// ForceLoginMethod is `claudeai`, `console` or `gateway`.
	ForceLoginMethod string `json:"forceLoginMethod"`
}

// pinsLogin reports whether these settings police which account signs
// in.
//
// Any value for either key counts, without looking at what it says: the
// question prism is asking is whether a first-party session could start
// unattended here, and every form of pin says no. Reading the value
// would only invite guesses about shapes (an empty list, an unknown
// method) that prism can't satisfy anyway.
func (s claudeManagedSettings) pinsLogin() bool {
	if s.ForceLoginMethod != "" {
		return true
	}
	switch strings.TrimSpace(string(s.ForceLoginOrgUUID)) {
	case "", "null", `""`:
		return false
	}
	return true
}

// claudeLoginPinned reports whether a managed-settings source on this
// machine pins Anthropic sign-ins.
func claudeLoginPinned() bool {
	return claudeLoginPinnedIn(claudeManagedSources())
}

// claudeLoginPinnedIn is claudeLoginPinned over a given source list.
//
// An unreadable or absent source is not a pin, and doesn't stop the
// search: prism's behaviour must not hinge on being able to read every
// root-owned file an administrator might have left behind.
func claudeLoginPinnedIn(sources []claudeManagedSource) bool {
	for _, src := range sources {
		settings, err := readClaudeManagedSettings(src)
		if err != nil {
			continue
		}
		if settings.pinsLogin() {
			return true
		}
	}
	return false
}

// readClaudeManagedSettings parses one managed-settings source.
func readClaudeManagedSettings(src claudeManagedSource) (claudeManagedSettings, error) {
	raw, err := readClaudeManagedFile(src)
	if err != nil {
		return claudeManagedSettings{}, err
	}
	var settings claudeManagedSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		return claudeManagedSettings{}, err
	}
	return settings, nil
}

// readClaudeManagedFile returns a source's contents as JSON, converting
// a property list on the way.
func readClaudeManagedFile(src claudeManagedSource) ([]byte, error) {
	if !src.plist {
		return os.ReadFile(src.path)
	}
	// Don't spawn anything for a profile that isn't installed, which is
	// the common case.
	if _, err := os.Stat(src.path); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// plutil is how Claude Code itself reads the managed plist, and the
	// only converter guaranteed to be present.
	cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "--", src.path)
	return cmd.Output()
}
