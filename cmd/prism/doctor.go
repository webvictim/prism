package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/config"
	"github.com/webvictim/prism/internal/logfile"
	"github.com/webvictim/prism/internal/machineid"
	"github.com/webvictim/prism/internal/state"
	"github.com/webvictim/prism/internal/tbot"
	"github.com/webvictim/prism/internal/tshwrap"
)

// `prism doctor` is written to be pasted. Someone whose prism has
// stopped working usually cannot say which part broke — the symptoms of
// an expired login, a tbot that never joined, a stale service
// definition and a cluster app that has gone away are all "it doesn't
// work" — and whoever helps them has no access to the machine. So this
// prints one self-contained report: what prism is, what it is pointed
// at, and a line per check with the fix on it.
//
// Rules it follows, because the output goes to other people:
//
//   - every check is read-only, and nothing here repairs anything;
//   - the registration secret and the contents of requests never
//     appear, only whether a secret is present;
//   - a check that cannot run says so rather than guessing, since a
//     wrong "ok" is worse than a "couldn't tell".

type checkStatus int

const (
	statusOK checkStatus = iota
	statusWarn
	statusFail
	statusInfo
)

func (s checkStatus) label() string {
	switch s {
	case statusOK:
		return "[ ok ]"
	case statusWarn:
		return "[warn]"
	case statusFail:
		return "[FAIL]"
	default:
		return "[    ]"
	}
}

type check struct {
	Name   string
	Status checkStatus
	Detail string
	// Fix is the one command or action that addresses this line.
	Fix string
}

// report accumulates checks so the verdict can count them.
type report struct {
	checks []check
	out    io.Writer
}

func (r *report) add(c check) {
	r.checks = append(r.checks, c)
	fmt.Fprintf(r.out, "%s %-18s %s\n", c.Status.label(), c.Name, c.Detail)
	if c.Fix != "" {
		fmt.Fprintf(r.out, "       → %s\n", c.Fix)
	}
}

func (r *report) ok(name, detail string) { r.add(check{Name: name, Status: statusOK, Detail: detail}) }
func (r *report) info(name, detail string) {
	r.add(check{Name: name, Status: statusInfo, Detail: detail})
}
func (r *report) warn(name, detail, fix string) {
	r.add(check{Name: name, Status: statusWarn, Detail: detail, Fix: fix})
}
func (r *report) fail(name, detail, fix string) {
	r.add(check{Name: name, Status: statusFail, Detail: detail, Fix: fix})
}

func (r *report) count(s checkStatus) int {
	n := 0
	for _, c := range r.checks {
		if c.Status == s {
			n++
		}
	}
	return n
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	noRequest := fs.Bool("no-request", false, "skip the live request through the router")
	model := fs.String("model", "", "Anthropic model for the live request (default: let the gateway choose)")
	_ = fs.Parse(args)

	out := os.Stdout
	r := &report{out: out}

	hostname, _ := os.Hostname()
	cfg, cfgErr := config.Load()
	if cfg == nil {
		cfg = &config.Config{}
	}
	s, _ := state.Load()

	fmt.Fprintln(out, "prism doctor")
	fmt.Fprintln(out, repeat("─", 60))
	self, _ := os.Executable()
	fmt.Fprintf(out, "prism %s · %s/%s · host %s · %s\n", version, runtime.GOOS, runtime.GOARCH,
		hostname, time.Now().Format(time.RFC3339))
	fmt.Fprintf(out, "binary %s\n", self)
	// Shortened on purpose: the report is written to be pasted to
	// somebody else, and all anyone needs from a machine id is whether
	// two of them differ.
	if id, err := machineid.Current(); err == nil {
		fmt.Fprintf(out, "machine %s\n", oneLine(truncate(id, 12)))
	}
	logDir, _ := state.DaemonLogDir()
	fmt.Fprintf(out, "logs %s\n\n", logfile.LatestPath(logDir))

	if cfgErr != nil {
		r.fail("config", fmt.Sprintf("could not read prism's config: %v", cfgErr),
			"check ~/.config/prism/config.json is valid JSON")
	}

	checkService(r)
	checkDaemon(r, s)
	identity := cfg.Identity
	if s != nil && s.IdentitySource != "" {
		identity = s.Identity()
	}
	if identity == "" {
		identity = state.IdentitySourceTSH
	}
	r.info("identity mode", identity)

	switch identity {
	case state.IdentitySourceTbot:
		dir := tbotDirFor(cfg, s)
		checkTbot(r, dir, s)
		checkTbotProcesses(r, dir, s)
	default:
		checkTsh(r, cfg.Proxy)
	}

	if !*noRequest {
		checkGateway(r, s, *model)
	}

	fmt.Fprintln(out, repeat("─", 60))
	failed, warned := r.count(statusFail), r.count(statusWarn)
	switch {
	case failed == 0 && warned == 0:
		fmt.Fprintln(out, "prism doctor: everything checks out.")
		return nil
	case failed == 0:
		fmt.Fprintf(out, "prism doctor: %d warning(s), nothing broken.\n", warned)
		return nil
	}
	fmt.Fprintf(out, "prism doctor: %d check(s) failed, %d warning(s).\n", failed, warned)
	if identity == state.IdentitySourceTbot {
		fmt.Fprintln(out, "If the failures are about the bot or the tunnel, `prism tbot reset` rebuilds")
		fmt.Fprintln(out, "this machine's identity from scratch and checks it works.")
	}
	return fmt.Errorf("%d check(s) failed — see the report above", failed)
}

