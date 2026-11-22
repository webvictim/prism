package tshwrap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Beam is one entry of `tsh beams ls --format=json`, which is also the
// shape `tsh beams add --format=json` prints.
type Beam struct {
	// ID is the beam's alias, e.g. "starry-satellite".
	ID string `json:"id"`
	// UUID is the beam's identifier, and the SSH node name its host
	// certificate is valid for: `beams@<uuid>` connects, while the
	// `beam-<uuid>` hostname fails the host check.
	UUID    string    `json:"uuid"`
	Owner   string    `json:"owner"`
	Expires time.Time `json:"expires"`
	Region  string    `json:"region"`
	Phase   string    `json:"phase"`
}

// ErrNoLogin is returned by the beam commands when there is no usable tsh
// login. They need one — beams belong to a person — and asking tsh anyway
// opens a browser login window, which a daemon must never do.
var ErrNoLogin = errors.New("no usable tsh login")

// requireLogin refuses up front when tsh has no live login, rather than
// letting tsh start an interactive SSO flow. A var so tests can skip it.
var requireLogin = func() error {
	st, err := StatusJSON()
	if err != nil {
		return err
	}
	if !st.LoggedIn() || st.IsExpired() {
		return ErrNoLogin
	}
	return nil
}

// LoginTTLMinutes is the login prism asks for: 30 hours, the longest a
// stock Beams role allows. Teleport shortens a request longer than the
// user's roles allow to their limit instead of refusing it
// (RoleSet.AdjustSessionTTL), so asking for more is safe on any cluster;
// tsh's own default is 12 hours.
const LoginTTLMinutes = 30 * 60

// LoginUnattended runs `tsh login` for the daemon, with no terminal. It
// returns when the login completes or fails, or ctx ends.
//
// With openBrowser false (the default flow) tsh is told not to open one
// (--browser=none) and prints the link to its local callback instead,
// which is handed to onLink as soon as it appears: prism puts it in the
// message an agent shows, so the user can finish the login from wherever
// they are — a VM's forwarded port included. With openBrowser true
// (beam_auto_login) the browser opens, which completes the login without
// a prompt where the identity provider signs the user in by itself.
func LoginUnattended(ctx context.Context, openBrowser bool, onLink func(string)) error {
	bin, err := tshBin()
	if err != nil {
		return fmt.Errorf("tsh not found: %w", err)
	}
	args := []string{"login", fmt.Sprintf("--ttl=%d", LoginTTLMinutes)}
	if !openBrowser {
		args = append(args, "--browser=none")
	}
	cmd := exec.CommandContext(ctx, bin, proxyArgs(args)...)
	HideWindow(cmd)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	var tail bytes.Buffer
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		found := false
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		sc.Split(scanLinesOrCR)
		for sc.Scan() {
			line := sc.Text()
			if !found {
				if link := loginLink(line); link != "" {
					found = true
					if onLink != nil {
						onLink(link)
					}
					continue
				}
			}
			if strings.TrimSpace(line) != "" && !strings.Contains(line, "Update progress") {
				tail.WriteString(line + "\n")
			}
		}
	}()
	err = cmd.Run()
	pw.Close()
	<-scanned
	if err != nil {
		return fmt.Errorf("tsh login: %w (%s)", err, strings.TrimSpace(lastLines(tail.String(), 3)))
	}
	return nil
}

// loginLinkRE matches the local callback tsh prints for an SSO login.
var loginLinkRE = regexp.MustCompile(`https?://(127\.0\.0\.1|localhost|\[::1\]):\d+/\S+`)

func loginLink(line string) string { return loginLinkRE.FindString(line) }

// scanLinesOrCR splits on \n or \r: tsh's update progress bar redraws
// with carriage returns and would otherwise be one enormous line.
func scanLinesOrCR(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " / ")
}

// BeamsList returns the user's beams.
func BeamsList(ctx context.Context) ([]Beam, error) {
	if err := requireLogin(); err != nil {
		return nil, err
	}
	out, err := runCtx(ctx, "beams", "ls", "--format=json")
	if err != nil {
		return nil, err
	}
	return parseBeams(out)
}

