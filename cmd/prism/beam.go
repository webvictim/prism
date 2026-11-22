package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/beam"
	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tshwrap"
)

// cmdBeam implements `prism beam start|status|end|link`.
//
// With a beam in use, the daemon sends inference out through it, so the
// agent session is recorded, summarised and classified in Beams exactly
// as if it had run there — while the agent, the code and the tools stay
// on this machine. See internal/beam.
func cmdBeam(args []string) error {
	if len(args) == 0 || wantsHelp(args[:1]) {
		return printCommandHelp("beam")
	}
	switch args[0] {
	case "start":
		return beamStart(args[1:])
	case "status":
		return beamStatus(args[1:])
	case "end", "stop":
		return beamEnd(args[1:])
	case "link":
		return beamLink(args[1:], os.Stdout)
	case "test":
		return beamTest(args[1:])
	default:
		return fmt.Errorf("unknown beam command %q (use start, status, end, link or test)", args[0])
	}
}

// beamControlURL is the daemon's beam endpoint, or an error saying how to
// get a daemon running.
func beamControlURL() (string, error) {
	s, err := state.Load()
	if err != nil {
		return "", err
	}
	if s == nil || s.LocalPort == 0 {
		return "", errors.New("prism isn't running — start it with `prism up`")
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.LocalPort, beam.ControlPath), nil
}

func beamRequest(method, action string, timeout time.Duration) ([]byte, int, error) {
	return beamRequestIn(method, action, timeout, true)
}