// checkService covers the service definition, including the staleness
// the repair in service_path.go acts on: doctor reports, never repairs.
func checkService(r *report) {
	if !isServiceManaged() {
		r.info("service", "not installed (prism runs as a background process)")
		return
	}
	detail := serviceManagedLabel()
	if serviceIsActive() {
		detail += ", loaded"
	} else {
		detail += ", NOT loaded"
	}
	if st := serviceStale(); st.stale() {
		fix := "`prism up` repairs it, or `prism install` rewrites it now"
		if st.Breaks {
			r.fail("service", detail+" — "+st.Reason, fix)
		} else {
			r.warn("service", detail+" — "+st.Reason, fix)
		}
		return
	}
	if !serviceIsActive() {
		r.warn("service", detail, "run `prism up`")
		return
	}
	r.ok("service", detail)
}

func checkDaemon(r *report, s *state.State) {
	if s == nil {
		r.fail("daemon", "no session recorded", "run `prism up`")
		return
	}
	detail := fmt.Sprintf("router 127.0.0.1:%d", s.LocalPort)
	if s.DaemonPID != 0 {
		detail += fmt.Sprintf(", pid %d", s.DaemonPID)
		if !processAlive(s.DaemonPID) {
			detail += " (not running)"
		}
	}
	if !portResponds(s.LocalPort) {
		r.fail("daemon", detail+" — nothing is listening", "run `prism up` (or `prism down && prism up`)")
		return
	}
	if err := routerHealth(s.LocalPort); err != nil {
		r.fail("daemon", detail+" — "+err.Error(), "check `prism logs`")
		return
	}
	r.ok("daemon", detail+", health ok")
}

func checkTsh(r *report, proxy string) {
	// tsh is already aimed at prism's cluster (see applyConfigEnv), so
	// this reports the login prism would actually use, not whichever
	// one the shell is on.
	login := "run `tsh login --proxy=" + withDefaultPort(cmp.Or(proxy, "<proxy>")) + "`"
	st, err := tshwrap.StatusJSON()
	switch {
	case err != nil:
		r.fail("tsh login", err.Error(), login)
	case !st.LoggedIn():
		// Not the same as expired: StatusJSON reports "nobody is
		// logged in" as a zero Status, whose zero expiry would
		// otherwise read as "expired at 0001-01-01".
		r.fail("tsh login", "not logged in"+forCluster(proxy), login)
	case st.IsExpired():
		r.fail("tsh login", fmt.Sprintf("expired at %s", st.ValidUntil.Local().Format(time.RFC3339)), login)
	default:
		r.ok("tsh login", fmt.Sprintf("%s on %s, %s left", st.Username, st.Cluster,
			time.Until(st.ValidUntil).Round(time.Minute)))
	}
}

func forCluster(proxy string) string {
	if proxy == "" {
		return ""
	}
	return " to " + proxy
}

// checkTbot is the reason this command exists: every way a Machine ID
// setup can be broken, named, in the order that makes the next one
// meaningful.
// tbotDirFor is the directory prism would use right now: the running
// session's, else the configured one.
func tbotDirFor(cfg *config.Config, s *state.State) string {
	if s != nil && s.TbotDir != "" {
		return s.TbotDir
	}
	if cfg != nil {
		return cfg.TbotDir
	}
	return ""
}

