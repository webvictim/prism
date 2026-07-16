package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gravitational/prism/internal/logfile"
	"github.com/gravitational/prism/internal/state"
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

func tailFollow(path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(w, f); err != nil {
		return err
	}
	for {
		n, err := io.Copy(w, f)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if n == 0 {
			time.Sleep(200 * time.Millisecond)
		}
	}
}
