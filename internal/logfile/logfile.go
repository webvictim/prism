// Package logfile provides a date-rotating log writer for the prism daemon.
package logfile

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Writer is a date-rotating log file writer. It writes to
// daemon-YYYY-MM-DD.log and compresses older .log files to .log.gz.
type Writer struct {
	mu      sync.Mutex
	dir     string
	today   string // "2006-01-02"
	file    *os.File
	nowFunc func() time.Time // for testing
}

// Open creates a Writer that writes dated log files into dir.
// On first call it migrates any legacy daemon.log to daemon-legacy.log.gz.
func Open(dir string) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	migrateLegacy(dir)
	w := &Writer{dir: dir, nowFunc: time.Now}
	if err := w.rotate(); err != nil {
		return nil, err
	}
	go w.compressOld()
	return w, nil
}

// Write implements io.Writer. It checks for date rollover on each call.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	today := w.nowFunc().Format("2006-01-02")
	if today != w.today {
		w.today = today
		if err := w.rotateLocked(); err != nil {
			if w.file != nil {
				return w.file.Write(p)
			}
			return 0, err
		}
		go w.compressOld()
	}
	return w.file.Write(p)
}

// Close closes the current log file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// CurrentPath returns the path of today's log file.
func CurrentPath(dir string) string {
	return filepath.Join(dir, fmt.Sprintf("daemon-%s.log", time.Now().Format("2006-01-02")))
}

// LatestPath returns the most recent log file in dir (for prism logs fallback).
func LatestPath(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return CurrentPath(dir)
	}
	var latest string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "daemon-") && strings.HasSuffix(name, ".log") {
			if name > latest {
				latest = name
			}
		}
	}
	if latest == "" {
		return CurrentPath(dir)
	}
	return filepath.Join(dir, latest)
}

func (w *Writer) rotate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.today = w.nowFunc().Format("2006-01-02")
	return w.rotateLocked()
}

func (w *Writer) rotateLocked() error {
	if w.file != nil {
		w.file.Close()
	}
	path := filepath.Join(w.dir, fmt.Sprintf("daemon-%s.log", w.today))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	return nil
}

func (w *Writer) compressOld() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	w.mu.Lock()
	today := w.today
	w.mu.Unlock()

	todayFile := fmt.Sprintf("daemon-%s.log", today)
	for _, e := range entries {
		name := e.Name()
		if name == todayFile {
			continue
		}
		if strings.HasPrefix(name, "daemon-") && strings.HasSuffix(name, ".log") {
			compressFile(filepath.Join(w.dir, name))
		}
	}
}

func compressFile(path string) {
	src, err := os.Open(path)
	if err != nil {
		return
	}
	defer src.Close()

	dst, err := os.OpenFile(path+".gz", os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return
	}

	gz := gzip.NewWriter(dst)
	if _, err := io.Copy(gz, src); err != nil {
		gz.Close()
		dst.Close()
		os.Remove(path + ".gz")
		return
	}
	if err := gz.Close(); err != nil {
		dst.Close()
		os.Remove(path + ".gz")
		return
	}
	dst.Close()
	src.Close()
	os.Remove(path)
}

func migrateLegacy(dir string) {
	legacy := filepath.Join(dir, "daemon.log")
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	compressFile(legacy)
	// compressFile creates daemon.log.gz and removes daemon.log.
	// Rename to daemon-legacy.log.gz for clarity.
	src := filepath.Join(dir, "daemon.log.gz")
	dst := filepath.Join(dir, "daemon-legacy.log.gz")
	os.Rename(src, dst)
}
