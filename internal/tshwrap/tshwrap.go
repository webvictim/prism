// Package tshwrap wraps the `tsh` CLI for the subcommands prism uses.
package tshwrap

import (
	"bytes"
	"context"
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
		PublicAddr string     `json:"public_addr"`
		URI        string     `json:"uri"`
		Inference  *Inference `json:"inference"`
	} `json:"spec"`
}

// Inference describes the wire format and model names an LLM app accepts.
type Inference struct {
	Format   string           `json:"format"`
	Provider string           `json:"provider"`
	Models   []InferenceModel `json:"models"`
}

type InferenceModel struct {
	Name         string `json:"name"`
	ProviderName string `json:"provider_name,omitempty"`
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

// proxy is the cluster every tsh invocation prism makes is aimed at.
// It is set once from prism's configured proxy (see applyConfigEnv),
// and by the commands that resolve a proxy of their own.
//
// Being explicit matters. Otherwise tsh acts on whichever profile the
// user's shell last logged into, so prism's idea of "the cluster" and
// tsh's can differ with nothing to say so — and the only fix on offer
// would be telling people to change their login context, which is not
// prism's business and gets worse the moment a team shares a
// centralised cluster. prism's configured proxy is the authority;
// `tsh --proxy=<addr>` is how that gets honoured.
var proxy string

// SetProxy aims subsequent tsh invocations at addr. Empty leaves tsh
// to choose its own active profile, which is all prism can do when
// nothing is configured yet.
func SetProxy(addr string) { proxy = addr }

// Proxy returns the address tsh invocations are aimed at, or "".
func Proxy() string { return proxy }

// Args returns a tsh argument list aimed at prism's proxy, for callers
// that build their own exec.Cmd rather than going through this package
// — the tunnel supervisor's long-running `tsh proxy app`, which prism
// starts and watches rather than runs to completion.
func Args(args ...string) []string { return proxyArgs(args) }

// proxyArgs puts prism's --proxy in front of a tsh command. tsh takes
// it as a global flag, so it has to precede the subcommand.
func proxyArgs(args []string) []string {
	if proxy == "" {
		return args
	}
	return append([]string{"--proxy=" + proxy}, args...)
}

var (
	streamIn  io.Reader = os.Stdin
	streamOut io.Writer = os.Stdout
)

// SetStdio attaches interactive tsh commands, such as `tsh app login`,
// to in and out instead of prism's own stdin and stdout. A nil in gives
// tsh the null device.
func SetStdio(in io.Reader, out io.Writer) { streamIn, streamOut = in, out }

func run(args ...string) ([]byte, error) {
	return runCtx(context.Background(), args...)
}

func runCtx(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := tshBin()
	if err != nil {
		return nil, fmt.Errorf("tsh not found: %w", err)
	}
	cmd := exec.CommandContext(ctx, bin, proxyArgs(args)...)
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
	cmd := exec.Command(bin, proxyArgs(args)...)
	cmd.Stdout = streamOut
	cmd.Stderr = os.Stderr
	cmd.Stdin = streamIn
	return cmd.Run()
}

// AppsList returns all apps visible to the current user.
func AppsList() ([]App, error) {
	return appsList("apps", "ls", "--format=json")
}

// AppsListAs runs `tsh -i <identityFile> apps ls --format=json`: the same
// listing, as the identity in that file rather than the user's login.
func AppsListAs(identityFile string) ([]App, error) {
	return appsList("-i", identityFile, "apps", "ls", "--format=json")
}

// appsListTimeout bounds an app listing. It runs in the daemon's model
// refresher and in `prism models`, never on a launch path, but a tsh
// stuck on the network must still give up rather than hold either.
const appsListTimeout = 10 * time.Second

func appsList(args ...string) ([]App, error) {
	ctx, cancel := context.WithTimeout(context.Background(), appsListTimeout)
	defer cancel()
	out, err := runCtx(ctx, args...)
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

// LoggedIn reports whether the status describes a profile at all.
//
// StatusJSON answers "not logged in" with a zero Status rather than an
// error, and a zero ValidUntil reads as expired, so without this a
// machine that has never logged in gets told its login "expired at
// 0001-01-01".
func (s *Status) LoggedIn() bool {
	return s != nil && s.Cluster != "" && !s.ValidUntil.IsZero()
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
		if noUsableLogin(err) {
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

// noUsableLogin reports whether `tsh status` failed because there is no
// login to report rather than because something went wrong. An expired
// profile counts: tsh exits non-zero with "Active profile expired", and
// surfacing that verbatim means prism's own messages carry tsh's
// wrapped stderr and ANSI colour codes instead of saying which cluster
// needs a login.
//
// Callers tell the two cases apart with Status.LoggedIn: a zero Status
// is "nothing usable", and IsExpired is true for it either way.
func noUsableLogin(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sign := range []string{"not logged in", "no profiles", "profile expired", "credentials have expired"} {
		if strings.Contains(msg, sign) {
			return true
		}
	}
	return false
}