func parseBeams(out []byte) ([]Beam, error) {
	var beams []Beam
	if err := json.Unmarshal(out, &beams); err != nil {
		return nil, fmt.Errorf("parse tsh beams ls: %w", err)
	}
	return beams, nil
}

// BeamsAdd creates a beam and returns it. It never opens a console, and
// shares no connected accounts: prism only relays inference through the
// beam, so it has no use for the user's GitHub identity in there.
//
// roles, when given, are the only roles delegated to the beam (`tsh beams
// add --roles`); otherwise it gets all of its owner's standing roles,
// tsh's default. The user must hold every role named.
func BeamsAdd(ctx context.Context, roles []string) (*Beam, error) {
	if err := requireLogin(); err != nil {
		return nil, err
	}
	args := []string{"beams", "add", "--format=json", "--no-console"}
	if len(roles) > 0 {
		args = append(args, "--roles="+strings.Join(roles, ","))
	}
	// Last, so the retry below can drop it.
	args = append(args, noShareFlag)
	out, err := runAdd(ctx, args...)
	if err != nil && rejectsFlag(err, noShareFlag) {
		// A tsh that predates the flag (18.11.7 does) has no connected
		// accounts to share, so leaving it off changes nothing.
		out, err = runAdd(ctx, args[:len(args)-1]...)
	}
	if err != nil {
		return nil, err
	}
	return parseBeam(out)
}

const noShareFlag = "--no-share-connected-accounts"

// runAdd is swapped out by tests.
var runAdd = runCtx

// rejectsFlag reports whether tsh refused to run because it doesn't know
// flag — kingpin's "unknown long flag '--x'".
func rejectsFlag(err error, flag string) bool {
	return strings.Contains(err.Error(), "unknown long flag '"+flag+"'")
}

// parseBeam reads add's output, which is one beam — but accept a one-entry
// list as well, since add and ls print the same entry shape.
func parseBeam(out []byte) (*Beam, error) {
	trimmed := strings.TrimSpace(string(out))
	if strings.HasPrefix(trimmed, "[") {
		beams, err := parseBeams(out)
		if err != nil {
			return nil, err
		}
		if len(beams) != 1 {
			return nil, fmt.Errorf("tsh beams add printed %d beams, want 1", len(beams))
		}
		return &beams[0], nil
	}
	var b Beam
	if err := json.Unmarshal(out, &b); err != nil {
		return nil, fmt.Errorf("parse tsh beams add: %w", err)
	}
	if b.UUID == "" {
		return nil, fmt.Errorf("tsh beams add printed no uuid: %s", trimmed)
	}
	return &b, nil
}

// BeamsRemove deletes a beam by alias or UUID.
func BeamsRemove(ctx context.Context, id string) error {
	if err := requireLogin(); err != nil {
		return err
	}
	_, err := runCtx(ctx, "beams", "rm", id)
	return err
}

// BeamNodeVisible reports whether the beam's SSH node is registered yet.
// A beam that `tsh beams add` has just returned isn't always reachable:
// `tsh ssh` to it fails with "unable to locate node matching UUID-like
// target" for a few seconds, so callers wait on this first.
func BeamNodeVisible(ctx context.Context, uuid string) (bool, error) {
	if err := requireLogin(); err != nil {
		return false, err
	}
	// --search rather than --query: a name predicate doesn't match beam
	// nodes, while a keyword search on the UUID does.
	out, err := runCtx(ctx, "ls", "--format=json", "--search="+uuid)
	if err != nil {
		return false, err
	}
	return nodeListed(out, uuid)
}

// nodeListed reads `tsh ls --format=json` output, which is followed by
// a "Not seeing nodes?" hint when the list is empty, so only the first
// JSON value is decoded.
func nodeListed(out []byte, uuid string) (bool, error) {
	var nodes []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(strings.NewReader(string(out))).Decode(&nodes); err != nil {
		return false, fmt.Errorf("parse tsh ls: %w", err)
	}
	for _, n := range nodes {
		if n.Metadata.Name == uuid {
			return true, nil
		}
	}
	return false, nil
}
