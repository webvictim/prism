package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/webvictim/prism/internal/tshwrap"
)

// checkTbotVersion reports the tbot prism runs against the cluster it
// joins. Version skew is invisible from inside prism — the symptoms are
// whatever the older or newer tbot happens to get wrong — so the report
// names both, and says when they can't work together:
//
//   - below the cluster's min_client_version: the cluster's own statement
//     of what it accepts, so a failure;
//   - a newer major than the cluster: Teleport supports clients up to the
//     server's version, not past it;
//   - the running tbot differs from the installed one: `brew upgrade
//     teleport` doesn't touch a tbot that's already running, and nothing
//     else would say so.
//
// Read-only and unauthenticated, as doctor must be: `tbot version`, the
// proxy's public /webapi/find, and the daemon log.
func checkTbotVersion(r *report, proxy, logPath string, running bool) {
	bin, err := tshwrap.LookPathStrict("tbot") // as the daemon finds it
	if err != nil {
		return // the tbot binary line has already failed
	}
	installed, err := tbotBinaryVersion(bin)
	if err != nil {
		r.warn("tbot version", "could not read `tbot version`: "+err.Error(), "check that "+bin+" runs")
		return
	}
	var cluster, minClient string
	if proxy != "" {
		cluster, minClient, _ = clusterVersions(proxy)
	}
	ranAs := ""
	if running {
		ranAs = runningTbotVersion(logPath)
	}
	status, detail, fix := judgeTbotVersion(installed, bin, cluster, minClient, ranAs)
	switch status {
	case statusFail:
		r.fail("tbot version", detail, fix)
	case statusWarn:
		r.warn("tbot version", detail, fix)
	default:
		r.ok("tbot version", detail)
	}
}

// judgeTbotVersion is checkTbotVersion's verdict. cluster, minClient and
// ranAs may be empty when unknown, and then aren't judged.
func judgeTbotVersion(installed, bin, cluster, minClient, ranAs string) (checkStatus, string, string) {
	detail := "v" + installed + " (" + bin + ")"
	if cluster != "" {
		detail += ", cluster v" + cluster
	}
	switch {
	case minClient != "" && versionLess(installed, minClient):
		return statusFail, detail + " — below the cluster's minimum client version v" + minClient,
			"upgrade Teleport (`brew upgrade teleport`), then `prism down && prism up`"
	case cluster != "" && versionMajor(installed) > versionMajor(cluster):
		return statusWarn, detail + " — a newer major version than the cluster, which Teleport doesn't support",
			"install a tbot no newer than v" + cluster
	case ranAs != "" && ranAs != installed:
		return statusWarn, detail + ", but the running tbot is v" + ranAs,
			"`prism down && prism up` starts the installed one"
	}
	return statusOK, detail, ""
}

var tbotVersionRe = regexp.MustCompile(`\bv(\d+\.\d+\.\d+\S*)`)

// tbotBinaryVersion runs `tbot version`, which prints e.g.
// "Teleport v18.11.7 git:v18.11.7-0-ga2db690b go1.26.10".
func tbotBinaryVersion(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", err
	}
	return parseTbotVersion(string(out))
}

func parseTbotVersion(out string) (string, error) {
	m := tbotVersionRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unrecognised output %q", oneLine(out))
	}
	return m[1], nil
}

// clusterVersions reads the proxy's public /webapi/find — the same call
// tbot makes before joining — for the cluster's version and the oldest
// client it accepts.
func clusterVersions(proxy string) (server, minClient string, err error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://" + withDefaultPort(proxy) + "/webapi/find")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("/webapi/find answered %d", resp.StatusCode)
	}
	var find struct {
		ServerVersion    string `json:"server_version"`
		MinClientVersion string `json:"min_client_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&find); err != nil {
		return "", "", err
	}
	return find.ServerVersion, find.MinClientVersion, nil
}

var runningTbotRe = regexp.MustCompile(`Initializing tbot version\.teleport:(\S+)`)

// runningTbotVersion is the version the running tbot reported when it
// started, from the last such line in the daemon log, or "" when the log
// doesn't have one (the daemon started on an earlier day).
func runningTbotVersion(logPath string) string {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return ""
	}
	m := runningTbotRe.FindAllSubmatch(data, -1)
	if len(m) == 0 {
		return ""
	}
	return string(m[len(m)-1][1])
}

// versionLess compares the numeric major.minor.patch of two Teleport
// versions, ignoring any suffix: the cluster's min_client_version is
// spelled like 17.0.0-aa, and a 17.0.0 client must pass it.
func versionLess(a, b string) bool {
	x, y := versionTriple(a), versionTriple(b)
	for i := range x {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return false
}

func versionMajor(v string) int { return versionTriple(v)[0] }

func versionTriple(v string) [3]int {
	var t [3]int
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		t[i], _ = strconv.Atoi(part)
	}
	return t
}
