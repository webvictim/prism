# prism

Route local AI traffic (Claude Code, Codex, or anything that speaks
Anthropic/OpenAI) through your Teleport cluster's managed LLM gateways.

Prism tunnels to the cluster-wide `anthropic` and `openai` apps via
Teleport, with a local HTTP router that dispatches by request path and
scrubs Bedrock-incompatible fields. No beams, no VMs, no embedded
binaries — just two tunnel subprocesses and a thin local router.

---

## What you need

- A working `tsh` and a fresh `tsh login` to a cluster with `anthropic`
  and `openai` apps visible in `tsh apps ls`.
- Go ≥ 1.24 to build.
- macOS, Linux, or Windows.

---

## Quick start

```bash
git clone https://github.com/gravitational/saleseng.git
cd prism
make
sudo make install    # or: make install PREFIX=$HOME/.local
```

Tell prism which Teleport cluster to use (once):

```bash
prism config set proxy <your-cluster>.beams.sh:443
```

Then just:

```bash
prism claude
```

That's it. Prism will `tsh app login` to both cluster apps, start a
local daemon, and exec `claude` with the correct environment variables.

---

## Usage

### Running AI tools

```bash
prism claude [args...]        # run Claude Code through prism
prism codex [args...]         # run Codex through prism
prism exec <cmd> [args...]    # run any command with prism env vars set
```

All three auto-start the daemon if it's not running. Pass through any
flags to the underlying tool:

```bash
prism claude --print "what's 2+2?"
prism codex --model gpt-4o
prism exec python my_script.py
```

### Manual control

```bash
prism up [--proxy ADDR] [--port N] [--tsh]   # start the daemon
prism down                                    # stop the daemon
prism status                                  # show tunnel state
prism env                                     # print export statements
prism logs                                    # tail daemon log
prism test [anthropic|openai]                 # smoke-test one or both backends
```

The daemon is detached from the calling terminal (Unix: `setsid`), so
closing the shell doesn't kill it — manage its lifecycle with
`prism up` / `prism down`.

### Using with other tools

```bash
prism up
eval "$(prism env)"
# Now any tool that reads ANTHROPIC_BASE_URL or OPENAI_BASE_URL will
# route through prism.
```

---

## Commands

| Command | What it does |
| --- | --- |
| `prism claude [args...]` | Ensures prism is up, exec's `claude` with prism env. |
| `prism codex [args...]` | Same, but for `codex`. |
| `prism exec <cmd> [args...]` | Same, but for any arbitrary command. |
| `prism up` | Starts the local daemon (tunnels + router). Idempotent. |
| `prism down` | Stops the daemon and logs out of the apps. |
| `prism status` | Port assignments, identity state, daemon liveness. |
| `prism env` | Prints `export` statements for your shell to `eval`. |
| `prism logs` | Tails the local daemon log (request-level logging). |
| `prism test [anthropic\|openai]` | One-shot smoke test against the named backend (or both). `--prompt`, `--model` override defaults. |
| `prism config [show\|set\|unset\|clear]` | View/edit persistent config (proxy, identity, tbot.dir). |
| `prism tbot bootstrap` | Generate Machine ID resources for long-lived identity. |
| `prism tbot configure` | Persist the registration secret after cluster setup. |
| `prism tbot status` | Validate tbot working directory. |
| `prism version` | Print build version. |

---

## Identity backends

### tsh (default)

Uses your interactive `tsh login`. Simple but expires after 12-24h.
When it expires, run `tsh login` and prism's daemon auto-recovers.

### tbot (recommended for always-on use)

Uses Teleport Machine ID with a bound-keypair join token. Self-refreshing
credentials that never expire. Resource names include your hostname for
multi-machine clusters (e.g. `prism-bot-athena`).

Setup (one-time):

```bash
prism tbot bootstrap                          # generates role/bot/token YAML
tsh login --proxy=<your-cluster>:443          # so tctl can talk to the cluster
tctl create -f ~/.config/prism/tbot/role.yaml
tctl create -f ~/.config/prism/tbot/bot.yaml
tctl create -f ~/.config/prism/tbot/token.yaml
prism tbot configure                          # fetches registration secret
prism config set identity tbot
prism config set tbot.dir ~/.config/prism/tbot
prism up
```

`prism tbot bootstrap` prints this exact sequence with your values
filled in, and `prism tbot configure` prints only the steps you still
need to run.

---

## Migrating from beam-based prism

If you're upgrading from the old beam-based architecture, just run
`prism up` with the new binary. It auto-detects the legacy state, stops
the old daemon, and starts fresh with direct cluster app tunnels.

For tbot users, re-run `prism tbot bootstrap` — it preserves your
existing registration secret and updates the role (to add the `llm`
app-type label) and tbot.yaml (multi-service format). Then:

```bash
tctl create -f ~/.config/prism/tbot/role.yaml   # re-apply updated role
prism down && prism up
```

Old beams are left running — destroy them manually with `tsh beams rm <id>`.

---

## Troubleshooting

### `API Error: 400 The inference provider rejected the request…`

The cluster gateway is Bedrock-backed. Prism strips known-incompatible
fields before forwarding. If a new field causes 400s, enable debug
logging and check the daemon log:

```bash
prism down
PRISM_DEBUG=1 prism up
prism logs    # in another terminal, reproduce the error
```

Add the offending field to `stripFields` in `internal/router/router.go`.

### `prism status` shows `[DEAD]`

The daemon was killed. Just `prism down && prism up`.

### A tunnel port is busy after `prism down`

If the daemon was killed uncleanly (SIGKILL, crash) on macOS/Linux, the
tbot subprocess gets reparented to PID 1 and keeps holding its port.
Run `pkill -x tbot` and retry. Windows uses Job Objects to avoid this.

### `tbot: app "anthropic"/"openai" not found`

The bot's role doesn't grant access to the cluster's LLM apps. Re-run
`prism tbot bootstrap` and re-apply the role:

```bash
tctl create -f ~/.config/prism/tbot/role.yaml
```

The role needs `app_labels: {"teleport.internal/beams/app-type": "llm"}`.

### tsh session expired

Run `tsh login` — the daemon auto-restarts its subprocesses when it
detects the refreshed identity.

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
│  /v1/messages → :7334  │
│  /v1/chat/*   → :7335  │
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
  Teleport      Teleport     cluster-managed LLM gateways
  anthropic     openai
  app           app
     │              │
     ▼              ▼
  Anthropic     OpenAI
  (Bedrock)     API
```

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
internal/tshwrap/      tsh CLI wrappers
Makefile               build targets for all platforms
```
