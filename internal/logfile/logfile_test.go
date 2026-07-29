package logfile

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteCreatesFile(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}

	today := time.Now().Format("2006-01-02")
	path := filepath.Join(dir, "daemon-"+today+".log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
	if string(data) != "hello\n" {
		t.Errorf("got %q, want %q", data, "hello\n")
	}
}

func TestDateRotation(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	day1 := time.Date(2025, 3, 15, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2025, 3, 16, 12, 0, 0, 0, time.UTC)

	w.mu.Lock()
	w.nowFunc = func() time.Time { return day1 }
	w.today = "" // force rotation
	w.mu.Unlock()

	w.Write([]byte("day1\n"))

	w.mu.Lock()
	w.nowFunc = func() time.Time { return day2 }
	w.mu.Unlock()

	w.Write([]byte("day2\n"))
	w.Close()

	// Give background compression goroutine time to finish.
	time.Sleep(50 * time.Millisecond)

	f1 := filepath.Join(dir, "daemon-2025-03-15.log")
	f1gz := f1 + ".gz"
	f2 := filepath.Join(dir, "daemon-2025-03-16.log")

	// day1 may have been compressed by compressOld; either form is acceptable.
	if _, err := os.Stat(f1); err != nil {
		if _, err := os.Stat(f1gz); err != nil {
			t.Errorf("day1 file missing (neither .log nor .log.gz)")
		}
	}
	if _, err := os.Stat(f2); err != nil {
		t.Errorf("day2 file missing: %v", err)
	}

	data, _ := os.ReadFile(f2)
	if string(data) != "day2\n" {
		t.Errorf("day2 content = %q, want %q", data, "day2\n")
	}
}

func TestCompressOld(t *testing.T) {
	dir := t.TempDir()

	stale := filepath.Join(dir, "daemon-2020-01-01.log")
	os.WriteFile(stale, []byte("old log data"), 0o600)

	w, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	// Give the background goroutine time to compress.
	time.Sleep(100 * time.Millisecond)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale .log file should have been removed after compression")
	}
	gz := stale + ".gz"
	if _, err := os.Stat(gz); err != nil {
		t.Fatalf("compressed file missing: %v", err)
	}

	// Verify the gzip content is valid.
	f, _ := os.Open(gz)
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip open: %v", err)
	}
	data, _ := io.ReadAll(r)
	if string(data) != "old log data" {
		t.Errorf("decompressed = %q, want %q", data, "old log data")
	}
}

func TestMigrateLegacy(t *testing.T) {
	parent := t.TempDir()
	logsDir := filepath.Join(parent, "logs")
	os.MkdirAll(logsDir, 0o700)

	legacy := filepath.Join(parent, "daemon.log")
	os.WriteFile(legacy, []byte("legacy content"), 0o600)

	w, err := Open(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Error("legacy daemon.log should have been removed")
	}
	migrated := filepath.Join(logsDir, "daemon-legacy.log.gz")
	if _, err := os.Stat(migrated); err != nil {
		t.Fatalf("migrated file missing: %v", err)
	}

	f, _ := os.Open(migrated)
	defer f.Close()
	r, _ := gzip.NewReader(f)
	data, _ := io.ReadAll(r)
	if string(data) != "legacy content" {
		t.Errorf("migrated content = %q, want %q", data, "legacy content")
	}
}

func TestLatestPath(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "daemon-2025-01-01.log"), []byte{}, 0o600)
	os.WriteFile(filepath.Join(dir, "daemon-2025-06-15.log"), []byte{}, 0o600)
	os.WriteFile(filepath.Join(dir, "daemon-2025-03-10.log"), []byte{}, 0o600)

	got := LatestPath(dir)
	want := filepath.Join(dir, "daemon-2025-06-15.log")
	if got != want {
		t.Errorf("LatestPath = %q, want %q", got, want)
	}
}
