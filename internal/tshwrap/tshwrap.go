// Package tshwrap wraps the `tsh` CLI for the subcommands prism uses.
package tshwrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// tshBin resolves the absolute path to tsh. On Windows, exec.LookPath
// may return a match from the current directory (ErrDot) which Go 1.19+
// rejects. When that happens, we search PATH directories explicitly,
// skipping CWD.
func tshBin() (string, error) {
	return lookPathStrict("tsh")
}

func lookPathStrict(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, exec.ErrDot) {
		return "", err
	}
	// ErrDot: found in CWD. Search PATH entries explicitly.
	return searchPATH(name)
}

// LookPathStrict resolves a binary from PATH, skipping the current
// directory. Use this instead of exec.LookPath to avoid the Go 1.19+
// ErrDot issue on Windows where CWD is searched first.
func LookPathStrict(name string) (string, error) {
	return lookPathStrict(name)
}

func searchPATH(name string) (string, error) {
	pathenv := os.Getenv("PATH")
	if pathenv == "" {
		return "", fmt.Errorf("%s not found in PATH", name)
	}

	var exts []string
	if runtime.GOOS == "windows" {
		for _, e := range strings.Split(strings.ToLower(os.Getenv("PATHEXT")), ";") {
			if e != "" {
				exts = append(exts, e)
			}
		}
		if len(exts) == 0 {
			exts = []string{".com", ".exe", ".bat", ".cmd"}
		}
	}

	cwd, _ := os.Getwd()

	for _, dir := range filepath.SplitList(pathenv) {
		if dir == "" || dir == "." {
			continue
		}
		abs, _ := filepath.Abs(dir)
		if abs == cwd {
			continue
		}
		if runtime.GOOS == "windows" {
			for _, ext := range exts {
				p := filepath.Join(dir, name+ext)
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
					return p, nil
				}
			}
		} else {
			p := filepath.Join(dir, name)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s not found in PATH (only in current directory)", name)
}

// App is a subset of `tsh apps ls --format=json`.
type App struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		PublicAddr string `json:"public_addr"`
		URI        string `json:"uri"`
	} `json:"spec"`
}

// AppConfig is what `tsh app config --format=json` returns.
type AppConfig struct {
	Name string `json:"name"`
	URI  string `json:"uri"`
	CA   string `json:"ca"`
	Cert string `json:"cert"`
	Key  string `json:"key"`
	Curl string `json:"curl"`
}

func run(args ...string) ([]byte, error) {
	bin, err := tshBin()
	if err != nil {
		return nil, fmt.Errorf("tsh not found: %w", err)
	}
	cmd := exec.Command(bin, args...)
	HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tsh %v: %w (stderr: %s)", args, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

func runStream(args ...string) error {
	bin, err := tshBin()
	if err != nil {
		return fmt.Errorf("tsh not found: %w", err)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// AppsList returns all apps visible to the current user.
func AppsList() ([]App, error) {
	out, err := run("apps", "ls", "--format=json")
	if err != nil {
		return nil, err
	}
	var as []App
	if err := json.Unmarshal(out, &as); err != nil {
		return nil, fmt.Errorf("parse apps ls: %w", err)
	}
	return as, nil
}

// AppLogin runs `tsh app login <name>`.
func AppLogin(name string) error {
	return runStream("app", "login", name)
}

// AppLogout drops cached app credentials. Best-effort.
func AppLogout(name string) error {
	bin, err := tshBin()
	if err != nil {
		return nil
	}
	cmd := exec.Command(bin, "app", "logout", name)
	HideWindow(cmd)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
	return nil
}

// AppConfigJSON returns the JSON config for a logged-in app.
func AppConfigJSON(name string) (*AppConfig, error) {
	out, err := run("app", "config", "--format=json", name)
	if err != nil {
		return nil, err
	}
	var c AppConfig
	if err := json.Unmarshal(out, &c); err != nil {
		return nil, fmt.Errorf("parse app config: %w", err)
	}
	return &c, nil
}

// Status describes the current tsh session.
type Status struct {
	ProfileURL string    `json:"profile_url"`
	Username   string    `json:"username"`
	Cluster    string    `json:"cluster"`
	ValidUntil time.Time `json:"valid_until"`
}

func (s *Status) IsExpired() bool {
	if s == nil || s.ValidUntil.IsZero() {
		return true
	}
	return time.Now().After(s.ValidUntil)
}

// StatusJSON returns the active tsh profile.
func StatusJSON() (*Status, error) {
	out, err := run("status", "--format=json")
	if err != nil {
		if isNotLoggedIn(err) {
			return &Status{}, nil
		}
		return nil, err
	}
	var raw struct {
		Active *Status `json:"active"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse tsh status: %w", err)
	}
	if raw.Active == nil {
		return &Status{}, nil
	}
	return raw.Active, nil
}

func isNotLoggedIn(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not logged in") || strings.Contains(msg, "no profiles")
}