func checkTbot(r *report, dir string, s *state.State) {
	if _, err := tshwrap.LookPathStrict("tbot"); err != nil {
		r.fail("tbot binary", "not on PATH", "install Teleport: `brew install teleport`")
	}

	if dir == "" {
		r.fail("tbot dir", "identity is tbot but tbot.dir is not set",
			"run `prism tbot reset` to set this machine up from scratch")
		return
	}
	if problems := tbot.Validate(dir); len(problems) > 0 {
		r.fail("tbot dir", dir+" is incomplete: "+strings.Join(problems, "; "),
			"run `prism tbot reset`")
		return
	}
	r.ok("tbot dir", dir)

	sidecar, err := tbot.LoadSidecar(dir)
	if err != nil {
		r.fail("tbot config", err.Error(), "run `prism tbot reset`")
		return
	}

	// The clone check. Deliberately before anything that talks to the
	// cluster: if this fails, every later failure is explained by it.
	current, idErr := machineid.Current()
	switch {
	case idErr != nil:
		r.info("machine binding", "cannot read this machine's id: "+idErr.Error())
	case sidecar.MachineID == "":
		r.info("machine binding", "not recorded yet (recorded on the next prism start)")
	default:
		if err := tbot.CheckMachine(dir, current); err != nil {
			r.fail("machine binding", "this identity was issued to a different machine",
				"run `prism tbot reset` — this machine needs its own bot")
		} else {
			r.ok("machine binding", "issued to this machine")
		}
	}

	// Names are hostname-scoped, so a name that doesn't match this host
	// means the machine was renamed (or the directory was copied). It
	// still works — the cluster resources exist under the old name —
	// so this is a warning, not a failure.
	local := tbot.LocalNames()
	if sidecar.BotName != "" && sidecar.BotName != local.Bot {
		r.warn("bot name", fmt.Sprintf("%s, but this machine would use %s", sidecar.BotName, local.Bot),
			"`prism tbot reset` moves this machine onto "+local.Bot)
	} else {
		r.ok("bot name", sidecar.BotName)
	}

	if sidecar.RegistrationSecret == "" {
		r.fail("join secret", "not set", "run `prism tbot reset`")
	} else {
		r.ok("join secret", "set")
	}

	// Reachability of the proxy, separately from anything tbot does
	// with it: a Beams cluster that has been torn down looks exactly
	// like a broken bot from inside prism.
	if sidecar.ProxyServer == "" {
		r.fail("proxy", "no proxy recorded", "run `prism tbot reset --proxy <host:port>`")
	} else if err := dialProxy(sidecar.ProxyServer); err != nil {
		r.fail("proxy", sidecar.ProxyServer+" is not reachable: "+err.Error(),
			"check the cluster still exists and you are on the right network")
	} else {
		r.ok("proxy", sidecar.ProxyServer+" reachable")
	}

	// tbot's own view of itself. The body of a non-ready /readyz is the
	// single most useful line in this whole report when a tunnel won't
	// come up, so it is printed verbatim.
	if s == nil || s.TbotDiagPort == 0 {
		r.info("tbot health", "no diag endpoint (prism not running in tbot mode right now)")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := tbot.ProbeDiag(ctx, s.TbotDiagPort)
	switch {
	case !h.Reachable:
		// Worth saying, because a doctor run moments after `prism up`
		// catches tbot before it has bound anything.
		r.fail("tbot health", fmt.Sprintf("diag port 127.0.0.1:%d is not answering (%v)", s.TbotDiagPort, h.Err),
			"if prism only just started, re-run in a few seconds; otherwise check `prism logs`, then `prism tbot reset`")
		reportLastTbotError(r)
	case !h.Live:
		r.fail("tbot health", "tbot is running but not live", "check `prism logs`")
		reportLastTbotError(r)
	case !h.Ready:
		r.fail("tbot health", "tunnel not ready: "+oneLine(h.ReadyBody),
			"usually a join failure — `prism tbot reset` rebuilds the identity")
		reportLastTbotError(r)
	case h.Degraded:
		r.warn("tbot health", "tunnel ready but degraded: "+oneLine(h.DegradedReason),
			"credentials may expire — watch `prism logs`")
	default:
		r.ok("tbot health", "live and ready")
	}
}

// reportLastTbotError adds the most recent error tbot wrote to the
// daemon log. The diag endpoint only ever says "initializing", so this
// is where the actual reason lives — "could not lookup signer for
// public key …" for a missing bound keypair, for instance. It is the
// line a remote helper most wants to see, and without it the report
// says only that the tunnel isn't ready.
//
// Only tbot's own error lines are quoted, never the surrounding
// request log.
func reportLastTbotError(r *report) {
	logDir, err := state.DaemonLogDir()
	if err != nil {
		return
	}
	if msg := lastTbotError(logfile.LatestPath(logDir)); msg != "" {
		r.info("last tbot error", msg)
	}
}

func lastTbotError(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	// The tail is enough, and a daemon log can be large.
	const window = 64 << 10
	if fi.Size() > window {
		if _, err := f.Seek(-window, io.SeekEnd); err != nil {
			return ""
		}
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	best := ""
	for line := range strings.SplitSeq(string(body), "\n") {
		if !strings.Contains(line, "tbot:") {
			continue
		}
		if strings.Contains(line, "Original Error:") || strings.Contains(line, "ERRO") {
			best = line
		}
	}
	if best == "" {
		return ""
	}
	// Keep the message, drop prism's timestamp and tbot's own prefixes.
	if _, after, found := strings.Cut(best, "Original Error: "); found {
		best = after
	} else if _, after, found := strings.Cut(best, "tbot: "); found {
		best = after
	}
	return oneLine(best)
}

// checkTbotProcesses counts the tbot processes running against
// prism's own tbot.yaml.
//
// Parentage is not the test, even though the stray case is a daemon
// that was SIGKILL'd and left tbot reparented to PID 1: a
// service-managed daemon's pid isn't recorded anywhere, and a machine
// may legitimately run other tbots from other configs (a
// configuration-management one, say) which are none of prism's
// business. The config path is what identifies prism's.
func checkTbotProcesses(r *report, dir string, s *state.State) {
	if runtime.GOOS == "windows" || dir == "" {
		return
	}
	procs, err := tbotProcesses()
	if err != nil {
		return
	}
	wanted := tbot.TbotYAMLPath(dir)
	var mine, others []string
	for pid, cmdline := range procs {
		if strings.Contains(cmdline, wanted) {
			mine = append(mine, strconv.Itoa(pid))
			continue
		}
		others = append(others, strconv.Itoa(pid))
	}
	sort.Strings(mine)
	sort.Strings(others)

	running := s != nil && portResponds(s.LocalPort)
	switch {
	case len(mine) > 1:
		// Two tbots sharing one storage directory renew the same bound
		// keypair against each other, which is the same hazard as a
		// cloned machine.
		r.fail("tbot process", "more than one tbot is running on prism's config: pid "+strings.Join(mine, ", "),
			"stop the extras (`kill "+mine[0]+"`), then `prism down && prism up`")
	case len(mine) == 1 && !running:
		r.warn("tbot process", "pid "+mine[0]+" is still running but prism is not — it keeps holding the tunnel port",
			"clean up with `pkill -x tbot`, then `prism up`")
	case len(mine) == 1:
		r.ok("tbot process", "pid "+mine[0])
	case running:
		r.fail("tbot process", "none running on prism's config", "check `prism logs`, then `prism tbot reset`")
	default:
		r.info("tbot process", "none running")
	}
	if len(others) > 0 {
		// Running tbot for other things alongside prism is normal and
		// fine. It is listed because it explains a lot when someone is
		// debugging renewals remotely, and as an info line it counts
		// towards neither the warnings nor the failures.
		r.info("other tbots", "pid "+strings.Join(others, ", ")+" — another tbot config, nothing to do with prism")
	}
}

// checkGateway is the end-to-end answer: one small request through the
// router. Everything else can pass while this fails (an app that has
// been removed from the cluster, a gateway with no credit), and that
// distinction is what a user cannot make on their own.
//
// A gateway with no default model gets a warning, not a failure: the
// request reached it, so nothing here is broken, but no inference ran
// either, and a provider that refuses every model looks exactly like that.
func checkGateway(r *report, s *state.State, model string) {
	if s == nil || !portResponds(s.LocalPort) {
		r.info("inference", "not checked (prism is not running)")
		return
	}
	served, err := probeInference(s.LocalPort, model, 60*time.Second)
	if errors.Is(err, errNoDefaultModel) {
		r.warn("inference", "reached the gateway, but it has no default model, so no request was completed",
			"`prism doctor --model <id>` sends a real request")
		return
	}
	if err != nil {
		r.fail("inference", err.Error(), "the lines above usually say why; `prism logs` has the request")
		return
	}
	r.ok("inference", "the gateway answered as "+served)
}

// errNoDefaultModel is a gateway that refused an unnamed model because
// it has no fallback configured to pick instead. The request still went
// through the router, the tunnel and Teleport's app access before being
// turned away, so it proves the identity works — just not inference.
var errNoDefaultModel = errors.New("the gateway has no default model, so it can't be left to choose one")

// gatewayWantsModel reports whether a reply is the gateway refusing an
// omitted model. Teleport's LLM proxy only lets an unknown name through
// to its fallback; with no fallback configured, an omitted model is just
// an unknown one, refused as `requested model "" is not supported`.
func gatewayWantsModel(status int, body []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	var reply struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &reply) == nil &&
		reply.Error.Message == `requested model "" is not supported`
}

// probeInference sends one short request through the router and returns
// the model the gateway says served it. With model empty, none is named:
// a gateway with a fallback picks its own default and reports it, which
// is also how `prism test` avoids hardcoding model names. A gateway with
// no fallback answers errNoDefaultModel instead.
func probeInference(port int, model string, timeout time.Duration) (string, error) {
	req := map[string]any{
		"max_tokens": 16,
		"messages":   []map[string]string{{"role": "user", "content": "say ok"}},
	}
	if model != "" {
		req["model"] = model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/v1/messages", port)
	hreq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(hreq)
	if err != nil {
		return "", fmt.Errorf("the request never completed: %w", err)
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if gatewayWantsModel(resp.StatusCode, reply) {
		return "", errNoDefaultModel
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the gateway answered %d: %s", resp.StatusCode, oneLine(string(reply)))
	}
	served := modelFromReply(reply)
	if served == "" {
		served = "an unnamed model"
	}
	return served, nil
}

// waitForInference retries the probe until the gateway answers or the
// budget runs out, returning the last failure.
//
// tbot joins the cluster and binds the tunnel a few seconds *after* the
// router starts listening, so the first request after a restart
// routinely comes back 502. Waiting through that is right where the
// point is to end up working — `prism tbot reset` — and wrong in
// `prism doctor`, which reports the state it finds.
//
// errNoDefaultModel returns at once: it is the gateway's configuration,
// not a tunnel still coming up, so no amount of waiting changes it.
func waitForInference(port int, budget time.Duration) (string, error) {
	deadline := time.Now().Add(budget)
	for attempt := 1; ; attempt++ {
		model, err := probeInference(port, "", time.Minute)
		if err == nil || errors.Is(err, errNoDefaultModel) {
			return model, err
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w (after %d attempts over %s)", err, attempt, budget)
		}
		time.Sleep(2 * time.Second)
	}
}

func routerHealth(port int) error {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/_prism/health", port))
	if err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("health check answered %d", resp.StatusCode)
	}
	return nil
}

func dialProxy(addr string) error {
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}

// tbotProcesses maps the pid of every running tbot to its command
// line. `pgrep -x` finds them (the full-command form differs between
// macOS and Linux, so it is avoided), and one `ps` reads the command
// lines back.
func tbotProcesses() (map[int]string, error) {
	out, err := exec.Command("pgrep", "-x", "tbot").Output()
	if err != nil {
		// Exit 1 means nothing matched, which is not an error.
		return nil, nil
	}
	var pids []string
	for field := range strings.FieldsSeq(string(out)) {
		if _, err := strconv.Atoi(field); err == nil {
			pids = append(pids, field)
		}
	}
	if len(pids) == 0 {
		return nil, nil
	}
	out, err = exec.Command("ps", "-o", "pid=,command=", "-p", strings.Join(pids, ",")).Output()
	if err != nil {
		return nil, err
	}
	procs := make(map[int]string, len(pids))
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		field, rest, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		pid, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		procs[pid] = rest
	}
	return procs, nil
}

// truncate shortens s to n characters, marking that it did.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// oneLine flattens a message for a single report line, keeping it short
// enough to read in a pasted transcript.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
