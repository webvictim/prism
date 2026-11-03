package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/webvictim/prism/internal/tshwrap"
)

// Everything prism asks tctl to do names the cluster explicitly.
//
// tctl otherwise acts on whichever tsh profile is active in the user's
// shell, which is not necessarily the cluster prism is configured for.
// Getting that wrong is expensive in a way most mistakes here are not:
// `prism tbot reset` would delete and create bots on a cluster nobody
// asked about, and the failure only shows up at the end, when tbot
// tries to join a cluster that never issued its token. Telling people
// to switch logins first is not a fix — prism's configured proxy is
// the authority, so prism points tctl at it.

// tctlArgs puts --auth-server in front of a tctl command. It is tctl's
// global flag, so it has to precede the subcommand, and it wants a
// host:port.
func tctlArgs(proxy string, args ...string) []string {
	if proxy == "" {
		return args
	}
	return append([]string{"--auth-server=" + withDefaultPort(proxy)}, args...)
}

// withDefaultPort adds Teleport's web port when an address carries
// none, since a bare hostname is a common way to write a proxy in
// prism's config but not something tctl accepts.
func withDefaultPort(addr string) string {
	if addr == "" || strings.Contains(addr, ":") {
		return addr
	}
	return addr + ":443"
}

// tctlBin resolves tctl with a message aimed at someone who has never
// heard of it.
func tctlBin() (string, error) {
	bin, err := tshwrap.LookPathStrict("tctl")
	if err != nil {
		return "", errors.New("tctl is not on PATH — it ships with Teleport: `brew install teleport`")
	}
	return bin, nil
}

// runTctl runs tctl against proxy with the user's terminal attached, so
// a per-session MFA prompt can be answered rather than hanging, while
// still returning the output for a message of prism's own.
func runTctl(proxy string, args ...string) (string, error) {
	bin, err := tctlBin()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, tctlArgs(proxy, args...)...)
	var buf bytes.Buffer
	cmd.Stdin = os.Stdin
	cmd.Stdout = &buf
	// Tee'd rather than captured: an MFA prompt is written here, and a
	// prompt nobody can see is a hang.
	cmd.Stderr = io.MultiWriter(&buf, os.Stderr)
	if err := cmd.Run(); err != nil {
		return strings.TrimSpace(buf.String()), err
	}
	return strings.TrimSpace(buf.String()), nil
}

// captureTctl runs tctl against proxy and returns stdout alone, for the
// commands prism parses. Stdin is still attached so an MFA prompt is
// answerable; stderr is kept separate so it can be quoted in an error
// without corrupting the JSON.
func captureTctl(proxy string, args ...string) ([]byte, error) {
	bin, err := tctlBin()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, tctlArgs(proxy, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdin = os.Stdin
	cmd.Stdout = &stdout
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return stdout.Bytes(), nil
}
