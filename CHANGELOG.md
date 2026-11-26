# Changelog

All notable changes to prism, newest first. Dates are release dates.

Each version links to its full commit range. Upgrading generally means
`prism down && prism up` so the daemon picks up the new binary — entries
call out when more than that is needed.

## Unreleased

### Changed

- **Each Pi session gets a beam of its own on Claude models too.** Pi sends
  no session id on Anthropic requests unless asked, so its sessions in a
  repository shared one beam. `prism setup pi` and `prism pi` now turn on
  `compat.sendSessionAffinityHeaders` for the Anthropic models they route
  through prism, Pi's own and the "via prism" ones, so Pi sends its session
  id and a resumed session goes back to its beam. A model where you set it
  to `false` in `models.json` keeps your choice. Thanks to @ibeckermayer
  (#6).

## [v1.0.0-beta.5] — 2026-11-25

**A pre-release, not on Homebrew.** `brew upgrade` stays on v0.5.0. To try
it, download your platform's binary from this release with `curl -L`, make
it executable, and run that copy's `prism down && prism up` (it takes over
the background service). Coming from beta.3 or beta.4, the next idle
`prism claude` restarts the daemon for you. To go back, run Homebrew's
`prism down && prism up`.

### Changed

- **statusline separates the Claude and OpenAI limits with a divider.**
  Without colour (a stale reading) they used to run together. Existing
  setups gain it at the next `prism claude`.
- **`prism login expired` appears once, on the account segment**, after
  the beam or cluster name (`fresh-beans • prism login expired`), instead
  of as `(tsh login expired)` on both limits beside their `(stale)`. It
  names `prism login`, the command that fixes it. It shows even with
  `statusline_beam false`, which now blanks the segment rather than
  removing it, so the segment stays in statusline's settings whatever the
  setting says.

### Fixed

