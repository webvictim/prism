# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & test commands

```bash
make                         # build bin/prism for the host
make prism-linux-amd64       # cross-compile for Linux amd64
make prism-linux-arm64       # cross-compile for Linux arm64
make prism-windows           # cross-compile bin/prism-windows-amd64.exe
make clean                   # remove bin/
go test ./...                # run all Go tests
go vet ./...                 # vet host target
GOOS=windows GOARCH=amd64 go vet ./...   # vet Windows target too
```

There is no linter wired up beyond `go vet`. End-to-end testing means
running `prism up && prism test && prism down` against a real cluster.
`prism test` exercises both backends; `prism test anthropic` and
`prism test openai` scope it to one tunnel.

## Architecture: direct cluster app tunnels

Prism tunnels traffic to two cluster-wide Teleport apps (`anthropic` and
`openai`) via `tsh proxy app` (interactive login) or `tbot` (Machine ID).
A local HTTP router on 127.0.0.1:7331 dispatches by path and applies
Bedrock-compatibility scrubbing to Anthropic requests.

```
Client → 127.0.0.1:7331 (local HTTP router + Bedrock scrubbing)
  /v1/chat/completions, /v1/models, /v1/embeddings → openai tunnel
  /v1/messages, everything else                    → anthropic tunnel
```

**tsh mode**: Two `tsh proxy app` subprocesses (anthropic + openai).
**tbot mode** (default when configured): One `tbot start` process with
two `application-tunnel` services in a generated tbot.yaml.

There are no beams, no embedded binaries, no in-beam proxy, and no
rotation. The cluster-wide apps are permanent and don't expire.

## Where the pieces live

```
cmd/prism/             local CLI (up, down, claude, codex, exec, daemon, etc.)
  daemon.go            starts tunnel services + router; branches tsh/tbot
  up.go                resolves identity, app login, picks ports, forks daemon
  claude.go            shared runToolWithPrism() for claude/codex/exec
internal/router/       local HTTP router: path dispatch + Bedrock scrubbing
internal/tunnel/       subprocess supervisor (tsh proxy app or tbot)
internal/tbot/         tbot config rendering, sidecar, bootstrap/configure
internal/identity/     polls tsh status, fires OnExpired/OnRecovered callbacks
internal/state/        ~/.config/prism/state.json persistence
internal/config/       ~/.config/prism/config.json (proxy, identity, tbot.dir)
internal/tshwrap/      thin wrappers around tsh apps/status commands
```

## State file

`~/.config/prism/state.json` stores the daemon PID and port assignments.
Much simpler than before — no beam IDs, no certificates, no bearer tokens.

## Listener ports

A running prism in tbot mode owns four 127.0.0.1 listeners:

- **Router** (default 7331): user-facing HTTP. Path-dispatches to the
  tunnels and serves `/_prism/health`.
- **Anthropic tunnel** (~7333): internal, fronted by tsh/tbot.
- **OpenAI tunnel** (~7334): internal, fronted by tsh/tbot.
- **tbot diag** (~7332): tbot's `--diag-addr`; serves `/livez` and
  `/readyz/<service>`. Only present in tbot mode.

In tsh mode there's no diag port, so three listeners total. There is no
separate control/health port — `/_prism/health` is hung off the router.

## Identity backends

- **tsh** (default): uses the user's interactive `tsh login`. Subject to
  12-24h SSO expiry. The identity watcher detects expiry and restarts the
  subprocess when the user re-logs-in.
- **tbot** (recommended for unattended use): Machine ID with bound-keypair
  join. Self-refreshing. Configure via `prism tbot bootstrap` +
  `prism tbot configure`. Resource names include the hostname
  (e.g. `prism-bot-athena`, `prism-bot-role-athena`).

## Bedrock scrubbing

The cluster's Anthropic gateway is Bedrock-backed. The local router
(`internal/router/router.go`) mutates `/v1/messages` requests:

- **Strips top-level fields**: `metadata`, `context_management`, `thinking`.
  Add new ones to `stripFields` when a new Claude Code feature breaks.
- **Caps `max_tokens`** to 8192 for non-streaming requests.
- **Short-circuits** requests with `output_config.format` (400 immediately).

## Daemon lifecycle

`prism up` forks the daemon with `Setsid: true` (Unix) so it survives
the parent terminal closing. The daemon owns the tbot/tsh subprocess(es)
and exits cleanly on SIGTERM (sent by `prism down`). On Unix, if the
daemon is SIGKILL'd, the tbot subprocess gets reparented to PID 1 and
keeps holding its port — manual cleanup is `pkill -x tbot`. On Windows,
Job Objects (`internal/tunnel/job_windows.go`) tie subprocess lifetime
to the daemon, so this isn't an issue there.

## Cross-platform notes

`proc_unix.go` / `proc_windows.go` carry platform-specific bits (signal
handling, detach attrs). Windows has no SIGTERM — `prism down` uses
`p.Kill()`.

## What not to do

- Don't add beam-related code — that architecture has been removed.
- Don't reach for `golang.org/x/sys` for things stdlib `syscall` provides.
- Don't depend on `tsh` version-specific behaviour — use `--format=json`.
