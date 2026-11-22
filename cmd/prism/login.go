package main

import (
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/webvictim/prism/internal/beam"
	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/egress"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tshwrap"
)

// cmdLogin implements `prism login`: a `tsh login` to prism's cluster, for
// as long as the cluster allows. Beams belong to a person, so recording
// sessions in Beams needs the user's own login even when tbot carries the
// tunnels; this is the one command to remember when prism says so.
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	ttl := fs.Int("ttl", tshwrap.LoginTTLMinutes, "minutes the login should last (your roles cap it: Teleport shortens a longer request rather than refusing it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := tshLogin(*ttl, fs.Args()); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "prism: Teleport login %s; beams can be started until then.\n", describeLogin())
	return nil
}

// tshLogin runs `tsh login` attached to the terminal, so its browser or
// MFA prompts reach the user.
func tshLogin(ttlMinutes int, extra []string) error {
	bin, err := tshwrap.LookPathStrict("tsh")
	if err != nil {
		return fmt.Errorf("tsh not found: %w", err)
	}
	args := tshwrap.Args(append([]string{"login", "--ttl=" + strconv.Itoa(ttlMinutes)}, extra...)...)
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tsh login: %w", err)
	}
	return nil
}

// loginUsable reports whether there's a login a beam can be started with,
// and until when.
func loginUsable() (time.Time, bool) {
	st, err := tshwrap.StatusJSON()
	if err != nil || !st.LoggedIn() || st.IsExpired() {
		return time.Time{}, false
	}
	return st.ValidUntil, true
}

// prepareBeamForLaunch gets a beam ready before a launcher starts its
// tool, when one will be needed. It is the friendly path: a person at a
// terminal is logged in on the spot if their login has lapsed (the daemon
// can never do that), and sees the beam start rather than waiting out a
// silent first request. Without a terminal it does nothing, and the first
// request starts the beam, or is refused with the reason.
//
// It returns an error only when the tool would be unusable: in required
// mode, with no beam to be had.
//
// override is the launch's `--no-beam` / `--beam` (beam_override.go).
// --no-beam skips all of it, since the point is not to wait on a beam:
// no login, no warmed standby. --beam prepares as required mode does.
func prepareBeamForLaunch(tool, override string) error {
	if override == egress.BeamOff {
		return nil
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stderr) {
		return nil
	}
	b, _, err := beamRequest(http.MethodGet, "", 3*time.Second)
	if err != nil {
		// A daemon from before beams, or one not answering yet: the tool
		// still works, so say why there's no beam and carry on.
		fmt.Fprintf(os.Stderr, "prism: can't check recording in Beams: %v\n", err)
		return nil
	}
	var st beam.Status
	if err := json.Unmarshal(b, &st); err != nil {
		return nil
	}
	if restarted, err := restartIfOutdated(st); err != nil {
		return err
	} else if restarted {
		if b, _, err = beamRequest(http.MethodGet, "", 3*time.Second); err != nil || json.Unmarshal(b, &st) != nil {
			return nil
		}
	}
	mode := cmp.Or(st.Mode, config.BeamRequired)
	if override == egress.BeamRequired {
		mode = config.BeamRequired
	}
	if mode == config.BeamOff {
		return nil
	}
	without := func(err error) error {
		if mode == config.BeamPreferred {
			fmt.Fprintf(os.Stderr, "prism: %v\nprism: this session will NOT be recorded in Beams (beam_mode=preferred)\n", err)
			return nil
		}
		return fmt.Errorf("%w\nprism: not starting %s: prism sends inference only through a beam, so it couldn't answer. "+
			"`prism login`, then try again", err, tool)
	}
	if _, ok := loginUsable(); !ok {
		fmt.Fprintln(os.Stderr, "prism: recording this session in Beams needs your own Teleport login, which has expired. Logging in…")
		if err := tshLogin(tshwrap.LoginTTLMinutes, nil); err != nil {
			return without(err)
		}
	}
	// Each session gets a beam of its own when it sends its first request.
	// Warming one now means that request needn't wait for it; the session
	// claims it. At the cap there's no room to warm one, and the session
	// will share a beam of this repository's instead.
	if st.Standby || (st.Max > 0 && len(st.Beams) >= st.Max) {
		return nil
	}
	// In off mode a started beam is the manual one, which would carry
	// every launch's traffic, not just this one's; this session starts its
	// own on its first request instead.
	if override == egress.BeamRequired && st.Mode == config.BeamOff {
		return nil
	}
	fmt.Fprintln(os.Stderr, "prism: starting a beam to record this session in (about 15 seconds)…")
	b, _, err = beamRequest(http.MethodPost, "start", 4*time.Minute)
	if err != nil {
		return without(explainBeamError(err))
	}
	if err := json.Unmarshal(b, &st); err == nil {
		for _, l := range st.Beams {
			if l.Beam.Key == "standby" {
				fmt.Fprintf(os.Stderr, "prism: recording in beam %s: %s\n", l.Beam.Alias, l.Link)
			}
		}
	}
	return nil
}

// restartIfOutdated restarts a daemon older than this prism — what `brew
// upgrade` leaves running — so an upgrade takes effect without anyone
// typing `prism down && prism up`. Only when it is carrying nothing
// (Status.Busy): a restart cuts off whatever is in flight, an agent's
// stream included. Otherwise it says so and leaves it be; the next launch
// tries again. It keeps the router's port, which running agents point at.
func restartIfOutdated(st beam.Status) (bool, error) {
	if st.Version == "" || st.Version == version {
		return false, nil
	}
	if st.Busy > 0 {
		fmt.Fprintf(os.Stderr, "prism: the daemon is running %s, older than this prism (%s); it's busy, so it'll be "+
			"restarted at a quieter launch (or now: `prism down && prism up`)\n", st.Version, version)
		return false, nil
	}
	s, err := state.Load()
	if err != nil || s == nil || s.LocalPort == 0 {
		return false, nil
	}
	fmt.Fprintf(os.Stderr, "prism: restarting the daemon to pick up this prism (%s, was %s)…\n", version, st.Version)
	if err := cmdDown(nil); err != nil {
		return false, fmt.Errorf("restart the daemon: %w", err)
	}
	// up prints the environment for eval; a launcher's stdout is the tool's.
	if err := withoutStdout(func() error { return cmdUp([]string{"--port", strconv.Itoa(s.LocalPort)}) }); err != nil {
		return false, fmt.Errorf("restart the daemon: %w", err)
	}
	return true, nil
}

// withoutStdout runs f with os.Stdout sent to the null device.
func withoutStdout(f func() error) error {
	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return f()
	}
	defer null.Close()
	orig := os.Stdout
	os.Stdout = null
	defer func() { os.Stdout = orig }()
	return f()
}
