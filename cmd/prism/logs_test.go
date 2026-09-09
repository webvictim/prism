package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a Writer safe to read while tailFollow writes to it.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitFor polls until the buffer contains want, or fails the test.
func waitFor(t *testing.T, out *syncBuf, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; got:\n%s", want, out.String())
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// The daemon rotates by creating a new dated file rather than renaming the
// current one, so a follower holding one handle goes silent at midnight.
func TestTailFollowSwitchesToRotatedFile(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "daemon-2026-09-08.log")
	newPath := filepath.Join(dir, "daemon-2026-09-09.log")

	appendLine(t, oldPath, "line from yesterday")

	out := &syncBuf{}
	go func() { _ = tailFollow(oldPath, out) }()

	waitFor(t, out, "line from yesterday")

	// Still following the current file.
	appendLine(t, oldPath, "another yesterday line")
	waitFor(t, out, "another yesterday line")

	// Rotation: a new dated file appears. Anything already in the old file
	// must still be delivered, in order, before the switch.
	appendLine(t, oldPath, "last gasp")
	appendLine(t, newPath, "line from today")

	waitFor(t, out, "line from today")
	waitFor(t, out, "last gasp")

	got := out.String()
	if strings.Index(got, "last gasp") > strings.Index(got, "line from today") {
		t.Errorf("old file's tail should be drained before switching; got:\n%s", got)
	}

	// And it keeps following the new file afterwards.
	appendLine(t, newPath, "still going")
	waitFor(t, out, "still going")
}

// A follower started before the daemon has written today's file should pick
// it up when it appears.
func TestTailFollowPicksUpFirstRotation(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "daemon-2026-09-08.log")
	appendLine(t, oldPath, "before midnight")

	out := &syncBuf{}
	go func() { _ = tailFollow(oldPath, out) }()
	waitFor(t, out, "before midnight")

	appendLine(t, filepath.Join(dir, "daemon-2026-09-09.log"), "after midnight")
	waitFor(t, out, "after midnight")
}

func TestTailFollowMissingFileErrors(t *testing.T) {
	if err := tailFollow(filepath.Join(t.TempDir(), "nope.log"), &syncBuf{}); err == nil {
		t.Error("expected an error for a missing log file")
	}
}
