# prism

Route local AI tools (Claude Code, Codex, anything that talks to the
Anthropic or OpenAI API) through your Teleport cluster's LLM gateways,
powered by [Teleport Beams](https://goteleport.com/beams/).

*(The name? A prism redirects beams.)*

```
prism claude        # Claude Code, routed via Teleport
prism codex         # Codex CLI, routed via Teleport
prism exec <cmd>    # any other tool, with prism env vars set
```

No API keys to manage, no shared secrets, no `.env` files. Auth is
your Teleport identity.

---

## Prerequisites

- A **Teleport cluster with Beams enabled**. Beams provisions
  cluster-wide apps (`anthropic` and `openai`) that proxy traffic to
  the upstream LLM APIs. Prism tunnels to these apps — it doesn't talk
  to Anthropic or OpenAI directly.
- **`tsh` on PATH** and a valid `tsh login` to the Beams cluster.
  Prism uses `tsh proxy app` (or `tbot` for unattended use) to
  establish authenticated tunnels to the Beams apps.
- **Go ≥ 1.24** (if building from source).

---

## Install

### Homebrew (macOS / Linux)

```bash
brew install webvictim/tap/prism
```

### From source

```bash
git clone https://github.com/webvictim/prism.git
cd prism
make
sudo make install              # or: make install PREFIX=$HOME/.local
```

Requires Go ≥ 1.24.

---

## Quick start (tsh, the "just try it" path)

If you already have an interactive `tsh login` to a Beams-enabled
cluster, you can be running in 30 seconds:

```bash
tsh login --proxy=<your-cluster>.beams.run:443
prism config set proxy <your-cluster>.beams.run:443
prism claude
```

Prism logs into both cluster apps, starts a local daemon, and execs
`claude` with the right env vars. Your tsh session expires after
12-24h; when it does, run `tsh login` again and prism's daemon
auto-recovers.

**For unattended use** (overnight agents, long jobs, CI), use tbot
instead — it never expires. See the next section.

---

## Recommended setup: tbot (no token expiry, no re-login)

`tbot` is Teleport's Machine ID daemon. Prism runs it as a sidecar so
your AI tools have a self-refreshing identity that doesn't expire.
This is the right setup if you ever:

- leave Claude Code or Codex running overnight,
- use prism from CI / scripts / cron,
- don't want to think about `tsh login` ever again.

**One-time setup** (assumes you have `tctl` admin perms on the cluster):

```bash
# 1. Generate Machine ID resource templates and a join token.
prism tbot bootstrap

# 2. Log in with admin perms so tctl can apply them.
tsh login --proxy=<your-cluster>.beams.run:443

# 3. Apply the three generated YAMLs (paths printed by bootstrap).
tctl create -f ~/.config/prism/tbot/role.yaml
tctl create -f ~/.config/prism/tbot/bot.yaml
tctl create -f ~/.config/prism/tbot/token.yaml

# 4. Fetch the registration secret and persist tbot config.
prism tbot configure

# 5. Switch prism over to the tbot backend.
prism config set identity tbot
prism config set tbot.dir ~/.config/prism/tbot
prism up

# 6. (Linux) Optionally install as a systemd user service for persistence.
prism install
```

`prism tbot bootstrap` prints this exact sequence with your hostname
filled in (resources are named `prism-bot-<hostname>` so multiple
machines can share a cluster).

On Linux, `prism install` sets up a systemd user service so the daemon
starts on boot and survives logout — ideal for unattended use. Requires
[lingering](https://www.freedesktop.org/software/systemd/man/loginctl.html)
enabled for your user (`sudo loginctl enable-linger $USER`).

**Verify it's working:**

```bash
prism tbot status      # validates tbot dir, prints proxy/role/bot/secret state
prism status           # shows daemon liveness + listener ports
prism test             # smoke-tests both anthropic and openai backends
```

After that, every invocation of `prism claude` / `prism codex` /
`prism exec` uses the tbot identity — no re-login, ever.

---

## Daily use

```bash
prism claude [args...]        # run Claude Code through prism
prism codex [args...]         # run Codex through prism
prism exec <cmd> [args...]    # run any command with prism env vars set
```

All three auto-start the daemon if it isn't already running, then exec
into the tool with `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL` pointed
at the local router. Any flags pass through:

```bash
prism claude --print "what's 2+2?"
prism codex --model gpt-4o
prism exec python my_script.py
```

For tools you launch outside prism (an IDE plugin, a shell script),
export the env vars yourself:

```bash
prism up
eval "$(prism env)"
```

---

## Token usage tracking

Prism tracks token consumption from every API request that flows through
the router. Usage is recorded per-request in
`~/.config/prism/usage.jsonl` and can be queried with `prism usage`:

```bash
prism usage              # today's totals by model and proxy
prism usage --week       # last 7 days
prism usage --all        # all time
prism usage --json       # raw JSONL records (pipe to jq)
```

Example output:

```
prism usage (today, 42 requests)
────────────────────────────────────────────────────────────

Model                                       Input   Output
────────────────────────────────────────────────────────────
claude-sonnet-4-20250514                   150.2k    28.4k  cache: r=120.0k w=15.0k
claude-opus-4-20250514                      45.0k    12.3k  cache: r=30.0k w=5.0k
gpt-4o                                      8.5k     3.2k

Proxy                                       Input   Output
────────────────────────────────────────────────────────────
prod.teleport.example.com:443              180.2k    35.6k
staging.teleport.example.com:443            23.5k     8.3k
────────────────────────────────────────────────────────────
TOTAL                                      203.7k    43.9k
```

The per-proxy breakdown is useful for tracking consumption against
cluster-level token caps.

---

## Pi integration

[Pi](https://github.com/anthropics/pi) does not read `ANTHROPIC_BASE_URL` or
`OPENAI_BASE_URL` from the environment. Instead it uses hardcoded base URLs
from its model registry (`~/.pi/agent/models-store.json`). To route Pi through
prism, you need to write custom model entries to `~/.pi/agent/models.json`:

```bash
prism pi config
```

This writes entries for `claude-opus-4-6`, `gpt-4o`, and `gpt-5.5` that point
at the local prism router. Run it once (or again after changing the prism
port). After that, `prism exec pi` works as expected.

---

## Commands

| Command | What it does |
| --- | --- |
| `prism claude [args...]` | Ensures the daemon is up; execs `claude` with prism env. |
| `prism codex [args...]` | Same, for `codex`. |
| `prism exec <cmd> [args...]` | Same, for any command. |
| `prism up` | Starts the local daemon (tunnels + router). Idempotent. |
| `prism down` | Stops the daemon and logs out of the apps. |
| `prism status` | Port assignments, identity state, daemon liveness. |
| `prism env` | Prints `export` statements for your shell to `eval`. |
| `prism logs` | Tails the local daemon log (request-level logging). |
| `prism test [anthropic\|openai]` | Smoke test against one or both backends. |
| `prism usage [--week\|--all\|--json]` | Show token usage by model and proxy. |
| `prism pi config` | Write Pi model config to route through prism. |
| `prism config [show\|set\|unset\|clear]` | View/edit persistent config (proxy, identity, tbot.dir). |
| `prism tbot bootstrap` | Generate Machine ID resources for tbot identity. |
| `prism tbot configure` | Persist the bound-keypair registration secret. |
| `prism tbot status` | Validate the tbot working directory. |
| `prism install` | Install as a system service (Linux systemd / macOS LaunchAgent). |
| `prism uninstall` | Remove the system service. |
| `prism version` | Print build version. |

---

## Troubleshooting

### `prism status` shows `[DEAD]`

The daemon was killed. `prism down && prism up`.

### A tunnel port is busy after `prism down`

If the daemon was SIGKILL'd on macOS/Linux, the tbot subprocess gets
reparented to PID 1 and keeps holding its port. `pkill -x tbot` and
retry. (Windows uses Job Objects, so this can't happen there.)

### `tbot: app "anthropic"/"openai" not found`

The bot's role doesn't grant access to the cluster's LLM apps. Re-run
`prism tbot bootstrap` and re-apply the role:

```bash
tctl create -f ~/.config/prism/tbot/role.yaml
```

### `API Error: 400 The inference provider rejected the request…`

The cluster's Anthropic gateway is Bedrock-backed and rejects some
upstream fields. Prism strips the known ones; if a new one shows up,
turn on debug logging and check the daemon log:

```bash
prism down
PRISM_DEBUG=1 prism up
prism logs    # in another terminal, reproduce the error
```

Then add the offending field to `stripFields` in
`internal/router/router.go`.

### tsh session expired

Run `tsh login` — the daemon detects the refreshed identity and
restarts its subprocesses automatically. If you're tired of this,
switch to tbot (see above).

### Debugging the system service (`prism install`)

If prism is installed as a service and misbehaving, these commands help:

**macOS (LaunchAgent):**

```bash
# Check if the job is loaded (modern launchctl)
launchctl print gui/$(id -u)/com.prism.daemon

# Stop and restart
launchctl bootout gui/$(id -u)/com.prism.daemon
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.prism.daemon.plist

# View the plist
cat ~/Library/LaunchAgents/com.prism.daemon.plist

# Tail daemon logs
prism logs
```

**Linux (systemd):**

```bash
# Service status + recent logs
systemctl --user status prism

# Full logs (follow mode)
journalctl --user -u prism -f

# Restart
systemctl --user restart prism

# Check if lingering is enabled (survives logout)
ls /var/lib/systemd/linger/$USER
```

---

## How it works

```
┌────────────────────────┐
│ Claude Code / Codex /  │  ANTHROPIC_BASE_URL=http://127.0.0.1:7331
│ any AI tool            │  OPENAI_BASE_URL=http://127.0.0.1:7331/v1
└──────────┬─────────────┘
           │ plain HTTP
           ▼
┌────────────────────────┐
│ prism router (:7331)   │  path-based dispatch + Bedrock scrubbing
│  /v1/messages → :7333  │
│  /v1/chat/*   → :7334  │
└────┬──────────────┬────┘
     │              │
     ▼              ▼
┌─────────┐   ┌─────────┐
│ tsh/tbot│   │ tsh/tbot│  tunnel subprocesses
│ proxy   │   │ proxy   │
│anthropic│   │ openai  │
└────┬────┘   └────┬────┘
     │              │
     ▼              ▼
  Teleport      Teleport     Beams-managed LLM gateway apps
  anthropic     openai
  app           app
     │              │
     ▼              ▼
  Anthropic     OpenAI
  (Bedrock)     API
```

The router lives in `internal/router/router.go` and does Bedrock-
compatibility scrubbing on `/v1/messages` (strips `metadata`,
`context_management`, `thinking`; caps `max_tokens` at 8192). For
OpenAI requests, it renames `max_tokens` to `max_completion_tokens`
(required by gpt-5.5+). The router also captures token usage from
responses and logs it to `~/.config/prism/usage.jsonl`. The tunnels
are `tsh proxy app` subprocesses, or — in tbot mode — a single
`tbot start` with two `application-tunnel` services.

---

## Repo layout

```
cmd/prism/             local CLI + daemon
internal/router/       HTTP router with path dispatch + scrubbing
internal/tunnel/       subprocess supervisor
internal/tbot/         tbot config rendering and bootstrap
internal/identity/     tsh session expiry watcher
internal/state/        runtime state persistence
internal/config/       persistent machine config
internal/usage/        token usage tracking (JSONL writer + reader)
internal/tshwrap/      tsh CLI wrappers
Makefile               build targets for all platforms
```

---

## License

Apache 2.0 — see [LICENSE](./LICENSE).
