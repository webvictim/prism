# Changelog

All notable changes to prism, newest first. Dates are release dates.

Each version links to its full commit range. Upgrading generally means
`prism down && prism up` so the daemon picks up the new binary — entries
call out when more than that is needed.

## [v0.1.19] — 2026-09-08

### Added

- **chat/completions → Responses translation.** Newer gateways serve OpenAI
  models only on `/v1/responses` and reject every model on
  `/v1/chat/completions` with `model ... isn't supported on this route`. Prism
  now translates those requests, streaming included, so OpenAI-compatible
  clients that only speak chat/completions work again — MacWhisper, Teleport
  session summaries, and anything else with an "OpenAI-compatible endpoint"
  box. `/v1/responses` is passed through untouched, so Codex is unaffected.
- **`openai_chat_completions_shim` config flag.** On by default.
  `prism config set openai_chat_completions_shim false` relays
  `/v1/chat/completions` byte-for-byte instead, so a current prism still works
  against an older gateway.
- **`prism test` flags**: `--format anthropic|openai-responses|openai-completions`
  picks the wire format, `--model` picks a model, `--stream` exercises SSE.
  Omitting `--model` sends no model field at all, so the gateway picks its own
  default and prism reports which one it served.
- Translated requests are visible in `prism logs`:
  `POST /v1/chat/completions [-> /v1/responses] 200 ...`. Errors relayed from
  the gateway are logged too, so a transient upstream 5xx is no longer
  indistinguishable from one of prism's own.

### Changed

- **No model names are compiled into prism.** The hardcoded reasoning-model
  list added in v0.1.8 is gone — its prefix matching had silently stopped
  working once model ids gained an `openai.` vendor prefix. Prism now forwards
  what the client sent, reads the rejected parameter's name out of the
  gateway's own error, drops it and retries, remembering the rejection per
  model for the life of the daemon. Deliberately not persisted to disk: a
  stale cache would keep stripping a parameter after the gateway started
  accepting it again.
- **`prism pi config` mirrors Pi's model catalog.** Pi's overrides match by
  model id, so it now reads Pi's own catalog and writes it back with only
  `baseUrl` repointed at prism. Every model Pi knows routes through prism, and
  each entry keeps its cost, context-window and compat metadata.
  `--anthropic-model` / `--openai-model` narrow the rewrite to one id. Re-run
  after `pi update` refreshes the catalog.

### Fixed

- Usage records showed `?` instead of a model whenever the client omitted
  `model` or the gateway aliased it — the request's model was overwriting the
  response's. The served model now wins.
- Direct `/v1/responses` traffic recorded **no** usage at all: the parser only
  understood chat/completions' `prompt_tokens`, not the Responses API's
  `input_tokens`. Codex traffic through prism was invisible to `prism usage`.
- Tool calling isn't translated; a request carrying `tools` now gets a clear
  400 pointing at `/v1/responses` rather than a confusing gateway error.

## [v0.1.18] — 2026-08-20

### Fixed

- Forward-proxy mode served expired leaf certificates after 24 hours or a
  laptop wake — the MITM cert cache had no expiry check. Certs are now
  re-issued on demand with a 5-minute buffer.

## [v0.1.17] — 2026-08-19

### Added

- Forward-proxy mode now does request logging and token-usage tracking, so
  `prism logs` and `prism usage` cover Remote Control traffic the same way
  they cover the router.

### Fixed

- README corrections: Remote Control section rewritten, stale references and
  example hostnames fixed.

## [v0.1.16] — 2026-08-19

### Fixed

- **Headless Remote Control.** The Remote Control bridge client doesn't issue
  `CONNECT` when `HTTPS_PROXY` is set — it sends absolute-form plain-HTTP
  requests straight to the proxy. Prism treated those as ordinary local
  requests and path-dispatched them to the tunnel, where the gateway's 404
  made the CLI report "Remote Control environments are not available for your
  account". Interactive sessions were unaffected because they use `CONNECT`.

### Changed

- Module renamed to `github.com/webvictim/prism`.

## [v0.1.15] — 2026-08-18

### Added

