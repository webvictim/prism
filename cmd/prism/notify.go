package main

import (
	"context"
	"log"
	"os/exec"
	"runtime"
	"strconv"
	"time"
)

// desktopNotify shows msg as a desktop notification, for the few things
// the daemon must tell a person who may not be looking at any terminal:
// recording has stopped, or is about to. Best effort, and the log line is
// written either way by the caller.
//
// The daemon runs in the user's session (a LaunchAgent, a systemd user
// unit, or a process forked from their shell), which is what lets it
// reach the notification centre at all.
func desktopNotify(logger *log.Logger, msg string) {
	var cmd []string
	switch runtime.GOOS {
	case "darwin":
		cmd = []string{"/usr/bin/osascript", "-e",
			"display notification " + strconv.QuoteToGraphic(msg) + ` with title "prism"`}
	case "linux":
		path, err := exec.LookPath("notify-send")
		if err != nil {
			return
		}
		cmd = []string{path, "--app-name=prism", "prism", msg}
	default:
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			logger.Printf("daemon: desktop notification failed: %v %s", err, out)
		}
	}()
}
