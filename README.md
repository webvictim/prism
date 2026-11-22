# prism

Route local AI tools (Claude Code, Codex, OpenCode, anything that talks
to the Anthropic or OpenAI API) through your Teleport cluster's LLM
gateways, powered by [Teleport Beams](https://goteleport.com/beams/).

*(The name? A prism redirects beams.)*

```
prism claude        # Claude Code, routed via Teleport
prism codex         # Codex CLI, routed via Teleport
prism opencode      # OpenCode, routed via Teleport
prism pi            # Pi, routed via Teleport
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
- **Go ≥ 1.26** (only if building from source).

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

Release notes live in [CHANGELOG.md](CHANGELOG.md) and on the
[releases page](https://github.com/webvictim/prism/releases). Upgrading is
usually just `prism down && prism up` afterwards.

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
prism test             # smoke-tests all three wire formats
```

After that, every invocation of `prism claude` / `prism codex` /
`prism opencode` / `prism pi` / `prism exec` uses the tbot identity — no
re-login, ever.

> **Note:** Teleport caps bot certificates at 12 hours
> (`DefaultBotMaxSessionTTL`). Prism configures tbot to renew every 8
> hours, well within this limit. If you see warnings about expired bot
> identities, run `prism down && prism up` to regenerate tbot.yaml with
> the correct renewal interval.

---

## Daily use

```bash
prism claude [args...]        # run Claude Code through prism
prism codex [args...]         # run Codex through prism
prism opencode [args...]      # run OpenCode through prism
prism pi [args...]            # run Pi through prism
prism exec <cmd> [args...]    # run any command with prism env vars set
```

All of them auto-start the daemon if it isn't already running. Claude Code
and `prism exec` get endpoint environment variables pointing at the local
router. Codex, OpenCode and Pi ignore those variables: `prism codex` passes
Codex a provider definition on its command line, and `prism opencode` and
`prism pi` add "via prism" models to the tool's own configuration before it
starts (see [OpenCode and Pi](#opencode-and-pi-integration)). Any flags pass
through:

```bash
prism claude --print "what's 2+2?"
prism codex --model openai.gpt-5.6-sol
prism opencode run "explain this repo"
prism pi -c
prism exec python my_script.py
```

For tools you launch outside prism (an IDE plugin, a shell script),
export the env vars yourself:

```bash
prism up
eval "$(prism env)"
```

Codex is the exception: it ignores `OPENAI_BASE_URL` (checked from 0.145 to
0.165), so `eval "$(prism env)"` is not enough and a bare `codex` talks to
api.openai.com. `prism codex` registers a provider for you on Codex's own
command line. To use `codex` directly (or `prism exec codex`), put the same
provider in `~/.codex/config.toml`:

```toml
model_provider = "prism"

[model_providers.prism]
name = "prism"
base_url = "http://127.0.0.1:7331/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
supports_websockets = false
```

`prism codex` narrows Codex's model picker to the models the cluster's
`openai` app serves, so you can't pick one that would be refused. It asks
Codex for the catalog it uses itself (`codex debug models`), keeps the
entries the app serves with their capabilities, and passes the result as a
`model_catalog_json` command-line override, removed on exit.

The list comes from a copy the daemon keeps: it reads the cluster's model
list shortly after starting and every hour after, including straight after
the machine wakes from sleep, so launching Codex never waits on the
cluster. On a tbot machine it reads the list with a short-lived identity
tbot keeps for the purpose, so no `tsh` login is needed; otherwise it uses
your `tsh` login, and only if it's live.

It's on by default and never stops Codex starting: if the list can't be
read, or nothing in it matches, prism prints one line and Codex keeps its
own model list. An app with no model list leaves Codex unrestricted, as
the gateway passes any name through. Turn it off with
`prism config set model_discovery false`, which also stops the daemon
reading the list; `prism config unset model_discovery` turns it back
on. No restart is needed either way.

Use the router port from `prism status` if you changed it from the default
7331, and keep `OPENAI_API_KEY` set to anything non-empty — the router
strips it. `supports_websockets = false` matters: Codex's built-in `openai`
provider opens a WebSocket to `wss://api.openai.com/v1/responses` that no
base URL redirects, and prism's router speaks plain HTTP.

### Driving prism from another program

A program that parses the tool's stdout, such as one driving
`claude -p --output-format stream-json`, can set `PRISM_QUIET=1` so
prism writes nothing there itself:

```bash
PRISM_QUIET=1 prism claude -p --output-format stream-json "explain this repo"
```

`prism up` then skips its `export` lines, and the `tsh app login` and
`pi update --models` prism runs before a launch read `/dev/null` and
write to stderr, so a prompt on stdin reads end of file instead of
waiting. Only the value `1` turns it on, and `prism env`, `status` and
`version` still print to stdout.

### Running a tool that isn't the one on PATH

Set a launcher's variable to an absolute path to run that binary instead
of the one on `PATH`:

| Variable | Launcher |
| --- | --- |
| `PRISM_CLAUDE_BIN` | `prism claude` |
| `PRISM_CODEX_BIN` | `prism codex` |
| `PRISM_OPENCODE_BIN` | `prism opencode` |
| `PRISM_PI_BIN` | `prism pi` |

```bash
PRISM_CLAUDE_BIN=/opt/claude-canary/bin/claude prism claude
```

Whatever its name, the binary gets the same setup as the tool on
`PATH`. `prism exec /path/to/claude` doesn't, because prism keys that
setup on the tool's name, but `prism exec claude` honours the variable,
and so does `prism env` when it checks Claude Code's login.
A value that isn't an absolute path to an executable stops the launch
with an error such as
`prism: PRISM_CLAUDE_BIN=claude: not an absolute path` instead of
falling back to `PATH`.

---

## Recording sessions in Teleport Beams

Policy asks for every pull request with AI-generated code
to link to a session recorded in Beams. You don't have to work inside a
beam for that: prism sends your inference *out through* one, and the
session is recorded, summarised and classified in Beams exactly as if the
agent had run there. The agent, your code and your tools stay on this
machine; only the LLM traffic passes through the beam.

**This is on by default.** prism sends inference only through a beam, so
every session is recorded without you doing anything:

- **Each agent session gets a beam of its own**, started when it sends its
  first request (about 15 seconds; `prism claude` and the other launchers
  start it before the agent opens, so you don't wait). Three Claude Code
  sessions in three tabs are three beams and three recordings; a
  subagent shares its parent's.
- **A beam is ended after two hours unused**, which is also what gets it
  summarised (about 15 minutes later). Work that runs past a beam's 11-hour
  life moves to a successor without interruption.
- **At most 10 beams run at once** (`prism config set beam_max <n>` to
  change it). At the limit, a new session shares a beam with another
  session from the same repository, or a beam idle for 15 minutes is ended
  to make room.
- **For the PR**, `prism beam link --pr` prints a markdown section listing
  the beams that carried *this repository's* work. The launchers install
  the `prism-beams` skill, which teaches Claude Code, Codex, OpenCode and
  Pi to add it when they open a pull request (`prism setup skill --remove`
  removes it and keeps it removed).
- **It needs your own Teleport login.** Beams belong to a person, so even a
  machine whose tunnels run on tbot needs `tsh login`. When the login runs
  out, prism stops sending inference and your agent says so, with a link
  that logs you in; `prism login` does the same from a terminal, and
  `prism claude` (and the others) log you in before the agent starts.
  Nothing is ever sent unrecorded instead.

```bash
prism login            # tsh login to prism's cluster, for as long as it allows
prism beam status      # the beams running, and your login
prism beam link --pr   # the PR section, for this repository
prism beam end         # end this repository's beams now (summaries sooner)
prism beam test        # start a beam of its own and prove inference works through it
```

The policy also asks for evidence of human design and review. prism only
says which beams carried the inference; describing your own review is up
to you.

### Upgrading

**If prism uses your `tsh login`** (the default), there's nothing to do.
Upgrading from v0.4 or earlier needs one `prism down && prism up`, because
that daemon can't tell prism whether it's busy. After that, every session
is recorded, and the launchers (`prism claude` and the rest) keep the
agent skill installed and current. Later upgrades restart the daemon
themselves, at the first launch that finds it idle.

**If prism uses tbot**, prism keeps working exactly as before, unrecorded,
and reminds you each time you run it, until you switch. Switching is one
command, the same for everyone (on a tsh machine it only confirms you're
set):

```bash
prism setup beams
```

It logs you in to Teleport with your own account (alongside tbot, which
keeps carrying prism's own checks), checks that you can use Beams, switches
prism to recording every session, and installs the agent skill. Nothing
restarts. `prism status` and `prism doctor` show the same steps.

The company's central Beams tenant won't offer tbot, and caps logins at 8
hours, so the tsh login is the setup to get used to. To move a machine
over to it now:

```bash
prism config set identity tsh && prism down && prism up
```

and, when the central tenant arrives, `prism config set proxy <its address>`
followed by `prism down && prism up`.

### Settings (for power users)

| Setting | Default | What it does |
| --- | --- | --- |
| `beam_mode` | `required` | `required`: inference only through a beam. `preferred`: through a beam when one can be started, otherwise unrecorded. `off`: unrecorded, unless you run `prism beam start` (then everything goes through that one beam until `prism beam end`). |
| `beam_scope` | `session` | `session`: a beam per agent session. `repo`: one per repository, shared by its sessions. |
| `beam_max` | `10` | How many beams may run at once. |
| `beam_auto_login` | `false` | When a login is needed, let the daemon open your browser and hold the request until the beam is up. For an identity provider that signs you in without a prompt; otherwise leave it off and use the link the agent shows. |
| `beam_roles` | your standing roles | Delegate only these roles to each beam (`tsh beams add --roles`). `beam-user` is enough for inference; `prism beam test` checks a set. |

All of them apply to a running daemon within a few seconds. Inference that
doesn't need recording (personal projects on your own tenant, say) is
`prism config set beam_mode off`; `prism config unset beam_mode` turns
recording back on.

For a single launch, put a flag before the tool:

```bash
prism --no-beam claude -p 'quick question'   # no beam: no wait, not recorded
prism --beam codex exec 'record this one'    # through a beam, whatever beam_mode
```

`--no-beam` skips the beams entirely, including the wait for one to start,
so a one-off request costs only the request. `--beam` sends that launch
only through a beam, as `required` would, even when `beam_mode` is `off` or
`preferred`. Both last for the whole launch, subagents included, and work
only with `prism claude` and `prism codex`. Those are the launchers that can
add a header to the tool's requests, which is how the daemon tells the
launch apart. `prism beam status` counts the requests sent with `--no-beam`.

### Working in a VM (Lima, a devcontainer, over SSH)

prism runs the same inside a Linux VM. A login started there can't open
your browser, so prism never tries to: the link it prints (in your agent's
message, or from `prism login`) points at a port on the VM's `127.0.0.1`,
which Lima forwards to your Mac, so opening it in your Mac's browser
completes the login in the VM. Desktop notifications aren't available in a
VM, so the agent's message is where you'll hear that a login is needed.

### What isn't recorded per session

Pi sends no session id on Anthropic requests, and a bare `claude` started
from a shell with `prism env` (rather than `prism claude`) can't be tagged
with its repository. Their requests share one beam, which `prism beam link
--pr` lists separately as work it couldn't attribute.

## Claude Code authentication (no subscription needed)

You don't need a Claude subscription — or any Anthropic credential — to
use `prism claude`. Inference is authenticated cluster-side: the Teleport
LLM app holds the provider credentials and prism strips whatever the
client sends, so your Teleport login is the only thing that grants
access.

Claude Code itself still refuses to send a request until it believes it
has a credential, stopping at `Not logged in · Please run /login`. So
`prism claude` hands it a dummy gateway token
(`ANTHROPIC_AUTH_TOKEN=teleport`, discarded by the router exactly like
`OPENAI_API_KEY`). Install prism, run the setup steps above, and
`prism claude` works on a machine that has never been logged in — and on
one whose claude.ai login has lapsed.

The one case prism leaves alone is forward-proxy mode with a working
claude.ai login, because [Remote Control](#claude-code-remote-control)
needs a genuinely signed-in session. prism asks Claude Code itself
(`claude auth status --json`) and only injects the token when the answer
is "no usable login".

| `claude_auth` | Behaviour |
| --- | --- |
| `auto` (default) | Inject the token, except in forward-proxy mode with a claude.ai login on a Pro/Max/Team/Enterprise plan. On a machine whose managed settings pin sign-ins, use platform mode instead. |
| `token` | Always inject, ignoring any stored login. |
| `login` | Never inject; Claude Code uses its own credential. |
| `platform` | Never inject; run Claude Code as a third-party platform client, which needs no Anthropic sign-in at all. |

```bash
prism config set claude_auth token
```

Reach for `token` when Claude Code still *reports* a usable login that
no longer works — a cancelled or downgraded plan, or a deactivated
account. `claude auth status` reads Claude Code's cached state (it keeps
reporting a subscription with the network unplugged), so prism can't tell
a dead account from a live one; this is the override for that case. It's
also the way to force prism's token while keeping forward-proxy mode on
for other reasons.

Use `login` if you want a session to authenticate as your own account
even in plain router mode — for instance to keep `/usage` and
account-tied features pointed at your subscription. No restart is needed
either way; the setting is read when `prism claude` launches.

### Managed machines that pin sign-ins

A managed machine can require Anthropic sign-ins to belong to one
organization (`forceLoginOrgUUID`) or use one method
(`forceLoginMethod`). Claude Code then refuses to start while *any*
Anthropic credential is configured — prism's dummy token included:

```
This machine's managed settings require a first-party login: they pin
Anthropic sign-ins to your organization (forceLoginOrgUUID), and this
session is configured with a bearer token from ANTHROPIC_AUTH_TOKEN,
which cannot be checked against that policy.
```

Dropping the token doesn't help on its own: with no credential at all
Claude Code stops at `Not logged in · Please run /login` under the same
policy. What the pin actually polices is *first-party* sign-in, so prism
stops being a first-party client. On a pinned machine it runs Claude Code
as a **third-party platform client** pointed at the local router, with no
Anthropic credential at all — the same shape a Teleport beam uses against
the very same cluster gateway. No sign-in, nothing to configure: upgrade
and run `prism claude`.

```
prism: managed settings here pin Anthropic sign-ins, so prism is running
Claude Code as a third-party platform client against the Teleport
gateway — no sign-in needed.
```

The note appears only when stderr is a terminal, so a program running
`prism claude -p` sees Claude Code's own errors first.

On the wire nothing changes (`POST /v1/messages`, model in the body), so
scrubbing, usage capture and the request log all behave as usual.
`claude auth status` reports `"authMethod": "third_party"`. Remote
Control stays unavailable there, because it needs a first-party session —
which is exactly what the pin forbids unattended.

prism reads the policy from the files Claude Code itself reads —
`/Library/Managed Preferences/com.anthropic.claudecode.plist` and
`/Library/Application Support/ClaudeCode/managed-settings.json` on macOS,
`/etc/claude-code/managed-settings.json` on Linux,
`C:\Program Files\ClaudeCode\managed-settings.json` on Windows. For a pin
delivered another way (a `policyHelper`, or the Windows registry), ask
for platform mode outright:

```bash
prism config set claude_auth platform
```

Managed settings that say nothing about sign-ins change nothing: the
dummy token is what lets prism work with no Anthropic account, and it
stays.

---

## Claude Code Remote Control

Claude Code disables [Remote Control](https://claude.ai/code) when
`ANTHROPIC_BASE_URL` is set (which prism does by default). To keep
Remote Control while still routing inference through Teleport, enable
forward-proxy mode:

```bash
prism config set claude_forward_proxy_mode true
prism down && prism up
```

In this mode, `prism claude` sets `HTTPS_PROXY` and
`NODE_EXTRA_CA_CERTS` instead of `ANTHROPIC_BASE_URL`. The daemon acts
as an HTTPS forward proxy: model API calls (`/v1/messages`) are
intercepted and routed through the Teleport tunnel, while everything
else (Remote Control, feature flags, telemetry, MCP) passes through to
the real `api.anthropic.com` with your credentials intact. A
locally-generated CA certificate (`~/.config/prism/ca.pem`) makes the
interception transparent to Claude Code.

Both interactive sessions and headless Remote Control work:

```bash
prism claude       # interactive session, controllable from claude.ai/code
prism claude rc    # headless Remote Control worker
```

To go back to plain base-URL mode:
`prism config set claude_forward_proxy_mode false`, then
`prism down && prism up`.

---

## Token usage tracking

Prism tracks token consumption from every API request that flows through
the router. Usage is recorded per-request in monthly JSONL files under
`~/.config/prism/usage/` and can be queried with `prism usage`:

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
claude-opus-5                              150.2k    28.4k  cache: r=120.0k w=15.0k
claude-sonnet-5                             45.0k    12.3k  cache: r=30.0k w=5.0k
openai.gpt-5.6-sol                           8.5k     3.2k

Proxy                                       Input   Output
────────────────────────────────────────────────────────────
prod.teleport.example.com:443              180.2k    35.6k
staging.teleport.example.com:443            23.5k     8.3k
────────────────────────────────────────────────────────────
TOTAL                                      203.7k    43.9k
```

The per-proxy breakdown is useful for tracking consumption against
cluster-level token caps.

### The tenant's token quota

A Teleport Beams tenant caps how many tokens its LLM apps serve: for
example 5M output tokens a day, or 30M for the tenant's lifetime. When a
window is used up, every request is refused with `tokens quota exceeded`.
prism shows how much of it is gone:

```
Tenant quota, output tokens (fresh-beans, read 2m ago)
────────────────────────────────────────────────────────────
anthropic  daily, resets 20:00              131.3k / 5.0M*   2.6%
openai     daily, resets 20:00              128.2k / 5.0M*   2.6%
this machine, same windows: anthropic 58.9k  openai 60
* assumed: the cluster doesn't say; `prism config set quota_limits anthropic=5M,openai=5M`
```

That section is printed under `prism usage`. `prism status` gets one line
for it, `prism status --json` gets a `quota` key, `prism doctor` warns at
80%, and [statusline](https://github.com/ryanclark/statusline) can show it
in Claude Code (below).

- **It needs your `tsh` login.** The cluster only lets a tenant's own user
  read the counters, and a tbot identity is refused. The daemon reads them
  every couple of minutes while you're working, and every 30 minutes
  otherwise, but only while a login is live. It never starts a login
  itself. Without one, or without `tctl` on `PATH`, nothing is shown, and
  `prism usage` says why. `prism usage --refresh` reads it now.
- **The counts are the whole tenant's**: every user, every beam, and
  anything that reached the apps without prism. The "this machine" line is
  your share over the same window.
- **The limits come from the cluster** when it says. Teleport v19 lists
  each LLM app's limits, and every beam already runs it. With the 18.x
  `tsh` most clusters pin today, prism assumes what Beams tenants use:
  5M output and 35M input tokens for a daily window (resetting when the
  cluster says it does), and 30M / 90M for a lifetime one, marked `*`.
  The percentage counts whichever is closer, which for OpenAI is usually
  input: its count includes cached tokens. If your tenant differs:
  `prism config set quota_limits anthropic=10M,openai=10M` (output).
  Reading the limits needs only access to the apps, so if your roles
  can't read the usage, prism still knows the limits.
- **When the tenant's usage can't be read** (your tsh login expired, or
  roles that can't see it), prism estimates it, marked `≈`. While the
  last reading's window is still open, that is the reading plus this
  machine's requests since; once the window resets, it is this machine's
  requests alone. Other users and beams keep counting meanwhile, so the
  real figure may be higher. prism estimates only when it has reason to
  think the tenant has a quota (an earlier reading, the cluster's limits,
  or `quota_limits`), and from this machine alone only for an app it has
  used in the window; otherwise `prism usage` shows the limits alone and
  says why.
- **Inside a beam**, prism needs no configuration: it reads the beam's own
  cluster as the beam's identity. A beam with your standing roles shows
  usage; one started with `--roles beam-user` shows only the limits.
- tctl is always aimed at the configured `proxy`
  (`--auth-server=<proxy>`), never at whichever cluster your shell last
  logged into, and numbers saved for one proxy are never shown for
  another.

#### In statusline

If Claude Code's status line is [statusline](https://github.com/ryanclark/statusline)
(as its `statusLine` command or its plugin), `prism claude` shows the
quota with no setup: `Claude 3% 2h41m  OpenAI 3% 2h41m`. Only sessions
started with `prism claude` use prism as the usage source, so other
Claude Code sessions keep their claude.ai usage. The first launch adds two
segments to `~/.statusline/settings.json`, which stay blank in those other
sessions. `prism config set statusline_auto false` turns this off.

**`(stale)` means log in again.** The daemon reads the quota with your
tsh login. When that expires it can't, and statusline shows the figure
as prism last knew it, brought up to date with this machine's requests:

```
Claude (tsh login expired) ≈ 18% 12h23m (stale)
```

statusline adds `(stale)` and dims the value once the tenant's own
reading is more than 35 minutes old. `tsh login --proxy=<your proxy>`
fixes it: the daemon reads the quota again within a couple of minutes,
and the marks go.

To use prism as the source in every session instead (a bare `claude`
under `prism env`, say):

```bash
prism setup statusline           # add prism as statusline's usage source
prism setup statusline --remove  # undo
```

This adds a `usage_source` running `prism usage --statusline` to
`~/.statusline/settings.json` (backing it up first), plus two
`usage_limit` segments, `beams_anthropic` and `beams_openai`, that render
as `Claude 3% 2h41m  OpenAI 3% 2h41m`. `prism usage --statusline` reads
only the daemon's saved copy, so it answers instantly. While an external
source is configured, statusline stops fetching claude.ai account usage
(`five_hour` / `seven_day`). Sessions through prism's router have none
anyway.

### When the gateway serves a different model

The cluster gateway only serves the models it is configured for. Ask for
anything else and it **silently swaps in a fallback** — the request
succeeds, nothing in the response says a swap happened, and your client
goes on reporting the model it asked for. Claude Code also costs the
call at that model's list price, so a substituted Opus request shows up
as Opus tokens at Opus rates.

Prism is the only place both names are visible at once, so it flags
them. Any such request gets `requested=` and a `[!]` in the log:

```
POST /v1/messages 200 req=747024B resp=2073B requested=claude-opus-5-5 model=claude-sonnet-5 [!] in=2 out=89 cache_read=18807 cache_write=209741 2.721s
```

`prism usage` totals them up:

```
Substituted by gateway  [!]                        Requests
────────────────────────────────────────────────────────────
claude-opus-5-5 → claude-sonnet-5                      1204
openai.gpt-6-astra → openai.gpt-5.6-luna                 37
```

To find out what your cluster actually serves, ask for a model and read
the `model` field back out of the reply — a name that comes back
unchanged is served, anything else is the fallback:

```bash
curl -s http://127.0.0.1:7331/v1/messages \
  -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"claude-opus-5","max_tokens":4,"messages":[{"role":"user","content":"hi"}]}' \
  | jq -r .model
```

Prism deliberately does **not** repoint your client at the served model
for you. Switching tiers would need prism to pick models on your behalf,
and a silent remap is the problem, not the cure — so it tells you, and
leaves the choice with you.

---

## OpenCode and Pi integration

OpenCode (v2) and Pi both ignore `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL`:
OpenCode sends requests straight to the vendor with prism's dummy key
(`invalid x-api-key`), and Pi reads base URLs from its model registry. So
prism writes models into each tool's own configuration instead:

```bash
prism setup opencode     # ~/.config/opencode/opencode.jsonc (or .json)
prism setup pi           # ~/.pi/agent/models.json
```

Each adds a **"<model> via prism"** entry for every model `prism models`
lists, under three providers of prism's own, one per API:

| Provider | Models | API |
| --- | --- | --- |
| `prism-anthropic` | the `anthropic` app's | Messages |
| `prism-openai` | the `openai` app's | Responses |
| `prism-openai-chat` | `openai` app models served only on chat/completions (Kimi, GLM) | chat/completions |

Pick a model by its "via prism" name (`prism-anthropic/claude-opus-5-5` on
the command line). The tool's built-in `anthropic` and `openai` providers
aren't touched, so it's fine for them to be in OpenCode's
`disabled_providers`; prism warns if one of its own providers is.

The model list doesn't say which OpenAI models are chat/completions-only,
so the first time prism sees a model it sends it one tiny request to find
out, and files the entry under the provider that serves it. That needs
prism running; a model it couldn't check is left out with a warning, and
added next time.

Running it again only **adds**: new models get entries, and each
provider's base URL follows the router port, but an entry already there is
left as it is, so you can edit one (OpenCode's `limit`, say) and keep the
edit. OpenCode's config is edited in place, so your comments and the rest
of the file survive. `--overwrite` instead replaces every provider in the
file with prism's alone, after copying the file to `<file>.bak`; settings
outside the provider block are kept.

`prism opencode` and `prism pi` do the additive refresh on every launch,
from the daemon's cached model list so a launch never waits on the
cluster. Plain `opencode` or `pi` work too once the entries exist, as long
as prism is running.

Pi gets two extras:

- **`prism pi` also routes Pi's own models through prism**: it mirrors
  Pi's catalog into `models.json` with each built-in Anthropic and OpenAI
  entry's `baseUrl` pointed at the router, so whichever model you pick
  goes through prism. Each `via prism` entry copies Pi's metadata (cost,
  context window, capabilities) for its id when Pi's catalog has it. On a
  fresh Pi install it first runs `pi update --models` to populate the
  catalog, and stops rather than starting Pi with direct vendor routes if
  that fails. `PI_CODING_AGENT_DIR` is respected.
- **Mid-conversation effort is turned off** on the OpenAI entries prism
  writes. With it on, Pi sends the thinking level as `configuration_update`
  items, which the gateway rejects ("The 'configuration_update' item type
  is not supported with this model"); without it Pi sends
  `reasoning.effort`, so the level you pick still applies.

`--anthropic-model` / `--openai-model` narrow the rerouting of Pi's own
models to one id. Launch with `prism exec pi` after a narrowed or
`--overwrite` setup to keep it, since `prism pi` restores every catalog
model.

`prism setup pi` used to be `prism pi config`, which hid Pi's own `pi
config`. The old spelling still works for now, with a notice; until it
passes through, reach Pi's command with `prism exec pi config`.

---

## chat/completions shim

Newer gateways serve OpenAI models **only** on the Responses API. Ask for
any model on `/v1/chat/completions` and you get:

```
model `openai.gpt-5.6-sol` isn't supported on this route
```

Clients that only speak chat/completions — MacWhisper, Teleport session
summaries, most "OpenAI-compatible endpoint" boxes — would be dead in the
water. So prism translates: `/v1/chat/completions` in, `/v1/responses`
upstream, chat-shaped reply back out, streaming included. Requests to
`/v1/responses` are untouched, so Codex and anything else already speaking
Responses is unaffected.

It's on by default. Turn it off to talk to an older gateway that still
serves chat/completions natively:

```bash
prism config set openai_chat_completions_shim false
prism down && prism up
```

With it off, `/v1/chat/completions` is relayed byte-for-byte, which is what
makes a current prism usable against a legacy Beam.

Either way, the setting is only the default. Some models are the other way
round and exist **only** on chat/completions — Kimi K2.5 and GLM 5, for
example — and the gateway refuses those on the Responses API. When the
gateway says a model lives on the other route, prism sends it there instead
and remembers that for the model, so one gateway can serve both kinds with
nothing to configure. The first request for each such model pays for one
extra round trip, and the daemon log says so once:

```
chatcompat: kimi-k2.5 isn't served on /v1/responses — sending it to /v1/chat/completions, and remembering for the rest of this daemon's life
```

Translated requests are visible in the log:

```
POST /v1/chat/completions [-> /v1/responses] 200 req=141B resp=373B model=openai.gpt-5.6-luna in=11 out=9 cache_read=0 cache_write=0 2.95s
```

**No model names are compiled into prism.** Reasoning models reject
parameters like `temperature` and `top_p`, but which ones depends on the
model, so prism doesn't guess: it forwards what the client sent, reads the
parameter name out of the gateway's rejection, drops it, and retries —
remembering the rejection per model for the life of the daemon.

```
chatcompat: openai.gpt-5.6-sol rejected parameter "temperature" — dropped it,
retrying, and remembering for the rest of this daemon's life
```

The memory is deliberately in-process, not on disk: a persisted cache would
keep stripping a parameter forever after the gateway started accepting it.
Relearning costs one extra round trip per model per restart.

Two limitations worth knowing:

- **Tool calling isn't translated.** A request carrying `tools` is relayed
  to `/v1/chat/completions` as it is, which works for models served there
  (Kimi, GLM). A model served only on `/v1/responses` gets a 400 pointing
  there instead.
- **Reasoning tokens count against `max_tokens`.** A client that asks for
  `max_tokens: 40` may spend all of it on reasoning and get
  `finish_reason: "length"` with little or no text. That's the gateway's
  accounting, not prism's — raise the client's limit if replies come back
  truncated.

---

## Commands

| Command | What it does |
| --- | --- |
| `prism claude [args...]` | Ensures the daemon is up; execs `claude` with prism env. |
| `prism codex [args...]` | Same, for `codex`, plus a `model_providers` definition on its command line, since Codex ignores `OPENAI_BASE_URL`. |
| `prism opencode [args...]` | Same, for `opencode`, after adding any new "via prism" models to its config. |
| `prism pi [args...]` | Same, for Pi, after adding "via prism" models and routing Pi's own models through prism. |
| `prism exec <cmd> [args...]` | Same, for any command. |
| `prism --no-beam\|--beam claude\|codex [args...]` | One launch without a beam (no wait, not recorded), or only through one, whatever `beam_mode` says. See [Settings](#settings-for-power-users). |
| `prism up` | Starts the local daemon (tunnels + router). Idempotent. |
| `prism down` | Stops the daemon and logs out of the apps. |
| `prism status` | Port assignments, identity state, daemon liveness. |
| `prism status --json` | Whether prism is ready, as one JSON object for scripts. See [below](#prism-status---json). |
| `prism env` | Prints `export` statements for your shell to `eval`. |
| `prism logs` | Tails the local daemon log (request-level logging). |
| `prism test [anthropic\|openai\|all]` | Smoke test. `--format anthropic\|openai-responses\|openai-completions` picks a wire format, `--stream` exercises SSE. `--anthropic-model` / `--openai-model` pick each backend's model and `--model` sets both; see [Probe models](#probe-models). |
| `prism models [--cached] [--json]` | List the models the cluster's LLM apps serve, and which one `prism test` and `prism doctor` will use. Asks the cluster live (with prism's tbot identity, or your `tsh` login) and refreshes the daemon's cached copy; `--cached` shows that copy and how old it is. |
| `prism usage [--week\|--all\|--json] [--refresh]` | Show token usage by model and proxy, and the tenant's quota; see [The tenant's token quota](#the-tenants-token-quota). |
| `prism usage --statusline` | statusline's external usage command: the tenant's quota as its protocol-v1 JSON. |
| `prism doctor [--no-request] [--model <id>]` | Check everything — service, daemon, identity, tbot health, the gateway — and print a report with the fix on each line. Exits non-zero if anything failed. |
| `prism setup opencode [--overwrite]` | Add a "via prism" model to OpenCode's config for every model the cluster serves. `--overwrite` replaces its providers with prism's only. See [OpenCode and Pi](#opencode-and-pi-integration). |
| `prism setup pi [--overwrite]` | The same for Pi, plus routing Pi's own models through prism; `--anthropic-model` / `--openai-model` narrow that part to one id. |
| `prism setup statusline [--remove]` | Show the tenant's quota in statusline. |
| `prism setup skill [--remove]` | Install the `prism-beams` skill for Claude Code, Codex, OpenCode and Pi, so agents record work in Beams and link it in PRs. |
| `prism beam status\|link [--pr]\|end\|test\|start` | The beams recording your sessions in Beams: what's running, the PR section, ending them, proving the path works; see [Recording sessions in Teleport Beams](#recording-sessions-in-teleport-beams). |
| `prism login [--ttl <min>]` | `tsh login` to prism's cluster for as long as your roles allow — the login beams need. |
| `prism setup beams [--auto-login]` | The one-time switch to recording every session in Beams, for a machine set up for tbot. |
| `prism config [show\|set\|unset\|clear]` | View/edit persistent config (proxy, identity, tbot.dir, claude_forward_proxy_mode, claude_auth, model_discovery, openai_chat_completions_shim, anthropic_test_model, openai_test_model, beam_mode, beam_scope, beam_max, beam_auto_login, beam_roles, quota_limits, statusline_auto). |
| `prism tbot bootstrap` | Generate Machine ID resources for tbot identity. |
| `prism tbot configure` | Persist the bound-keypair registration secret. |
| `prism tbot status` | Validate the tbot working directory. |
| `prism tbot reset [--yes\|--dry-run]` | Rebuild this machine's Machine ID identity from scratch: delete and recreate its bot, role and token in the cluster, clear the local tbot state, restart prism and check that inference works. |
| `prism install` | Install as a system service (Linux systemd / macOS LaunchAgent). |
| `prism uninstall` | Remove the system service. |
| `prism version` | Print build version. |

### `prism status --json`

Lets a script or app check that prism can serve requests before it
starts work. It prints one JSON object on one line (expanded here) and
exits 0 either way. It costs one local health request plus `tsh status`
in tsh mode:

```json
{
  "schema": 1,
  "version": "0.2.2",
  "ready": true,
  "reason": null,
  "daemon": { "running": true, "pid": 4242, "managed_by": null },
  "router": { "port": 7331, "health": "ok" },
  "identity": { "backend": "tsh", "proxy": "example.teleport.sh:443", "expires_at": "2026-11-04T06:00:00Z" },
  "apps": { "anthropic": { "healthy": true }, "openai": { "healthy": null } },
  "quota": null
}
```

- `ready` needs a running daemon, a router that answers
  `/_prism/health` and an unexpired Teleport login. When it is false,
  `reason` says why and what to run.
- `router.health` is `"ok"`, `"fail"` or `"unknown"`.
  `daemon.managed_by` is `"launchd"`, `"systemd"` or `null`, and
  `daemon.pid` is `null` for a service-managed daemon.
- `identity.expires_at` is `null` in tbot mode, which renews itself.
  `apps.openai.healthy` is always `null`, because the health check
  covers only the Anthropic tunnel.
- `quota` is the tenant's token quota from the daemon's saved copy:
  `proxy`, `fetched_at`, and per app `interval`, `output_tokens`,
  `input_tokens`, `output_limit`, `input_limit`, `limit_source`
  (`cluster`/`default`/`config`), `used_percent`, `resets_at` and
  `approximate` (true for an estimate from this machine's own records,
  when `fetched_at` is `null`). When the limits can be read but nothing
  else, the usage fields and `fetched_at` are `null`. It is `null` when
  there is nothing to show; see
  [The tenant's token quota](#the-tenants-token-quota).
- Anything prism can't tell is `null`, and no key is ever left out.
  `schema` goes up only when a field is removed or changes meaning, so
  ignore keys you don't know.

---

## Troubleshooting

### Start here: `prism doctor`

```
$ prism doctor
prism doctor
────────────────────────────────────────────────────────────
prism 0.2.2 · darwin/arm64 · host gaia · 2026-11-03T08:58:34-04:00
binary /opt/homebrew/opt/prism/bin/prism
machine 6E1B2C3D-4F5…
logs /Users/gus/.config/prism/logs/daemon-2026-11-03.log

[ ok ] service            launchd-managed, loaded
[ ok ] daemon             router 127.0.0.1:7331, health ok
[    ] identity mode      tbot
[ ok ] tbot dir           /Users/gus/.config/prism/tbot
[ ok ] machine binding    issued to this machine
[ ok ] bot name           prism-bot-gaia
[ ok ] join secret        set
[ ok ] proxy              example.teleport.sh:443 reachable
[ ok ] tbot version       v18.11.7 (/opt/homebrew/bin/tbot), cluster v18.11.7
[FAIL] tbot health        tunnel not ready: { "status": "initializing", "updated_at": null }
       → usually a join failure — `prism tbot reset` rebuilds the identity
[    ] last tbot error    could not lookup signer for public key ssh-ed25519 AAAAC3Nza…
[ ok ] tbot process       pid 5860
[    ] inference          not checked (prism is not running)
────────────────────────────────────────────────────────────
prism doctor: 1 check(s) failed, 0 warning(s).
```

Every line carries its own fix, and the last line is the verdict —
non-zero exit if anything failed, so it can be scripted. The report is
meant to be pasted to whoever is helping you: it names the versions,
the cluster and the failure, and never prints your registration secret
or anything from a request. `--no-request` skips the live request at
the end (useful offline, or when the cluster is known to be down).

It changes nothing, including nothing in the cluster.

### Probe models

`prism test`, `prism doctor` and the last step of `prism tbot reset` send a
real request, so they have to name a model. For each backend, prism uses:

1. a flag, if you gave one (`--anthropic-model`, `--openai-model`, `--model`);
2. otherwise `anthropic_test_model` / `openai_test_model`, if set;
3. otherwise a model from the cluster's own list, as the daemon last read
   it — `claude-sonnet-5` or `gpt-6-luna` when the cluster serves them, else
   the first it lists;
4. otherwise `claude-sonnet-5` / `gpt-6-luna`, so it works even when the
   list can't be read.

`prism models` shows the cluster's lists and which model each backend
would use. If you'd rather pin one:

```bash
prism config set anthropic_test_model <id>   # Anthropic probes
prism config set openai_test_model <id>      # OpenAI probes
prism config unset anthropic_test_model      # back to the default
```

These take effect on the next command, with no restart. A refused model
tells you which key to set: `prism test` prints it, `prism doctor` shows a
warning rather than a failure (the request reached the gateway, so nothing
on your machine is broken), and `prism tbot reset` still finishes. A flag
overrides the key for one run.

### tbot is broken and you just want it working: `prism tbot reset`

```bash
prism tbot reset --dry-run   # print the plan, change nothing
prism tbot reset             # ask, then do it
```

Rebuilds this machine's Machine ID identity end to end: stop prism,
delete this machine's bot, role and token from the cluster, clear the
local tbot state, create all of it again, restart prism, and send one
request through it to prove the result works. If Teleport has locked the
bot's join token after a certificate generation mismatch, the reset
removes that lock first; a lock someone placed by hand stops it before
anything changes, naming the lock. It prints the plan and
asks for confirmation first (`--yes` skips the prompt, and is required
when stdin isn't a terminal). prism comes back up on the port it was
already using, and the last step waits for the tunnel rather than
declaring failure a second early.

It needs `tctl` on PATH and a live `tsh login` **for prism's cluster**,
with permission to manage bots; it says so up front, naming the exact
`tsh login --proxy=…`, rather than failing halfway. If your cluster
requires per-session MFA, the prompts appear as usual.

You do not need to switch your `tsh` login context first: prism passes
`--proxy` to tsh and `--auth-server` to tctl on every call, so it acts
on the cluster in `prism config` whatever your shell is logged into.
That also means **moving a machine to a different cluster is
`prism config set proxy <host:port>` followed by `prism tbot reset`** —
reset says when it is moving a machine rather than repairing one, and
leaves the old cluster's bot alone.

**Everything is scoped to this machine's hostname** —
`prism-bot-<host>`, `prism-bot-role-<host>`, `prism-bot-token-<host>` —
derived from the hostname, never from whatever the local config happens
to name. So it cannot touch another machine's bot, and if this machine's
tbot directory was copied from another computer (see below), reset
gives *this* machine its own bot and leaves the original's alone.

### `the tbot identity in … belongs to a different machine`

Cloning a Mac with Migration Assistant copies prism's tbot directory,
which holds a Teleport bound keypair issued to the original machine,
along with a LaunchAgent that starts prism at login. Two machines
renewing one bound keypair can lock the bot and break prism on the
original machine, so prism refuses to start tbot when it notices:

```
prism: daemon: the tbot identity in /Users/gus/.config/prism/tbot belongs to a
different machine (it records 6E1B2C3D-4F5…, this machine is A1B2C3D4-5E6…).
It was most likely copied here by Migration Assistant or a disk clone. …
Run `prism tbot reset` to give this machine its own bot
```

`prism tbot reset` is the fix. The original machine is unaffected.

If the clone has not been renamed yet, both machines want the same
`prism-bot-<host>` name and prism stops rather than guess: rename this
machine (macOS: System Settings → General → Sharing → Local hostname)
and run reset again, or pass explicit `--bot-name` / `--role-name` /
`--token-name`.

The binding is recorded the first time a machine runs prism 0.2.0 or
later, so a clone is only recognised if the machine it was cloned
*from* had run that version at least once. On Linux the check is weaker:
it reads `/etc/machine-id`, which a disk-image clone copies verbatim.

### `prism status` shows `[DEAD]`

The daemon was killed. `prism down && prism up`.

### A tunnel port is busy after `prism down`

If the daemon was SIGKILL'd on macOS/Linux, the tbot subprocess gets
reparented to PID 1 and keeps running — holding its port, and renewing
the identity alongside the next one, which gets the bot locked. `prism up`
stops it before starting its own tbot (`prism logs` says so). (Windows
uses Job Objects, so this can't happen there.)

### `… have been locked due to a certificate generation mismatch`

Teleport saw this machine's identity renewed out of order and locked its
join token, assuming a stolen certificate. Nothing has to have been
copied: a renewal whose reply was lost while the laptop slept is enough,
as is a second tbot left running on the same identity. `prism tbot reset`
rebuilds the identity and removes that lock — a recreated token keeps its
name, so the lock would otherwise still apply.

### `tbot: app "anthropic"/"openai" not found`

The bot's role doesn't grant access to the cluster's LLM apps. Re-run
`prism tbot bootstrap` and re-apply the role:

```bash
tctl create -f ~/.config/prism/tbot/role.yaml
```

### `tbot: too many levels of symbolic links`

tbot refuses to write to its storage directory if the path contains
symlinks (common in Lima VMs where `~/.config` is symlinked). Move the
tbot directory to a real path:

```bash
prism tbot bootstrap --dir ~/prism-tbot
prism config set tbot.dir ~/prism-tbot
# then re-run tctl create and prism tbot configure as normal
```

### `API Error: 400 The inference provider rejected the request…`

The cluster's Anthropic gateway is Bedrock-backed and rejects some
request fields the first-party API accepts (top-level fields such as
`thinking` and Pi's `fallbacks`, plus unknown keys inside `cache_control`).
Prism strips the known ones; if a new one shows up, turn on debug logging and
check the daemon log:

```bash
prism down
PRISM_DEBUG=1 prism up
prism logs    # in another terminal, reproduce the error
```

Then add the offending field to `anthropicStripFields` (or
`cacheControlAllowedKeys`) in `internal/scrub/anthropic.go`. To find
the culprit, replay the failing request body against
`http://127.0.0.1:7331/v1/messages` with fields removed until it
succeeds.

### `prism models` shows no models, but the cluster has them

prism reads the model list with `tsh`, and a tsh before 18.8.3 lists the
apps without their models. That can be the case even with a newer tsh
installed: with Teleport's managed client-tools updates on, the installed
tsh runs the version recorded for the cluster at your last `tsh login`,
and keeps running it after the cluster moves on. `tsh version` shows it
as `Re-executed from version: …`, and `prism doctor`'s `tsh version`
line names both versions and the fix. Usually that is `tsh login` again,
which records the cluster's current version. If the cluster itself asks
for an old one, `TELEPORT_TOOLS_VERSION=off` makes tsh use the installed
copy; set it for prism's service too, since the daemon keeps the list.

### tsh session expired

Run `tsh login` — the daemon detects the refreshed identity and
restarts its subprocesses automatically. If you're tired of this,
switch to tbot (see above).

### Claude Code says `Not logged in · Please run /login`

You don't need a Claude subscription to use prism, and you shouldn't
have to log in — see
[Claude Code authentication](#claude-code-authentication-no-subscription-needed).
If you see this, upgrade first (`brew upgrade prism`, or rebuild from
source); older versions deliberately cleared Claude Code's credential
variables and left this gate in place. `prism claude` then supplies the
dummy gateway token on its own, with nothing to configure.

If it persists, you're in forward-proxy mode with a stored login that
prism takes for a working one. Force the token:

```bash
prism config set claude_auth token
```

Check what prism is reading with `claude auth status --json` — a
`"loggedIn": true` with a `"subscriptionType"` that no longer matches
reality is exactly the case that override exists for. Don't set
`ANTHROPIC_API_KEY` yourself to work around it: Claude Code treats an
environment API key as untrusted and raises a *"Detected a custom API
key … use this API key?"* prompt whose pre-selected answer is **No**,
which lands you back in the browser login flow.

On a machine whose managed settings pin sign-ins, neither the token nor
this override lifts the gate — see below.

### Claude Code says `This machine's managed settings require a first-party login`

```
This machine's managed settings require a first-party login: they pin
Anthropic sign-ins to your organization (forceLoginOrgUUID), and this
session is configured with a bearer token from ANTHROPIC_AUTH_TOKEN,
which cannot be checked against that policy (requests go through
127.0.0.1:7331, per ANTHROPIC_BASE_URL).
```

The bearer token is prism's. Upgrade (`brew upgrade prism`, or rebuild
from source): prism reads the policy and runs Claude Code as a
third-party platform client instead, which the pin never applies to —
see
[Managed machines that pin sign-ins](#managed-machines-that-pin-sign-ins).
No login required.

If the error survives the upgrade, the pin is reaching Claude Code by a
route prism doesn't read (a `policyHelper`, or the Windows registry).
Ask for platform mode outright:

```bash
prism config set claude_auth platform
```

### My client says Opus but the replies look like Sonnet

The cluster gateway serves a fixed set of models and quietly rewrites a
request for anything else to its fallback. Check `prism logs` for a
`[!]` marker, or `prism usage` for the `Substituted by gateway` section;
both name the model that actually answered. See
[When the gateway serves a different model](#when-the-gateway-serves-a-different-model).

Nothing is broken on prism's side — the response really did come from
the model it names, and asking for a model the cluster does serve gets
you that model.

### Codex says `Incorrect API key provided: teleport`

Symptom on Codex 0.165 and later:

```
ERROR: Reconnecting... 4/5
  Unexpected status 401 Unauthorized: Incorrect API key provided:
  teleport. … url: wss://api.openai.com/v1/responses
```

Codex is talking to OpenAI directly, with the dummy key prism hands out.
It doesn't honour `OPENAI_BASE_URL` — not just in 0.165; the same thing
happens on versions as old as 0.145 — and from 0.165 its built-in
`openai` provider also opens a WebSocket to
`wss://api.openai.com/v1/responses`, which no base URL redirects and
which falls back to HTTPS against the same host.

Upgrade prism (`brew upgrade prism`, or rebuild from source) and use
`prism codex`: it registers a provider pointing at the router on Codex's
command line, with WebSockets disabled. If you run `codex` directly, add
that provider to `~/.codex/config.toml` — see
[Daily use](#daily-use) for the block to paste.

### `prism codex doctor` reports the provider unreachable

```
✗ reachability  prism API route probe  … returned HTTP 404 (required)
```

Expected, and harmless. Codex probes `GET <base_url>/models` and the
Teleport gateway only serves `/v1/responses`, answering anything else with
`unsupported endpoint`. Inference is unaffected; the same report's
`websocket` row should read *"Responses WebSocket is not enabled for the
active provider"*, which is the line that matters.

### Remote Control disabled / `Remote Control environments are not available`

Enable forward-proxy mode — see
[Claude Code Remote Control](#claude-code-remote-control) above. Both
interactive sessions and headless `prism claude rc` work in that mode.
Remote Control also needs a real claude.ai login, so if `prism claude`
is supplying its dummy gateway token (the default without a usable
login), Remote Control stays unavailable regardless of this mode.

### `status=203/EXEC` after upgrading prism (service won't start)

```
Process: 320835 ExecStart=/home/linuxbrew/.linuxbrew/Cellar/prism/0.1.21/bin/prism __daemon (code=exited, status=203/EXEC)
Active: activating (auto-restart) (Result: exit-code)
```

The service definition recorded a version-pinned path to the prism
binary and `brew upgrade` deleted that directory. systemd retries it
every 5s until it hits the start limit; launchd quietly never spawns the
job. Either way prism looks installed and isn't.

`prism up` repairs this on its own now — it rewrites the unit or plist
to a path that survives upgrades (Homebrew's `opt` symlink, or whichever
bin directory holds prism), clears the systemd start limit, and starts
the service:

```
prism: repairing systemd-managed service definition (/home/linuxbrew/.linuxbrew/Cellar/prism/0.1.21/bin/prism no longer exists)
```

`prism status` reports it before anything else, and `prism install`
rewrites the definition on demand. Definitions written by prism 0.1.28
and earlier carry the versioned path; they are rewritten the first time
any of those three commands runs, whether or not the upgrade has broken
them yet.

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

The router (`internal/router/`) dispatches by path; the shared scrub
package (`internal/scrub/`) makes requests Bedrock-compatible (strips
fields like `thinking` and `metadata`, sanitizes `cache_control`, caps
non-streaming `max_tokens` at 8192 — Bedrock requires streaming above
that). For OpenAI requests, it renames `max_tokens` to
`max_completion_tokens`, which newer models require. `/v1/chat/completions`
is handled by `internal/chatcompat/` (see
[chat/completions shim](#chatcompletions-shim)). The router also
captures token usage from responses into `~/.config/prism/usage/`. The
tunnels are `tsh proxy app` subprocesses, or — in tbot mode — a single
`tbot start` with two `application-tunnel` services.

In forward-proxy mode (see
[Claude Code Remote Control](#claude-code-remote-control)), the same
router also accepts HTTPS `CONNECT` and plain forward-proxy requests:
`api.anthropic.com` model calls are intercepted and sent through the
anthropic tunnel with the same scrubbing, and everything else passes
through untouched.

---

## Repo layout

```
cmd/prism/             local CLI + daemon
internal/router/       HTTP router with path dispatch + usage capture
internal/scrub/        Bedrock/OpenAI request scrubbing (shared)
internal/mitm/         forward proxy for Remote Control compatibility
internal/tunnel/       subprocess supervisor
internal/tbot/         tbot config rendering and bootstrap
internal/identity/     tsh session expiry watcher
internal/state/        runtime state persistence
internal/config/       persistent machine config
internal/usage/        token usage tracking (JSONL writer + reader)
internal/logfile/      rotating daemon log writer
internal/tshwrap/      tsh CLI wrappers
Makefile               build targets for all platforms
```

---

## License

Apache 2.0 — see [LICENSE](./LICENSE).