- **Forward-proxy mode for Claude Code Remote Control.** Claude Code
  v2.1.196+ disables Remote Control when `ANTHROPIC_BASE_URL` points at a
  non-Anthropic host. Enable with
  `prism config set claude_forward_proxy_mode true` and prism sets
  `HTTPS_PROXY` instead, MITM'ing only `api.anthropic.com` model calls and
  blind-tunnelling everything else (Remote Control, telemetry, MCP) with
  credentials intact.
- Tests for usage capture and aggregation, state, and config.

### Fixed

- Forward-proxy 400s from the Bedrock gateway, which rejects `cache_control`
  objects carrying the `scope` key that Claude Code adds when it believes it's
  talking directly to `api.anthropic.com`. Scrubbing is now shared between the
  router and the MITM proxy (`internal/scrub`) so requests reach the gateway
  in an identical shape in both modes. Earlier header-stripping attempts had
  been chasing the wrong variable.
- Panic on startup when forward-proxy mode was disabled (nil handler).

## [v0.1.14] — 2026-07-31

### Fixed

- **tbot certificates expired before renewal.** Teleport caps bot certs at 12h
  (`DefaultBotMaxSessionTTL`) but prism requested 24h with a 23h renewal
  interval, so the server silently capped them and every cycle forced a
  rejoin. Now 12h certs with an 8h renewal interval. `prism status` warns
  existing installs still carrying the old interval — run
  `prism down && prism up`.

### Added

- Test suite for the router, log rotation, and path expansion; release builds
  now run tests first.

## [v0.1.13] — 2026-07-29

### Fixed

- `~/` in `tbot.dir` paths is expanded, for both `--dir` flags and
  `prism config set tbot.dir`.
- `prism tbot bootstrap` sets `tbot.dir` in config automatically.

## [v0.1.12] — 2026-07-28

### Added

- **`prism pi config`** writes `~/.pi/agent/models.json` pointing at the local
  router. Pi resolves base URLs from its own registry and ignores
  `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL`, so it needs this file.
- `/v1/responses` is routed to the OpenAI backend.
- `prism env` exports the full set of environment variables a live beam sets.

### Fixed

- **401s from the gateway.** The router now strips client `Authorization` and
  `X-Api-Key` headers before forwarding; the tunnel authenticates by mTLS, so
  the dummy tokens from those env vars were being rejected.
- Daemon logs moved into `~/.config/prism/logs/` to keep the config directory
  clean.

## [v0.1.11] — 2026-07-16

### Fixed

- A stale macOS LaunchAgent plist (wrong binary path, or missing crash-log
  path) is now detected and reinstalled automatically on `prism up`.
- Panics and early fatal errors — raised before the rotating log writer
  exists — go to `crash.log` instead of being discarded.

## [v0.1.10] — 2026-07-16

### Added

- **Daily log rotation.** The daemon writes dated log files
  (`daemon-YYYY-MM-DD.log`) and gzips older ones in the background. On first
  start after upgrade the legacy `daemon.log` is compressed to
  `daemon-legacy.log.gz`. The daemon now owns its log file rather than relying
  on inherited stdout/stderr.

## [v0.1.9] — 2026-06-30

### Fixed

- tbot startup timeout raised from 10s to 60s; 10s was too aggressive on
  high-latency connections where the initial identity join takes longer.

## [v0.1.8] — 2026-06-15

### Fixed

- Non-default `temperature` was rejected by OpenAI reasoning models, breaking
  clients like MacWhisper that send `temperature=0`. Prism stripped the field
  for a known list of models.

  *Superseded in v0.1.19, which discovers this from the gateway's error
  message instead of a hardcoded list.*

## [v0.1.7] — 2026-06-15

### Changed

- Usage data is partitioned into monthly files
  (`usage/usage-YYYY-MM.jsonl`); legacy `usage.jsonl` is still read. Token
  counts also appear in the daemon log after each request.

## [v0.1.6] — 2026-06-14

### Added

- **Token usage tracking.** The router captures usage from streaming and
  non-streaming responses on both backends, and
  `prism usage [--week|--all|--json]` summarises consumption by model and by
  Teleport proxy.

### Fixed

- `max_tokens` is renamed to `max_completion_tokens` on
  `/v1/chat/completions`, which newer models require and older ones accept.

