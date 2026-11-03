package tbot

import (
	"fmt"
	"os"
	"strings"
)

// Names are the cluster resource names prism uses for one machine. They
// carry the hostname so that two machines sharing a cluster — and a
// clone that has been renamed — can never land on the same bot.
type Names struct {
	Token string
	Role  string
	Bot   string
}

// NamesForHost derives the resource names for a hostname, sanitised for
// use as Teleport resource names: lowercased, and only the first label,
// so `Athena.local` and `athena` agree.
func NamesForHost(hostname string) Names {
	host := strings.ToLower(strings.Split(strings.TrimSpace(hostname), ".")[0])
	if host == "" {
		host = "unknown"
	}
	return Names{
		Token: "prism-bot-token-" + host,
		Role:  "prism-bot-role-" + host,
		Bot:   "prism-bot-" + host,
	}
}

// Host returns the hostname the names are scoped to, or "" if they
// aren't in prism's own shape.
func (n Names) Host() string { return strings.TrimPrefix(n.Bot, "prism-bot-") }

// LocalNames is NamesForHost for the machine prism is running on.
func LocalNames() Names {
	hostname, _ := os.Hostname()
	return NamesForHost(hostname)
}

// ForeignMachineError reports a tbot directory whose identity was
// issued to a different machine — the shape a cloned Mac takes.
type ForeignMachineError struct {
	Dir      string
	Recorded string
	Current  string
}

func (e *ForeignMachineError) Error() string {
	return fmt.Sprintf("the tbot identity in %s belongs to a different machine "+
		"(it records %s, this machine is %s). It was most likely copied here by "+
		"Migration Assistant or a disk clone. Starting tbot would make two machines "+
		"renew one bot, which can lock it and break prism on the original machine. "+
		"Run `prism tbot reset` to give this machine its own bot",
		e.Dir, short(e.Recorded), short(e.Current))
}

// short trims a machine id to something a person can compare at a
// glance in a log line, since the full value is only ever matched
// mechanically.
func short(id string) string {
	if len(id) > 12 {
		return id[:12] + "…"
	}
	return id
}

// CheckMachine compares the machine id recorded in dir's sidecar with
// current, which the caller reads from machineid.Current.
//
// It returns a *ForeignMachineError on a mismatch, and nil in every
// other case — including when current is empty, because a platform
// prism can't read an id from must not be treated as a clone, and when
// the sidecar is missing or unreadable, which Validate reports instead.
//
// A sidecar with no recorded id adopts current. That is the upgrade
// path for installs made before prism recorded one, and it means the
// protection only covers a clone whose *source* machine ran a prism
// that records ids at least once before being cloned.
func CheckMachine(dir, current string) error {
	if current == "" {
		return nil
	}
	sidecar, err := LoadSidecar(dir)
	if err != nil {
		return nil
	}
	switch sidecar.MachineID {
	case current:
		return nil
	case "":
		sidecar.MachineID = current
		// A failed write is not worth blocking a working prism for: the
		// next run tries again.
		_ = SaveSidecar(dir, sidecar)
		return nil
	default:
		return &ForeignMachineError{Dir: dir, Recorded: sidecar.MachineID, Current: current}
	}
}