- **`prism config set statusline_beam false` takes effect at once.** It
  used to remove statusline's account segment only at the next `prism
  claude`, so sessions already running showed the cluster's name in its
  place until then. Every session now follows within a minute, at its
  next refresh.
- **Pi 1.5.0 sessions each get a beam of their own again.** That Pi spells
  its session header `session_id`, which prism didn't recognise, so all its
  sessions in a repository shared one beam. A session keeps its beam when Pi
  is upgraded to the newer spelling. Thanks to @ibeckermayer (#5).

## [v1.0.0-beta.4] — 2026-11-23

**A pre-release, not on Homebrew.** `brew upgrade` stays on v0.5.0. To try
it, download your platform's binary from this release with `curl -L`, make
it executable, and run that copy's `prism down && prism up` (it takes over
the background service). Coming from beta.3, the next idle `prism claude`
restarts the daemon for you. To go back, run Homebrew's `prism down &&
prism up`.

### Added

- **statusline shows the beam recording your session.** If Claude Code's
  status line is [statusline](https://github.com/ryanclark/statusline),
  `prism claude` now adds the session's beam beside the tenant's quota:
  `beam: quiet-otter`, `beam: starting…` while it comes up, or `beam: none
  yet` before the first request. A session that isn't recorded says so,
  as `unrecorded`, or `unrecorded (--no-beam)` for a `prism --no-beam`
  launch, so it's hard to miss. It is the session's own beam, including
  one it shares when `beam_max` is reached, and it catches up within a
  minute when a beam starts or is replaced. It appears in statusline's
  `account` segment, which the first `prism claude` adds in front of the
  quota segments.
- **`statusline_beam`** chooses what that segment shows: `true` (the
  default) the beam, `cluster` the cluster's name as before, `false`
  nothing, with the segment taken out of statusline's settings and kept
  out.

## [v1.0.0-beta.3] — 2026-11-23

**A pre-release, not on Homebrew.** `brew upgrade` stays on v0.5.0. To try
it, download your platform's binary from this release with `curl -L`, make
it executable, and run that copy's `prism down && prism up` (it takes over
the background service). To go back, run Homebrew's `prism down && prism
up`.

**prism now records every agent session in Teleport Beams, and sends
inference only that way.** Policy asks for every pull
request with AI-generated code to link to a session recorded in Beams;
with this release you get one without working in a beam. How to upgrade:

- **If prism uses your `tsh login`** (the default): after `brew upgrade
  prism`, run `prism down && prism up` once. That's all: from then on,
  upgrades restart the daemon themselves when it's idle, and the launchers
  install the agent skill.
- **If prism uses tbot**: nothing changes until you switch, and prism
  reminds you each time you run it. Switch with `prism setup beams` (the
  same command works on any machine). It adds your own Teleport login
  alongside tbot, checks you can use Beams, and turns recording on, without
  a restart.

### Added

- **Every agent session's inference goes out through a Teleport beam**, so
  the session is recorded, summarised and classified in Beams as if it
  had run there. Your agent, code and tools stay on this machine; only the
  LLM traffic passes through the beam.
  - Each session gets a beam of its own: three Claude Code tabs make three
    recordings. A subagent shares its parent's beam.
  - The beam starts with the session's first request (about 15 seconds).
    `prism claude` and the other launchers start it before the agent
    opens, so you don't wait.
  - A beam is ended, and summarised, after two hours unused. Work running
    past a beam's 11-hour life moves to a successor without a pause.
  - At most 10 beams run at once (`beam_max`). At that limit, a session
    shares a beam with another session from the same repository.
- **No beam, no inference.** When a beam can't be started, usually because
  your Teleport login has run out, prism refuses the request instead of
  sending it unrecorded. Your agent shows why, with a link that logs you
  back in, and the next request carries on. The link works from a VM too:
  Lima forwards it to your Mac's browser.
- **`prism login`**: `tsh login` to prism's cluster, for as long as your
  roles allow. The launchers run it for you when your login has lapsed.
- **`prism beam link --pr`** prints a section for the PR description,
  listing the beams that carried this repository's work. The `prism-beams`
  skill teaches Claude Code, Codex, OpenCode and Pi to add it when they
  open a pull request. The launchers keep it installed and current; `prism
  setup skill --remove` removes it for good.
- **Upgrades apply themselves.** A launcher that finds the daemon older
  than itself and carrying no requests restarts it, on the same port, so
  `brew upgrade` is the whole upgrade. (Not the first time: a daemon from
  v0.5 or earlier can't say whether it's busy, so that one still needs
  `prism down && prism up`.)
- **`prism beam status`, `prism beam end` and `prism beam test`**:
  - `status` lists the beams running and your login;
  - `end` ends this repository's beams now, so their summaries come
    sooner;
  - `test` starts a beam of its own, proves inference works through it,
    and removes it.
- **`prism status` and `prism doctor` report recording**, and say how to
  fix it when it isn't happening.
- **Desktop notifications** when inference pauses for a login, or about to:
  shortly before your login runs out ahead of a beam's replacement.
- **Settings for power users**, all applied without a restart:
  - `beam_scope repo`: one beam per repository instead of per session;
  - `beam_max`: how many beams may run at once;
  - `beam_roles`: delegate only these roles to each beam (`beam-user` is
    enough for inference);
  - `beam_auto_login`: for an identity provider that signs you in without
    a prompt. The daemon logs you in itself and holds the request
    meanwhile;
  - `beam_mode off`: inference that needn't be recorded.

### Fixed

- **Claude Code's startup check no longer goes to the gateway.** Its
  `HEAD /api/hello` is answered by prism, exactly as the gateway would
  answer it. Sent through a beam, it marked every recording "Recording
  incomplete".

## [v0.5.0] — 2026-11-23

### Added

- **Works with [statusline](https://github.com/ryanclark/statusline)**
  (`brew install ryanclark/tap/statusline`). If Claude Code's status line
  is statusline, as its `statusLine` command or its plugin, `prism claude`
  now shows the tenant's token quota there with no setup:
  `Claude 3% 2h41m  OpenAI 3% 2h41m`. Only sessions started with
  `prism claude` get it, so your other Claude Code sessions keep their
  claude.ai usage. The first launch adds two segments to
  `~/.statusline/settings.json` (backed up to `.bak`). They stay blank in
  other sessions. `prism setup statusline` makes prism the usage source
  in every session instead.

  **If it says `(tsh login expired)` and `(stale)`, run `tsh login`
  again.** The quota is read with your login, and without it statusline
  shows prism's last reading plus this machine's usage since, marked `≈`.

  **To undo it**, run `prism setup statusline --remove`. That takes
  prism's segments (and its usage source, if `prism setup statusline`
  added one) back out of `~/.statusline/settings.json`, and turns off
  `statusline_auto` so the next `prism claude` doesn't put them back.
  To keep the segments but stop `prism claude` from touching statusline,
  run `prism config set statusline_auto false` instead.
- **The tenant's token quota in `prism usage`, `prism status`,
  `prism status --json` and `prism doctor`.** Beams tenants cap tokens
  per LLM app, daily or for the tenant's lifetime, and refuse requests
  once a cap is reached. Nothing showed how close you were. prism now
  reads the counters with your tsh login (never starting a sign-in) and
  shows usage against the limit, with a percentage, the reset time and
  this machine's share. Where the counters can't be read (an expired
  login, say), it estimates, marked `≈`: the last reading plus this
  machine's usage since, or this machine's alone once that window resets.
  `prism usage --refresh` reads them now.
  `prism config set quota_limits anthropic=5M,openai=5M` overrides the
  assumed limits. A request refused for quota is logged with
  `quota=exhausted`.

  This works on tbot machines too. prism's tbot identity can't read the
  counters, so the daemon reads them with your tsh login when you have a
  live one, and otherwise `prism usage` says why it can't.

### Fixed

- **Usage records name the configured cluster.** A shell still exporting
  another cluster's `TELEPORT_PROXY` when the daemon started labelled every
  usage record with that cluster. The configured `proxy` now wins.

## [v0.4.1] — 2026-11-15

### Fixed

- **An empty model list now says why.** With Teleport's managed
  client-tools updates on, an installed tsh can run an older version —
  the one recorded at your last `tsh login` — and a tsh before 18.8.3
  lists the cluster's apps without their models. `prism models` used to
  answer "No LLM apps visible"; it now names the tsh that ran, the one
  you installed, and the fix (usually `tsh login` again). `prism doctor`
  has a `tsh version` line that checks the same thing, which until now it
  passed over silently while model discovery found nothing.
- **`prism claude -p` from a script shows Claude Code's own errors.** On
  a machine whose managed settings pin Anthropic sign-ins, prism printed
  a note about running Claude Code as a platform client ahead of
  everything else on stderr, so a caller that keeps the first line of a
  failure saw the note and not the error. The note is now printed only
  when stderr is a terminal, by `prism claude` and `prism env` alike.

## [v0.4.0] — 2026-11-06

### Added

- **`prism setup opencode` and `prism setup pi` add every model your
  cluster serves**, as entries named "<model> via prism" — the same list
  `prism models` shows, under providers of prism's own (`prism-anthropic`,
  `prism-openai`, `prism-openai-chat`), so the tools' built-in providers
  are left alone. Running it again only adds new models and keeps the
  base URL in step with the router port; entries you've edited stay as
  you left them. `--overwrite` replaces every provider in the file with
  prism's only, after saving a `.bak` copy. `prism opencode` and
  `prism pi` add any new models each time they start.

### Changed

- **`prism pi config` is now `prism setup pi`.** The old name hid Pi's
  own `pi config` command, and anything after `prism pi` should be Pi's.
  `prism pi config` still works for now, with a notice; in a future
  release it will open Pi's own command instead. Until then that is
  `prism exec pi config`.

### Fixed

- **OpenCode works through prism again.** OpenCode 2 ignores
  `ANTHROPIC_BASE_URL` and `OPENAI_BASE_URL`, so `prism opencode` sent
  requests straight to Anthropic and OpenAI with prism's placeholder key
  and failed with `invalid x-api-key` or `Incorrect API key provided:
  teleport`. prism now writes its models into OpenCode's config file,
  editing it in place so your comments and other settings are kept.

- **Kimi and GLM work in agents.** Tool calls to a model served only on
  `/v1/chat/completions` used to be refused by prism's chat/completions
  shim. They are now sent on as they are, so those models can run tools
  in OpenCode and Pi.

- **Pi's GPT models work through prism.** Pi sent the thinking level in a
  form the gateway rejects ("The 'configuration_update' item type is not
  supported with this model"). prism's Pi entries now have Pi send it
  the ordinary way, so the level you pick still applies.

## [v0.3.0] — 2026-11-04

### Added

- **`prism models` shows which models your cluster serves**, for both
  apps, along with the model `prism test` would send to each and why it
  picked that one. `--json` is for scripts, and `--cached` shows the
  daemon's copy of the list and how old it is, without asking the
  cluster. Reading the list needs tsh 18.8.3 or newer.

- **`prism codex` offers only models the cluster serves.** Codex's
  `/model` picker used to list models the gateway refuses with a 400.
  prism now gives Codex the part of its own model list the `openai` app
  actually serves. If that can't be worked out (no model list yet, an
  older tsh, Codex not answering), prism prints one line and Codex
  starts with its usual list, so this never stops a launch.
  `prism config set model_discovery false` turns it off. Thanks to Tener
  for the original feature (#3).

- **The daemon keeps a copy of the cluster's model list**, so nothing
  waits on the cluster when a tool starts. It is refreshed shortly
  after the daemon starts and then hourly, and a laptop waking from
  sleep catches up straight away rather than an hour later. On a tbot
  machine the list is read with tbot's own identity, so no `tsh login`
  is needed. That identity is a separate file in the tbot directory,
  valid for 20 minutes at a time, so a copied file stops working almost
  at once. `prism down && prism up` after upgrading starts it.

- **`PRISM_QUIET=1` keeps prism off stdout**, for programs that parse a
  tool's stdout, such as one driving
  `claude -p --output-format stream-json`. `prism up` skips its `export`
  lines, and the `tsh app login` and `pi update --models` that prism
  runs before a launch don't read stdin and write to stderr instead.

- **`prism status --json`** prints one JSON object saying whether prism
  is ready and, if not, why and what to run. It always exits 0 and is
  cheap enough to run before every job.

- **`PRISM_CLAUDE_BIN`, `PRISM_CODEX_BIN`, `PRISM_OPENCODE_BIN` and
  `PRISM_PI_BIN`** run a tool from an absolute path instead of the one
  on `PATH`, with the same setup. `prism env` checks the same `claude`
  when deciding Claude Code's credential. A bad path stops the launch
  with an error instead of falling back to `PATH`.

  Thanks to Ryan Clark for these three (#4).

### Changed

- **`prism claude` and `prism env` no longer print a model-swap warning
  at startup.** Since `prism env` runs in every new shell, the same
  swaps were repeated in every terminal for a day. The request log's
  `[!]` marker and the "Substituted by gateway" section of `prism usage`
  still show them.

- **Test requests take their model from the cluster's list.** With no
  flag or config key set, `prism test`, `prism doctor` and
  `prism tbot reset` use a model the cluster says it serves, preferring
  the built-in defaults when they're on the list. This supersedes
  v0.2.2's note that the OpenAI test requests send no model: they now
  default to `gpt-6-luna`.

- **tbot is restarted only when one of the two tunnels is down.**
  prism used to watch tbot's overall health, which also counts the new
  model-list identity, so a problem with that alone would have dropped
  both tunnels.

### Fixed

- **`prism test` passes on a gateway with no default model.** The
  OpenAI test requests sent no model, which such a gateway refuses with
  `requested model "" is not supported`, so `prism test` failed out of
  the box on a working setup.

## [v0.2.2] — 2026-11-03

### Added

- **`prism tbot reset` now fixes a locked bot.** When Teleport sees a
  bound-keypair identity renewed out of order, it assumes a stolen
  certificate and locks the bot's join token:

  ```
  Bot joins via the token "prism-bot-token-<host>" have been locked due to
  a certificate generation mismatch …, possibly indicating a stolen certificate.
  ```

  That lock targets the token's *name*, and a reset recreates the token
  under the same name, so until now a reset couldn't get past it and
  failed at its last step. Reset now removes that lock as part of the
  rebuild, and the plan it prints says so. A lock someone placed by
  hand is never removed: the reset stops before changing anything and
  names it.

  Nothing has to have been copied for this to happen. A renewal whose
  reply is lost while a laptop sleeps is enough, as is a second tbot left
  running on the same identity (see Fixed).

- **`prism doctor` reports your tbot's version against the cluster's.**
  A new `tbot version` line fails when tbot is older than the cluster
  accepts, and warns when it's a newer major version than the cluster,
  or when the tbot actually running differs from the one installed —
  `brew upgrade teleport` doesn't touch a tbot that's already running.

- **Kimi K2.5, GLM 5 and the GPT models all work from
  `/v1/chat/completions` at once.** Some models exist only on the Chat
  Completions API and others only on the Responses API, so no setting of
  `openai_chat_completions_shim` could reach both. prism now learns from
  the gateway's own error which API each model is served on, remembers
  it, and sends the model there. The setting still chooses where a model
  is tried first.

- **`prism config set anthropic_test_model <id>` and
  `openai_test_model <id>`** choose the model `prism test`,
  `prism doctor` and `prism tbot reset` use for their test requests, so
  a gateway that stops serving the default needs a config change rather
  than a new prism. `prism test` also takes `--anthropic-model` and
  `--openai-model`, and `prism doctor` takes `--model`.

### Changed

- **The test requests name `claude-sonnet-5` by default**, instead of
  sending no model and letting the gateway pick. Gateways without a
  default model refuse that outright, and Claude Sonnet 5 is served both
  before and after the latest Beams model update. The OpenAI test
  requests still send no model unless `openai_test_model` names one.

- **A model the gateway won't serve is a warning in `prism doctor`, not
  a failure**, and no longer fails the last step of `prism tbot reset`.
  Either way the request reached the gateway, so nothing on your machine
  is broken. Both say which config key to set. Other errors, like the
  provider refusing access, still fail.

### Fixed

- **A tbot left behind by a crashed daemon can no longer get your bot
  locked.** On macOS and Linux, a daemon that died uncleanly left its
  tbot running, and that tbot kept renewing alongside the next daemon's
  until Teleport locked the token. prism now stops any tbot still running
  on its own config before starting a new one, giving it time to finish
  a renewal first. If that tbot belongs to a prism daemon that's still
  running, the new daemon refuses to start instead. This replaces the
  old advice to `pkill -x tbot`.

- **`prism test`, `prism doctor` and `prism tbot reset` work on a
  gateway with no default model.** All three used to fail with
  `requested model "" is not supported`: doctor reported a failure and
  exited non-zero on a machine with nothing wrong, and reset waited 90
  seconds on an answer that could never change, then reported a
  successful reset as failed.

- **No more false "Substituted by gateway [!]" for OpenAI models.** The
  gateway answers `gpt-6-luna` as `openai.gpt-6-luna` (and `glm-5` as
  `zai.glm-5`), which prism counted as a model swap in the request log,
  `prism usage` and the `prism claude` warning. A vendor prefix is no
  longer a swap, and records already in your usage log stop counting.

## [v0.2.1] — 2026-11-03

### Changed

- **prism now tells Teleport which cluster it means, every time.** `tsh`
  and `tctl` act on whichever profile your shell last logged into, which
  is not necessarily the cluster prism is configured for. prism passes
  `--proxy` to tsh and `--auth-server` to tctl on every call, so nothing
  depends on your current login context and you never have to change it
  to run a prism command.

  This mattered most in `prism tbot reset`: logged into another cluster,
  it would have created this machine's bot, role and token there, and
  the only symptom would have been tbot failing to join at the last step
  — after leaving three resources behind on a cluster nobody asked
  about. It now acts on prism's own cluster and says which one, and its
  preflight check asks whether you have a live login *to that cluster*,
  naming the exact `tsh login --proxy=…` to run if not.

- **prism's configured proxy is the authority for which cluster a
  machine belongs to.** `prism tbot bootstrap` and `prism tbot reset`
  resolve the cluster from `--proxy`, then `prism config`, then the
  machine's current tbot config, and only consult your tsh login when
  nothing is configured at all. So a machine set up for one cluster
  never quietly follows a `tsh login` somewhere else, and **moving to a
  different cluster means `prism config set proxy <host:port>` first**,
  then `prism tbot reset`. Reset's plan says when it is moving a machine
  between clusters rather than repairing one, and leaves the old
  cluster's resources alone.

### Fixed

- **"Not logged in" no longer reads as "expired at 0001-01-01".** An
  expired or missing Teleport login now produces a plain sentence naming
  the cluster and the command to fix it, in `prism doctor`,
  `prism status` and `prism tbot reset`, instead of tsh's raw error text
  (complete with terminal colour codes) or a zero timestamp.

## [v0.2.0] — 2026-11-03

### Added

- **`prism doctor` checks everything and tells you what is wrong.** One
  read-only report covering the service definition, the daemon and its
  router, the identity (tsh login, or the whole tbot chain: directory,
  machine binding, bot name, join secret, proxy reachability, tbot's own
  live/ready state, stray tbot processes) and finally a real request
  through the router to see whether inference actually works. Every line
  carries its own fix, the last line is the verdict, and the exit code is
  non-zero when something failed, so it can be scripted.

  When a tbot health check fails it also quotes the last error tbot
  wrote to the log, which is the only place the actual reason lives —
  tbot's own status endpoint just says "initializing" — so a report
  says "could not lookup signer for public key …" rather than leaving
  you to guess.

  It is written to be pasted to whoever is helping you: it names the
  version, platform, cluster and failure, and never prints your
  registration secret or anything from a request. `--no-request` skips
  the live request.

  `prism status` now ends with a pointer to it whenever something it
  shows isn't healthy.

- **`prism tbot reset` rebuilds a machine's Machine ID identity from
  scratch.** For when tbot is broken and the answer is "start again":
  stop prism, delete this machine's bot, role and token from the
  cluster, clear the local tbot state, create all of it again, restart
  prism, and send one request through it to prove the result works —
  eight steps, each reported as it happens.

  Everything is scoped to this machine's hostname and derived from it,
  never from whatever the local config happens to name, so it cannot
  touch another machine's bot. It prints the plan and asks before
  changing anything (`--dry-run` to see the plan, `--yes` to skip the
  prompt), checks up front that `tctl`, `tbot` and a live `tsh login`
  are there rather than failing halfway, and leaves per-session MFA
  prompts answerable. It brings prism back up on the port it was using,
  and waits for the tunnel before declaring success — tbot joins a few
  seconds after the router starts listening.

- **prism notices when its tbot identity was copied from another
  machine.** A Mac cloned with Migration Assistant inherits prism's tbot
  directory — a Teleport bound keypair issued to the original machine —
  and a LaunchAgent that starts prism at login. Two machines renewing one
  bound keypair can lock the bot and break prism on the original machine,
  and nothing used to notice. prism now records which machine an identity
  was issued to and refuses to start tbot on another one, pointing at
  `prism tbot reset`.

  The binding is recorded the first time a machine runs this version, so
  a clone is only recognised if the machine it was cloned *from* had run
  it at least once. On Linux the check is weaker than on macOS: it reads
  `/etc/machine-id`, which a disk-image clone copies verbatim.

### Fixed

- **`--help` no longer acts on the commands that change things.**
  `prism down --help` stopped the daemon, and `prism uninstall --help`
  removed the system service and told you afterwards. Both now print
  help, as does `prism install --help`.

- **`prism tbot bootstrap` no longer tells a cloned machine to delete the
  original's cluster resources.** It printed `tctl rm` for the old token,
  role and bot whenever the local names didn't match the hostname — right
  after a rename, wrong after a clone, where the other machine is still
  alive and using them. It now tells the two apart and only suggests
  removal for a machine that was renamed.

- **The system service no longer breaks when prism is upgraded.**
  `prism install` recorded the fully resolved path to the prism binary,
  which under Homebrew means a version-pinned one
  (`…/Cellar/prism/0.1.21/bin/prism`). The next `brew upgrade` deleted
  that directory and left the service pointing into it: systemd retried
  it every five seconds and reported `status=203/EXEC` until it hit the
  start limit, launchd simply never spawned the job. Either way prism
  looked installed and wasn't, with no hint as to why.

  Service definitions now record a path that survives an upgrade —
  Homebrew's `opt` symlink, or whichever bin directory holds prism — and
  prism repairs an installed definition that has gone stale rather than
  waiting to be reinstalled:

  - `prism up` rewrites the unit or plist before starting the service,
    clears the systemd start limit a crash loop leaves behind, and
    reloads the plist launchd was holding.
  - `prism up` also repairs a definition mid-session when the recorded
    path is already gone or names a version an upgrade will remove,
    even if the daemon it describes is running fine, since nothing else
    notices before the next login.
  - `prism status` reports a stale definition before anything else, and
    `prism install` rewrites one on demand.

  Definitions written by earlier versions carry the versioned path and
  are rewritten the first time any of those commands runs — on macOS as
  well as Linux, where the breakage is the same but quieter.

  Upgrading from a broken state needs nothing but `prism up`.

## [v0.1.28] — 2026-10-30

### Added

- **prism now tells you when the cluster gateway served a different model
  than you asked for.** The Teleport LLM app only serves the models its
  cluster is configured for. Ask for anything else and it quietly
  substitutes the cluster's fallback: the request succeeds, nothing in the
  response mentions it, and your client goes on reporting the model it asked
  for — Claude Code also costs the call at that model's list price. Claude
  Code's own default, `claude-opus-5-5`, is absent from some clusters'
  lists, so a stock install can spend all day labelling (and pricing)
  `claude-sonnet-5` replies as Opus 5.5.

  prism is the only place the requested and served names exist side by side,
  so it flags the mismatch in three places:

  - the request log, where a substituted call gains `requested=` and a `[!]`:

    ```
    POST /v1/messages 200 req=747024B resp=2073B requested=claude-opus-5-5 model=claude-sonnet-5 [!] in=2 out=89 cache_read=18807 cache_write=209741 2.721s
    ```

  - `prism usage`, which grows a section counting requests per pair:

    ```
    Substituted by gateway  [!]                        Requests
    ────────────────────────────────────────────────────────────
    claude-opus-5-5 → claude-sonnet-5                      1204
    openai.gpt-6-astra → openai.gpt-5.6-luna                 37
    ```

  - `prism claude` and `prism env`, which print one line at launch when it
    happened in the last 24 hours.

  `model=` still carries the model that actually answered, exactly as before,
  so anything parsing that field is unaffected and an honoured request's log
  line is unchanged. Records in `prism usage --json` gain a
  `requested_model` field, present only when a substitution happened.

  prism deliberately does **not** repoint your client at the served model.
  That would mean prism picking models on your behalf, and replacing a silent
  substitution with a different silent remap is not a fix. To learn what your
  cluster actually serves, ask for a model and read `model` back out of the
  reply — a name that returns unchanged is served, anything else is the
  fallback.

  The `[!]` marker and the `requested=` field come from the daemon, so they
  start appearing after `prism down && prism up`. The `prism usage` section
  and the launch-time line work as soon as the new binary is in place.

## [v0.1.27] — 2026-10-29

### Fixed

- **`prism claude` works on a managed machine that pins Anthropic
  sign-ins, with no login at all.** A machine whose managed settings require
  sign-ins to belong to one organization (`forceLoginOrgUUID`) or use one
  method (`forceLoginMethod`) rejects every Anthropic credential Claude Code
  is given, and the dummy gateway token prism supplies is one of them, so
  Claude Code refused to start:

  ```
  This machine's managed settings require a first-party login: they pin
  Anthropic sign-ins to your organization (forceLoginOrgUUID), and this
  session is configured with a bearer token from ANTHROPIC_AUTH_TOKEN,
  which cannot be checked against that policy (requests go through
  127.0.0.1:7331, per ANTHROPIC_BASE_URL).
  ```

  Simply dropping the token is no better — Claude Code then stops at `Not
  logged in · Please run /login` under the same policy. What a pin polices is
  *first-party* sign-in, so prism now stops being a first-party client there:
  it reads the same managed-settings files Claude Code reads and, on a pinned
  machine, runs Claude Code as a third-party platform client pointed at the
  local router with no Anthropic credential at all — the shape a Teleport beam
  uses against the very same cluster gateway. **No sign-in, nothing to
  configure**; upgrading is the whole fix.

  On the wire nothing changes (`POST /v1/messages`, model in the body), so
  scrubbing, usage capture and the request log behave as before.
  `claude auth status` reports `"authMethod": "third_party"`. Remote Control
  stays unavailable on such a machine, since it needs the first-party session
  the pin forbids.

  Managed settings that say nothing about sign-ins change nothing: the gateway
  token added in v0.1.25 is what lets prism work with no Anthropic account,
  and it stays. New `prism config set claude_auth platform` asks for the
  third-party provider outright, for a pin that reaches Claude Code by a route
  prism doesn't read (a `policyHelper`, or the Windows registry).

## [v0.1.26] — 2026-10-28

### Fixed

- **`prism codex` points Codex at prism again.** Codex doesn't honour
  `OPENAI_BASE_URL`, so every session went to OpenAI directly with the dummy
  key prism hands out. On 0.165 that surfaced as
  `Incorrect API key provided: teleport … url:
  wss://api.openai.com/v1/responses`, retried five times and then repeated
  over HTTPS against the same host; on older versions the same thing happened
  with a quieter error. `prism codex` now registers a provider pointing at the
  local router on Codex's own command line, so there is nothing to configure —
  upgrading is the whole fix. Verified against Codex 0.145, 0.155, 0.164 and
  0.165.

  WebSockets are explicitly disabled on that provider. Codex 0.165's built-in
  `openai` provider opens one to `wss://api.openai.com/v1/responses`, which no
  base URL redirects, and prism's router speaks plain HTTP. Overriding the
  built-in provider isn't possible either: Codex only accepts `base_url`,
  `auth` and `http_headers` there, and `base_url` alone doesn't move the
  WebSocket endpoint.

  prism's overrides are passed before your arguments, and Codex lets the
  last value for a key win, so `prism codex exec -c model_provider=openai`
  still reaches the real API if that's what you want.

  `prism env` can't express a provider definition, so a bare `codex` — or
  `prism exec codex` — needs the same block in `~/.codex/config.toml`. The
  [README](https://github.com/webvictim/prism#daily-use) has it, along with
  a note on the one `prism codex doctor` row that fails harmlessly (Codex
  probes `GET /models`, which the Teleport gateway doesn't serve).

## [v0.1.25] — 2026-10-16

### Fixed

- **`prism claude` no longer demands a Claude subscription.** On a machine
  that had never run Claude Code — or one whose claude.ai login had lapsed,
  been downgraded or cancelled — every session stopped at
  `Not logged in · Please run /login`, even though prism needs no Anthropic
  credential at all: the Teleport LLM app authenticates to the provider
  cluster-side and prism strips whatever the client sends. Claude Code simply
  refuses to send a request until it believes it holds a credential of its
  own. `prism claude` (and `prism env`) now supply a dummy gateway token,
  `ANTHROPIC_AUTH_TOKEN=teleport`, discarded on the way out exactly like
  `OPENAI_API_KEY`. Installing prism and running the setup steps is now the
  whole story; nothing to configure, and upgrading is the entire fix for an
  affected machine.

  A working claude.ai login in forward-proxy mode is left untouched, since
  Remote Control needs a genuinely signed-in session: prism asks
  `claude auth status --json` and only injects the token when the answer is
  "no usable login". That report is Claude Code's cached view, so it can't
  tell a dead account from a live one — if a stale login still looks usable,
  `prism config set claude_auth token` forces the token. `claude_auth login`
  does the opposite and never injects.

  It is deliberately a token and not `ANTHROPIC_API_KEY`: Claude Code treats
  an environment API key as untrusted and raises a *"Detected a custom API
  key … use this API key?"* step whose pre-selected answer is **No**, which
  drops the user into the browser login flow — the very thing this fixes.

### Added

- **`prism config set claude_auth auto|token|login`** — overrides which
  credential `prism claude` hands Claude Code. See
  [Claude Code authentication](README.md#claude-code-authentication-no-subscription-needed).

## [v0.1.24] — 2026-10-01

### Fixed

- **Canceled requests no longer look like gateway outages in the log.** A
  client that drops its own request — Claude Code abandoning a stale or
  speculative call, which it does routinely — was reported as
  `router: anthropic upstream error: ... context canceled` and answered
  with a 502, exactly as if the cluster gateway had failed. This sent
  people hunting for tunnel problems that weren't there; one report
  pinned it on Claude Code's Stop hook, which turned out to have no
  network path to prism at all. Cancels now log a distinct
  `client canceled` line and answer 499, while genuine upstream failures
  (including gateway timeouts) still log as upstream errors and still
  return 502. Thanks to Boris Kurktchiev for finding and fixing the
  router half of this.

  Worth knowing if you grep the daemon log or parse it: these requests
  now carry `499` where they used to carry `502`.

## [v0.1.23] — 2026-09-28

### Fixed

- **Auto mode no longer refuses every command in newer Claude Code builds.**
  Tool calls were being denied with `The server-side auto mode classifier gave
  no verdict for Bash: the request that produced this action did not ask for
  one`. Newer Claude Code asks the API to run auto mode's safety check
  server-side, and the gateway never answers. In forward-proxy mode Claude
  Code believes it's talking to Anthropic directly, so it had nothing to fall
  back on. `prism claude` (and `prism env`) now set
  `CLAUDE_CODE_AUTO_MODE_SERVER=0`, which makes Claude Code run the check
  locally, as older builds did. If you've set that variable yourself, your
  value is kept.

## [v0.1.22] — 2026-09-28

### Fixed

- **Claude Code works again after its latest update.** Every request was
  failing with `API Error: 400 the inference provider rejected the request as
  invalid ... tool type 'advisor_20260301' is not supported for this model`.
  Claude Code now offers an "advisor" server tool that the Bedrock-backed
  gateway doesn't support. prism removes that tool from requests, along with
  any `tool_choice` that selects it. Every other tool is forwarded as before.
  Claude Code's advisor feature isn't available through prism, but it never
  worked through the gateway anyway.

### Added

- **Stopgap config for the next client update that breaks.** When Claude
  Code, Codex or Pi starts sending a field or tool the gateway rejects, you
  can now drop it yourself instead of waiting for a prism release. That
  matters on machines where prism is the only way to reach a model. The
  gateway's 400 message usually names what to add:

  ```bash
  prism config set anthropic_strip_fields some_field,another_field
  prism config set anthropic_strip_tool_types some_tool_
  prism config set openai_strip_fields some_field
  prism config set openai_strip_tool_types some_tool_
  prism down && prism up        # lists are read when the daemon starts
  ```

  Fields are top-level request body keys. Tool types are prefixes, so
  `advisor_` matches every dated version of that tool. Each list adds to
  prism's built-in handling and can't switch any of it off. Use
  `prism config unset <key>` to clear one. The Anthropic lists apply to
  `/v1/messages`, in normal and forward-proxy mode. The OpenAI lists apply to
  `/v1/responses` and `/v1/chat/completions`. They're empty by default, and
  OpenAI requests are forwarded unchanged until you set one.

## [v0.1.21] — 2026-09-18

### Added

- **`prism pi [args...]`** now does the whole Pi setup, like `prism claude`
  and `prism opencode` do for their tools. It starts prism if needed, supplies
  dummy credentials, rewrites Pi's Anthropic and OpenAI model entries for the
  current router port, and then launches Pi with every argument passed through.
  A fresh Pi install is bootstrapped with `pi update --models`, so there is no
  longer a separate `prism pi config && prism exec pi` first-run ritual.
  `prism pi config` keeps its existing setup-only meaning for scripts and
  narrowed model lists; use `prism exec pi config` for Pi's own unrelated
  package-configuration screen.
- **`prism opencode`**, alongside `prism claude` and `prism codex`. OpenCode
  already worked through `prism exec opencode`, but only on machines that had
  already stored an Anthropic credential: OpenCode offers a provider's models
  only when one of the environment variables named in its catalog is set, and
  prism deliberately clears `ANTHROPIC_API_KEY`. The new command sets it to a
  dummy value — the router strips it before forwarding, exactly as it already
  did for `OPENAI_API_KEY` — so the Anthropic models show up on a fresh
  install. `prism exec opencode` gets the same treatment. Note that a
  `provider.anthropic.options.baseURL` in your own OpenCode config still wins
  over the environment, and would bypass prism entirely.

### Fixed

- **Pi's Anthropic models no longer fail with `fallbacks: Extra inputs are not
  permitted`.** Newer Pi catalogs mark refusal-fallback models, which makes Pi
  send Anthropic's server-side `fallbacks` field. The Bedrock-backed gateway
  does not support that beta, so prism now strips the field with the other
  unsupported top-level Messages fields.
- Pi setup now honors `PI_CODING_AGENT_DIR`. Both `prism pi` and
  `prism pi config` write to the same custom agent directory Pi reads, rather
  than always modifying `~/.pi/agent`.
- **`prism pi config` no longer deletes unrelated custom providers.** It used
  to rewrite `models.json` with only the Anthropic and OpenAI entries, so a
  local provider such as llama-swap disappeared and Pi printed `No models
  match pattern` for its configured default. The command now replaces only
  the providers it manages and preserves the rest of the file.
- `prism pi config` is listed in `prism help`. It has worked since v0.1.12 but
  never appeared in the usage text, so the only way to find it was the README.
  The help text and the dispatch table are now checked against each other, in
  both directions, so a command can't ship unlisted and the help can't outlive
  a rename.

## [v0.1.20] — 2026-09-10

### Fixed

- **Clients that omit the `/v1` prefix were half-invisible and, worse,
  unscrubbed.** Prism hands out `ANTHROPIC_BASE_URL` without a `/v1` suffix
  because the official Anthropic SDKs append `/v1/messages` themselves. The
  Vercel AI SDK appends only `/messages`, and the gateway accepts both — so
  those requests worked while silently skipping every behaviour prism gates on
  the `/v1` prefix: path dispatch, request logging, usage accounting and
  Bedrock scrubbing. In practice an OpenCode session through
  `prism exec opencode` proxied fine, logged nothing, contributed nothing to
  `prism usage`, and sent unscrubbed bodies to a Bedrock-backed gateway — so
  `thinking`, `metadata` and oversized non-streaming `max_tokens` reached
  upstream unmodified, which is exactly the class of request that comes back
  as an unexplained "the inference provider rejected the request" 400. Prism
  now canonicalises a version-less API path to its `/v1` spelling before
  anything else looks at it. Requests appear in `prism logs` and `prism usage`
  as `/v1/...`, and are scrubbed like any other. Note that scrubbing strips
  `thinking`, so AI-SDK clients that ask for extended thinking through prism
  no longer get it — the same as every other client.
- **Anything prism proxies is now logged.** The request log was gated on a
  `/v1` allowlist, so a path prism forwarded but did not recognise left no
  trace at all. Only prism's own `/_prism/` endpoints are skipped now, so a
  future client with an unfamiliar path shape can't go dark.
- **Forward-proxy usage records named the wrong model.** In
  `claude_forward_proxy_mode` the MITM path overwrote the model with whatever
  the request asked for, discarding the one the gateway reported — so records
  logged as `usage: ?` and landed in `prism usage` under `(unknown)` whenever a
  client omitted the field, and were attributed to the requested alias rather
  than the served model otherwise. The response is now the authoritative
  source, matching the direct path.
- `prism logs` went silent after midnight. The daemon rotates by creating a
  new dated log file rather than renaming the current one, so the follower
  sat on a handle to a file nothing was writing to any more — and once that
  file was gzipped, to one that no longer existed. It now notices the new
  file, drains the old one, and switches over, printing a
  `==> daemon-YYYY-MM-DD.log <==` marker to stderr. Linux users on systemd
  were unaffected, since `prism logs` shells out to `journalctl -f` there.

### Changed

- **One log line per request.** The request line and its token counts used to
  be printed separately, by two different layers — so under any concurrency
  they interleaved and couldn't reliably be paired. They are now a single
  line, which is also about half the log volume:

  ```
  POST /v1/messages 200 req=747024B resp=2073B model=claude-opus-5 in=2 out=89 cache_read=18807 cache_write=209741 2.721s
  ```

  Cache counts are new to the log — worth having, since `in=2` on a 747KB
  request only makes sense next to them. Every field is always present,
  zeros included, and a model the gateway didn't name shows as `model=?`, so
  the line can be parsed without checking which fields it happens to carry.
  Requests that carry no token usage keep the plain request line.
- Response capture moved into a shared `internal/capture` package used by both
  the router and the forward proxy, alongside the request-direction scrubbing
  they already share. The two copies had drifted, which is what let the
  model-attribution bug above live in one path and not the other.

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

[v1.0.0-beta.5]: https://github.com/webvictim/prism/compare/v1.0.0-beta.4...v1.0.0-beta.5
[v1.0.0-beta.4]: https://github.com/webvictim/prism/compare/v1.0.0-beta.3...v1.0.0-beta.4
[v1.0.0-beta.3]: https://github.com/webvictim/prism/compare/v0.5.0...v1.0.0-beta.3
[v0.5.0]: https://github.com/webvictim/prism/compare/v0.4.1...v0.5.0
[v0.4.1]: https://github.com/webvictim/prism/compare/v0.4.0...v0.4.1
[v0.4.0]: https://github.com/webvictim/prism/compare/v0.3.0...v0.4.0
[v0.3.0]: https://github.com/webvictim/prism/compare/v0.2.2...v0.3.0
[v0.2.2]: https://github.com/webvictim/prism/compare/v0.2.1...v0.2.2
[v0.2.1]: https://github.com/webvictim/prism/compare/v0.2.0...v0.2.1
[v0.2.0]: https://github.com/webvictim/prism/compare/v0.1.28...v0.2.0
[v0.1.28]: https://github.com/webvictim/prism/compare/v0.1.27...v0.1.28
[v0.1.27]: https://github.com/webvictim/prism/compare/v0.1.26...v0.1.27
[v0.1.26]: https://github.com/webvictim/prism/compare/v0.1.25...v0.1.26
[v0.1.25]: https://github.com/webvictim/prism/compare/v0.1.24...v0.1.25
[v0.1.24]: https://github.com/webvictim/prism/compare/v0.1.23...v0.1.24
[v0.1.23]: https://github.com/webvictim/prism/compare/v0.1.22...v0.1.23
[v0.1.22]: https://github.com/webvictim/prism/compare/v0.1.21...v0.1.22
[v0.1.21]: https://github.com/webvictim/prism/compare/v0.1.20...v0.1.21
[v0.1.20]: https://github.com/webvictim/prism/compare/v0.1.19...v0.1.20
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
