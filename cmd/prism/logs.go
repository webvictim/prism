package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/webvictim/prism/internal/logfile"
	"github.com/webvictim/prism/internal/state"
)

func cmdLogs(_ []string) error {
	s, err := state.Load()
	if err != nil {
		return err
	}
	if s == nil {
		fmt.Fprintln(os.Stderr, "prism: no active session")
		return nil
	}

	if isServiceManaged() {
		return journalFollow()
	}

	logDir, err := state.DaemonLogDir()
	if err != nil {
		return err
	}
	logPath := logfile.LatestPath(logDir)
	return tailFollow(logPath, os.Stdout)
}

// tailFollow streams path to w and keeps following across daily log
// rotation. The daemon rotates by creating a new dated file rather than
// renaming the current one, so a follower holding a single handle simply
// stops seeing output at midnight — it has to notice the new file and
// switch to it.
func tailFollow(path string, w io.Writer) error {
	dir := filepath.Dir(path)
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { f.Close() }()

	for {
		n, err := io.Copy(w, f)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n > 0 {
			continue
		}

		// Caught up with the current file. If the daemon has rolled over,
		// switch — but drain the old handle one more time first, since
		// lines may have landed there between the copy above and now.
		if latest := logfile.LatestPath(dir); latest != path {
			next, oerr := os.Open(latest)
			if oerr == nil {
				if _, err := io.Copy(w, f); err != nil && !errors.Is(err, io.EOF) {
					next.Close()
					return err
				}
				f.Close()
				f, path = next, latest
				// To stderr, so piping `prism logs` stays clean.
				fmt.Fprintf(os.Stderr, "==> %s <==\n", filepath.Base(latest))
				continue
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
}