// beamRequestIn is beamRequest, naming this repository to the daemon
// unless withWorkspace is false.
func beamRequestIn(method, action string, timeout time.Duration, withWorkspace bool) ([]byte, int, error) {
	base, err := beamControlURL()
	if err != nil {
		return nil, 0, err
	}
	url := base
	if action != "" {
		url += "/" + action
	}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set(beam.ControlHeader, "1")
	if withWorkspace {
		req.Header.Set(egress.WorkspaceHeader, currentWorkspace())
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("talk to the prism daemon: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusNotFound && action == "" {
		return nil, resp.StatusCode, errors.New("the running daemon predates beam support — restart it with `prism down && prism up`")
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(b, &e) == nil && e.Error != "" {
			return nil, resp.StatusCode, errors.New(e.Error)
		}
		return nil, resp.StatusCode, fmt.Errorf("daemon answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return b, resp.StatusCode, nil
}

func beamStart(args []string) error {
	fs := flag.NewFlagSet("beam start", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "prism: starting a beam and opening its forwards (this takes a few seconds)…")
	b, _, err := beamRequest(http.MethodPost, "start", 4*time.Minute)
	if err != nil {
		return explainBeamError(err)
	}
	var st beam.Status
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	if st.Mode == config.BeamOff {
		if st.Beam == nil {
			return errors.New("the daemon started no beam")
		}
		fmt.Fprintf(os.Stderr, "prism: inference now goes through beam %s, until `prism beam end` "+
			"(it is replaced automatically before it expires)\nprism: session: %s\n", st.Beam.Alias, st.Link)
		return nil
	}
	fmt.Fprintln(os.Stderr, "prism: a beam is ready for the next agent session that starts, so its first request needn't wait. "+
		"(Sessions get their own beams anyway: this only saves the wait.)")
	return nil
}

func beamStatus(args []string) error {
	fs := flag.NewFlagSet("beam status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the daemon's status as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	b, _, err := beamRequest(http.MethodGet, "", 10*time.Second)
	if err != nil {
		return err
	}
	if *asJSON {
		_, err := os.Stdout.Write(b)
		return err
	}
	var st beam.Status
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	mode := cmp.Or(st.Mode, config.BeamRequired)
	here := currentWorkspace()
	if len(st.Beams) > 0 {
		fmt.Printf("beams:     %d running (at most %d)\n", len(st.Beams), st.Max)
		starred := false
		for _, l := range st.Beams {
			mark := " "
			if l.Beam.Workspace == here {
				mark, starred = "*", true
			}
			fmt.Printf("  %s %-16s %-22s %4d req  expires %s  %s\n", mark, l.Beam.Alias, laneLabel(l.Beam),
				l.Beam.Requests, l.Beam.Expires.Local().Format("15:04"), cmp.Or(workspaceLabel(l.Beam.Workspace), "-"))
			if len(l.Joined) > 0 {
				fmt.Printf("    also carrying %s (beam_max reached)\n", strings.Join(l.Joined, ", "))
			}
		}
		if starred {
			fmt.Println("  (* this repository)")
		}
	}
	cfg, _ := config.Load()
	_, loggedIn := loginUsable()
	switch {
	case mode == config.BeamOff && len(st.Beams) == 0:
		fmt.Println("beam:      none — inference goes through the tunnels and isn't recorded in Beams")
	case mode == config.BeamOff:
	case !loggedIn && !cfg.BeamAutoLogin && mode == config.BeamRequired:
		fmt.Println("new beams: can't start without your Teleport login: a new session's requests are refused until `prism login`")
	case !loggedIn && !cfg.BeamAutoLogin:
		fmt.Println("new beams: can't start without your Teleport login: a new session's requests go UNRECORDED until `prism login`")
	default:
		fmt.Printf("new beams: each new agent session starts its own (scope %s)\n", cmp.Or(st.Scope, beam.ScopeSession))
	}
	fmt.Printf("mode:      %s (%s)\n", mode, beamModeMeaning(mode))
	if mode != config.BeamOff {
		fmt.Printf("login:     %s\n", describeLogin())
	}
	if st.Unrecorded > 0 {
		fmt.Printf("no-beam:   %d request(s) sent unrecorded since the daemon started (`prism --no-beam`)\n", st.Unrecorded)
	}
	if st.LastError != "" {
		fmt.Printf("last error: %s\n", st.LastError)
	}
	return nil
}

// laneLabel says what a beam carries, for people.
func laneLabel(r beam.Record) string {
	k := r.Key
	switch {
	case k == "standby":
		return "standby (unclaimed)"
	case k == "shared":
		return "untagged traffic"
	case k == "manual":
		return "everything (beam start)"
	case strings.HasPrefix(k, "repo:"):
		return "repository"
	}
	if i := strings.Index(k, ":"); i > 0 {
		id, kind := k[i+1:], k[:i]
		if len(id) > 8 {
			id = id[:8]
		}
		if kind == "session" {
			return "session " + id // Codex, or Pi on Responses
		}
		return kind + " session " + id
	}
	return k
}

// workspaceLabel shortens a repository path for a table.
func workspaceLabel(ws string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(ws, home+"/") {
		return "~" + ws[len(home):]
	}
	return ws
}

func beamEnd(args []string) error {
	fs := flag.NewFlagSet("beam end", flag.ContinueOnError)
	all := fs.Bool("all", false, "end every beam, not just this repository's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Without a repository named, the daemon ends every beam.
	b, code, err := beamRequestIn(http.MethodPost, "end", 30*time.Second, !*all)
	if code == http.StatusConflict {
		fmt.Fprintln(os.Stderr, "prism: no beam is in use")
		return nil
	}
	if err != nil {
		return err
	}
	var out struct {
		Ended  []beam.Record `json:"ended"`
		Status beam.Status   `json:"status"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	for _, r := range out.Ended {
		fmt.Fprintf(os.Stderr, "prism: ended beam %s (%s)\n", r.Alias, laneLabel(r))
		fmt.Println(r.Link(""))
	}
	fmt.Fprintln(os.Stderr, "prism: each is removed once its in-flight requests finish, and summarised in Beams about 15 minutes after that.")
	if out.Status.Mode != config.BeamOff {
		fmt.Fprintln(os.Stderr, "prism: a session that sends another request gets a new beam.")
	}
	return nil
}

// beamLink prints the beams prism recorded this repository's work in
// recently, for a PR. It reads beams.json directly, so it works with the
// daemon stopped.
func beamLink(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("beam link", flag.ContinueOnError)
	pr := fs.Bool("pr", false, "print a markdown section for a pull request description")
	since := fs.Duration("since", 24*time.Hour, "include beams in use within this long")
	all := fs.Bool("all", false, "include every repository's beams, not just this one's")
	if err := fs.Parse(args); err != nil {
		return err
	}
	f, err := beam.Load()
	if err != nil {
		return err
	}
	recs := f.Since(time.Now().Add(-*since))
	// beams.json is saved when a beam starts, ends or the daemon stops,
	// so a running beam has a stale count there; the daemon has the live one.
	if b, _, err := beamRequest(http.MethodGet, "", 3*time.Second); err == nil {
		var st beam.Status
		if json.Unmarshal(b, &st) == nil {
			for _, l := range st.Beams {
				for i := range recs {
					if recs[i].UUID == l.Beam.UUID {
						recs[i].Requests = l.Beam.Requests
					}
				}
			}
		}
	}
	mine, untagged := recs, []beam.Record(nil)
	if !*all {
		mine, untagged = pickWorkspace(recs, currentWorkspace())
	}
	if len(mine) == 0 && len(untagged) == 0 {
		return fmt.Errorf("no beam carried this repository's work in the last %s (`--all` lists every repository's)", *since)
	}
	if !*pr {
		for _, r := range append(mine, untagged...) {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Alias, beamSpan(r), laneLabel(r), r.Link(""))
		}
		return nil
	}
	writeBeamPR(w, mine, untagged, f)
	return nil
}

// pickWorkspace splits recs into the beams tagged with ws and the shared
// beams — traffic that named no session or repository, such as Pi's —
// which may hold this repository's work too.
func pickWorkspace(recs []beam.Record, ws string) (mine, untagged []beam.Record) {
	for _, r := range recs {
		switch {
		case r.Workspace == ws:
			mine = append(mine, r)
		case r.Key == "shared" && r.Requests > 0:
			untagged = append(untagged, r)
		}
	}
	return mine, untagged
}

func beamSpan(r beam.Record) string {
	end := "now"
	if !r.Ended.IsZero() {
		end = r.Ended.UTC().Format("15:04")
	}
	return r.Started.UTC().Format("2006-01-02 15:04") + "–" + end + " UTC"
}

// writeBeamPR writes the PR section. It states only what prism knows: which
// beams carried the inference, and where their recordings are. The human
// review the policy asks for is the author's to describe.
func writeBeamPR(w io.Writer, mine, untagged []beam.Record, f *beam.File) {
	fmt.Fprintln(w, "### AI session recordings")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "AI agents' inference for this change was recorded in Teleport Beams (sent through each beam by [prism](https://github.com/webvictim/prism)):")
	fmt.Fprintln(w)
	pending := false
	table := func(recs []beam.Record) {
		fmt.Fprintln(w, "| Beam | Time | Requests | Recording |")
		fmt.Fprintln(w, "| --- | --- | --- | --- |")
		for _, r := range recs {
			note := ""
			if f.Running(r.UUID) || r.Ended.IsZero() {
				note = " (in use)"
				pending = true
			}
			fmt.Fprintf(w, "| `%s`%s | %s | %d | [session](%s) |\n", r.Alias, note, beamSpan(r), r.Requests, r.Link(""))
		}
		fmt.Fprintln(w)
	}
	if len(mine) > 0 {
		table(mine)
	}
	if len(untagged) > 0 {
		fmt.Fprintln(w, "Also in use then, carrying work prism couldn't attribute to a repository:")
		fmt.Fprintln(w)
		table(untagged)
	}
	if pending {
		fmt.Fprintln(w, "A beam still in use is summarised about 15 minutes after it ends (`prism beam end`).")
	} else {
		fmt.Fprintln(w, "Each beam's summary appears in Beams about 15 minutes after it ends.")
	}
}

// explainBeamError turns the errors a user can act on into instructions.
func explainBeamError(err error) error {
	if strings.Contains(err.Error(), "no usable tsh login") {
		return fmt.Errorf("%w — beams belong to a person, so they need your own login: run `prism login` and try again", err)
	}
	return err
}

// beamModeMeaning says in a few words what a beam mode does.
func beamModeMeaning(mode string) string {
	switch mode {
	case config.BeamOff:
		return "inference isn't recorded in Beams unless you run `prism beam start`"
	case config.BeamPreferred:
		return "inference is recorded in Beams when a beam can be started, and sent unrecorded when not"
	default:
		return "inference is sent only through a beam, so every session is recorded in Beams"
	}
}

// describeLogin says whether the tsh login that beams need is usable.
func describeLogin() string {
	st, err := tshwrap.StatusJSON()
	switch {
	case err != nil:
		return "unknown (" + err.Error() + ")"
	case !st.LoggedIn():
		return "none — `prism login` before the next beam has to start"
	case st.IsExpired():
		return "expired at " + st.ValidUntil.Local().Format("Mon 15:04") + " — `prism login` before the next beam has to start"
	}
	return "valid until " + st.ValidUntil.Local().Format("Mon 15:04")
}