## [v0.1.5] — 2026-06-11

### Fixed

- `prism tbot bootstrap` used the cluster name as the proxy address. It now
  parses `profile_url` from `tsh status`, which carries the real proxy FQDN.

## [v0.1.4] — 2026-06-08

### Added

- GitHub Actions release workflow building Linux, macOS and Windows binaries.

### Fixed

- `prism claude` / `codex` / `exec` reported the daemon as "not running" when
  it was managed by systemd or launchd, causing a redundant startup.

## [v0.1.3] — 2026-06-06

### Added

- **macOS LaunchAgent support** for `prism install`, matching the systemd
  integration. The plist captures the user's `PATH` so `tsh` and `tbot` are
  found at runtime.

### Fixed

- **Windows**: `tsh.exe` in the current directory no longer trips Go's
  `ErrDot` rejection, and background subprocesses no longer flash or leave
  console windows open.
- launchd integration switched to the modern `bootstrap`/`bootout` API so it
  persists across reboots, with a port-based liveness fallback for SSH
  sessions that can't query launchd's GUI domain.

## [v0.1.2] — 2026-06-06

### Added

- **systemd user service support** via `prism install` / `prism uninstall`. The
  daemon survives logout and restarts on crash; `prism up` and `prism down`
  delegate to `systemctl` once installed. `prism logs` and `prism status` read
  from `journalctl` when systemd-managed.

## [v0.1.1] — 2026-06-04

### Added

- **Health-based tbot restart.** Prism polls both the per-service and rolled-up
  tbot readiness endpoints, reports `degraded` in `prism status` when the
  tunnel is up but infrastructure services are failing, and after ~60s of
  sustained failure restarts the subprocess rather than leaving it wedged.

## [v0.1.0] — 2026-06-01

First release. Routes local AI traffic through cluster-wide Teleport apps
(`anthropic` and `openai`) via `tsh proxy app` or tbot Machine ID, behind a
local HTTP router that dispatches by path and applies Bedrock-compatibility
scrubbing. Includes `prism up`/`down`/`status`/`env`/`logs`/`test`,
`prism claude`/`codex`/`exec`, tbot onboarding, and Homebrew installation.

[v0.1.19]: https://github.com/webvictim/prism/compare/v0.1.18...v0.1.19
[v0.1.18]: https://github.com/webvictim/prism/compare/v0.1.17...v0.1.18
[v0.1.17]: https://github.com/webvictim/prism/compare/v0.1.16...v0.1.17
[v0.1.16]: https://github.com/webvictim/prism/compare/v0.1.15...v0.1.16
[v0.1.15]: https://github.com/webvictim/prism/compare/v0.1.14...v0.1.15
[v0.1.14]: https://github.com/webvictim/prism/compare/v0.1.13...v0.1.14
[v0.1.13]: https://github.com/webvictim/prism/compare/v0.1.12...v0.1.13
[v0.1.12]: https://github.com/webvictim/prism/compare/v0.1.11...v0.1.12
[v0.1.11]: https://github.com/webvictim/prism/compare/v0.1.10...v0.1.11
[v0.1.10]: https://github.com/webvictim/prism/compare/v0.1.9...v0.1.10
[v0.1.9]: https://github.com/webvictim/prism/compare/v0.1.8...v0.1.9
[v0.1.8]: https://github.com/webvictim/prism/compare/v0.1.7...v0.1.8
[v0.1.7]: https://github.com/webvictim/prism/compare/v0.1.6...v0.1.7
[v0.1.6]: https://github.com/webvictim/prism/compare/v0.1.5...v0.1.6
[v0.1.5]: https://github.com/webvictim/prism/compare/v0.1.4...v0.1.5
[v0.1.4]: https://github.com/webvictim/prism/compare/v0.1.3...v0.1.4
[v0.1.3]: https://github.com/webvictim/prism/compare/v0.1.2...v0.1.3
[v0.1.2]: https://github.com/webvictim/prism/compare/v0.1.1...v0.1.2
[v0.1.1]: https://github.com/webvictim/prism/compare/v0.1.0...v0.1.1
[v0.1.0]: https://github.com/webvictim/prism/releases/tag/v0.1.0
