package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/tbot"
)

// orphanGrace is how long a leftover tbot gets to exit on SIGTERM before
// it is killed. tbot finishes an in-flight renewal and writes it to
// storage/ on SIGTERM; SIGKILL mid-renewal is itself a way to strand the
// generation the cluster has already recorded.
const orphanGrace = 10 * time.Second

// stopOrphanedTbots stops any tbot still running on prism's own
// tbot.yaml, before the daemon starts one of its own.
//
// Two tbots on one storage directory renew one bound keypair against
// each other. The cluster records a new generation on every renewal, so
// whichever presents the older one next is taken for a stolen
// certificate and the join token is locked — permanently, and with no
// copying involved. The second tbot is usually one an earlier daemon
// left behind: on Unix nothing reaps a child when its daemon dies
// uncleanly, so it carries on renewing, reparented to PID 1, with its
// output going nowhere. That is why such a lock can appear with no
// mismatch line in prism's log at all.
//
// The test is the config path on tbot's command line, as in prism
// doctor's `tbot process` check, not parentage — except that a tbot
// whose parent is a live prism daemon belongs to that daemon. Killing it
// would only see it restarted, so the daemon refuses to start instead:
// two daemons are the same hazard one level up.
//
// Windows needs none of this: Job Objects end tbot with its daemon.
func stopOrphanedTbots(dir string, logger *log.Logger) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	procs, err := tbotProcesses()
	if err != nil {
		// Can't look, so can't tell. A daemon that refused to start on a
		// machine without ps would be worse than the hazard.
		logger.Printf("tbot: could not list running tbots (%v); not checking for leftovers", err)
		return nil
	}
	for _, pid := range tbotsOnConfig(procs, tbot.TbotYAMLPath(dir)) {
		if ppid, parent, err := processParent(pid); err == nil && ppid != os.Getpid() &&
			isPrismDaemon(parent) && processAlive(ppid) {
			return fmt.Errorf("another prism daemon (pid %d) is already running tbot (pid %d) on this identity; "+
				"two would renew one bound keypair against each other and get the bot locked — `prism down` first",
				ppid, pid)
		}
		logger.Printf("tbot: stopping pid %d, left running on prism's tbot.yaml by an earlier daemon — "+
			"two tbots on one identity get its join token locked", pid)
		if err := stopProcess(pid, orphanGrace); err != nil {
			return fmt.Errorf("could not stop the tbot (pid %d) left running on this identity: %w", pid, err)
		}
		logger.Printf("tbot: pid %d stopped", pid)
	}
	return nil
}

// isPrismTbot reports whether a tbot command line is running prism's
// config. It is the one definition of "prism's tbot", shared with
// prism doctor.
func isPrismTbot(cmdline, yamlPath string) bool {
	return strings.Contains(cmdline, yamlPath)
}

// tbotsOnConfig returns the pids, in order, of the tbots running yamlPath.
func tbotsOnConfig(procs map[int]string, yamlPath string) []int {
	var pids []int
	for pid, cmdline := range procs {
		if isPrismTbot(cmdline, yamlPath) {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids
}

// isPrismDaemon reports whether a command line is a prism daemon:
// a binary named prism (or a cross-built prism-<os>-<arch>) run with
// the hidden __daemon subcommand.
func isPrismDaemon(cmdline string) bool {
	fields := strings.Fields(cmdline)
	return len(fields) >= 2 && strings.HasPrefix(filepath.Base(fields[0]), "prism") &&
		slices.Contains(fields[1:], "__daemon")
}

// processParent returns pid's parent pid and the parent's command line.
func processParent(pid int) (int, string, error) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, "", err
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, "", err
	}
	out, err = exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(ppid)).Output()
	if err != nil {
		return ppid, "", err
	}
	return ppid, strings.TrimSpace(string(out)), nil
}

// stopProcess asks pid to exit and waits up to grace for it to go, then
// kills it.
func stopProcess(pid int, grace time.Duration) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := signalShutdown(p); err != nil {
		if !processAlive(pid) {
			return nil
		}
		return err
	}
	if waitGone(pid, grace) {
		return nil
	}
	if err := signalKill(p); err != nil && processAlive(pid) {
		return err
	}
	if waitGone(pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("still running after SIGKILL")
}

// waitGone polls until pid no longer exists or the timeout passes.
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return true
}
