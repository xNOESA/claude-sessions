# Claude Code session manager (`cs`) — design

Date: 2026-11-14. Status: awaiting owner approval.

## Intent

The owner runs several Claude Code sessions at once, across projects, using
several Claude accounts that belong to different organizations. Today the
accounts are managed by hand: logging out and in, and losing track of which
session is on which account. The account behind a session should be
interchangeable. Any account can start a session in any project, continue a
session another account began, and be swapped in mid-session, with nothing
lost. One view shows what every session is doing and what each account has
left.

## Goals

1. Hold several logins at once, one per account, each signed in once.
2. Start a session in any project directory under any account.
3. Resume any existing session under any account with its full conversation.
4. Change the account of a running session with one command.
5. A live dashboard, one row per session: project, session name, state, the
   account really in use, that account's usage, current-task progress, and
   project-plan progress where a plan exists.
6. Works for any project with no per-project setup. Per-project settings are
   optional refinements.
7. Plain `claude` keeps working exactly as before, and sees the same history.

## Non-goals

- Cloud sessions (claude.ai/code). Local terminal sessions only.
- Automatic account rotation. Every switch is an explicit command; the
  dashboard only shows which accounts have headroom.
- Syncing between machines.
- Windows.
- Configuring any particular sandbox. `cs` supports a per-project launch
  wrapper (below); granting a sandbox access to the account and state
  directories is that project's own task.

## Anti-goals

- A wrong account label. The dashboard must show the login the session is
  really using, never just the profile name it was launched with.
- Lost or forked history. A switch must never leave two diverging copies of a
  conversation, and must never interrupt a turn in progress unless forced.
- Credentials handled by `cs`. It never reads, copies, prints or stores a
  token. Claude Code keeps its own logins; `cs` only chooses which config
  directory a session runs with.
- A project's conversation sent through an account that should not see it,
  by accident. See "Allowed accounts".

## Constraints and verified facts

From the Claude Code docs (code.claude.com/docs, read 2026-11-14) and the
installed CLI (2.1.325):

- `CLAUDE_CONFIG_DIR` relocates everything under `~/.claude`, including
  `.claude.json`, settings, `projects/` (history) and the login. On macOS the
  Keychain entry is keyed to the config directory, so two directories hold
  two logins at once. On Linux the login is `<dir>/.credentials.json`.
- History is `<config>/projects/<cwd with non-alphanumerics as ->/<session-id>.jsonl`.
  The entry format is internal and may change.
- `claude --resume <id|name|path>`, `--session-id <uuid>`, `--name`,
  `--settings <file-or-json>`, `claude auth login`, `claude auth status`
  (JSON: `loggedIn`, `authMethod`, `configDirectory`, `projectsDirectory`).
- The status line command receives JSON on stdin with `session_id`,
  `session_name`, `transcript_path`, `cwd`, `cost.total_cost_usd`,
  `context_window.used_percentage`, `model.display_name`, and, for
  subscription accounts only and after the first response,
  `rate_limits.five_hour` / `rate_limits.seven_day` with `used_percentage`
  and `resets_at`. This is the only documented machine-readable source of
  limit usage.
- Hooks include SessionStart, UserPromptSubmit, Stop, Notification,
  SessionEnd, TaskCreated, TaskCompleted. Each gets `session_id`,
  `transcript_path`, `cwd` on stdin.
- Task lists persist under `<config>/tasks/`; the format is undocumented.

**Not documented, and the plan must prove these first (see "Proof steps"):**

- R1. Resuming under config dir B a session written under config dir A, when
  both share one `projects/` directory through a symlink.
- R2. Hooks and a status line supplied through `--settings` for one launch.
- R3. Where the signed-in account's email and organization can be read.
  Expected: `oauthAccount` in `<config>/.claude.json`.

## Approaches considered

- **A. One profile per account, one shared history (chosen by the owner).**
  Each account has its own config directory; all of them point at one history
  store. Sessions on different accounts run side by side; a switch is quit
  and resume. Cost: a session restarts to change account.
- **B. One config directory, swap the login in place.** Rejected: all running
  sessions share whichever login is current, so two sessions cannot be on
  different accounts, and swapping under a live session risks corrupting it.
- **C. Fresh session from a handoff note on every switch.** Rejected: loses
  the conversation, which is the hindrance being removed.

## Design

### Layout

```
~/.claude/                      unchanged; the shared store and the "default" account
~/.claude-sessions/
  accounts.json                 registry: name -> config dir, added date
  projects.json                 optional per-project settings, keyed by project root
  accounts/<name>/              CLAUDE_CONFIG_DIR for one account
    projects  -> ~/.claude/projects      (symlink)
    tasks     -> ~/.claude/tasks         (symlink)
    settings.json, CLAUDE.md, skills, agents, commands, plugins,
    keybindings.json -> ~/.claude/...    (symlinks, where the target exists)
    .claude.json, login          the account's own, never shared
  state/
    sessions/<session-id>.json  last report from each session
    accounts/<account-key>.json last-known usage per real account
    locks/<session-id>.lock     held by the wrapper running that session
```

`~/.claude` is the shared store, so nothing is migrated, plain `claude` is
unaffected, and every session ever run on the machine is already listed.
The login already in `~/.claude` is adopted as an account named `default`
whose config dir is `~/.claude` itself.

Shared: history, task lists, settings, instructions, skills, plugins, so a
session behaves the same under any account. Not shared: the login and
`.claude.json` (per-account state such as the signed-in identity and project
trust prompts). A project may therefore ask for trust once per account.

### Commands

```
cs                         live dashboard (same as cs status)
cs status [--once|--json]
cs account add <name>      create the profile, link it, run `claude auth login` in it
cs account list            name, real identity, plan type, last-known usage
cs account remove <name>   run `claude auth logout` in it, then delete the profile dir
cs new [-a account] [-n name] [-- claude args]     new session in the current directory
cs resume [session] [-a account]                   resume; no argument opens a picker
                                                   for the current project
cs switch <session> <account> [--force]            change a running session's account
cs project set <key> <value>                       per-project settings for the current project
```

`<session>` is an id, a unique id prefix, or a session name. With no `-a`,
`cs new` and `cs resume` use the account the session last ran under, else the
project's default account, else ask, listing each account's headroom.

### Running a session

`cs new` and `cs resume` run as a thin wrapper that stays the parent of
`claude` in the user's terminal:

1. Take the session lock; refuse if that session is already running.
2. Check the account is allowed for the project.
3. Launch `claude` with `CLAUDE_CONFIG_DIR=<account dir>`, `CS_SESSION=1`,
   `--session-id <uuid>` (new) or `--resume <id>` (resume), `--name`, and
   `--settings` carrying the reporter hooks and status line.
4. When `claude` exits, release the lock and mark the session ended, unless
   a switch was requested, in which case relaunch with `--resume <id>` under
   the new account in the same terminal.

`cs switch` writes a switch request into the session's state and signals the
wrapper. The wrapper acts only when the session is idle (last report was
Stop, not mid-turn); `--force` interrupts. It stops `claude` the way a user
quitting would, waits for exit, then relaunches. If the new account fails to
start (not signed in, not allowed), it relaunches under the previous account
and says why.

A session started with plain `claude` shows on the dashboard as unmanaged
(from the shared history, no live state). `cs resume` picks it up.

### Reporting

The injected settings call `cs report <event>` for SessionStart,
UserPromptSubmit, Stop, Notification, SessionEnd, TaskCreated, TaskCompleted
and as the status line. Each call reads stdin, updates
`state/sessions/<id>.json` atomically (write temp, rename), and exits fast;
it must never block or fail the session, so every error is swallowed and
logged to `~/.claude-sessions/cs.log`.

- State: `working` (UserPromptSubmit), `idle` (Stop), `needs you`
  (Notification), `ended` (SessionEnd, or the wrapper's lock is gone).
- The status line report also prints a one-line status line. If the user's
  own settings already define a status line command, `cs` runs it and prints
  its output instead, so nothing they had is lost.
- If R2 fails, fallback: install the same hooks into the shared
  `settings.json` guarded by `CS_SESSION`, so they do nothing under plain
  `claude`.

### The account shown

On every status line report, `cs` resolves the real identity of the config
directory the session is using: email and organization from R3's source,
re-read when that file's modification time changes. A `/login` inside a
session therefore shows correctly on the next report. If the identity cannot
be read, the row shows the profile name marked `?`. Usage is stored under
the real identity (`account-key` = organization id + account id, or the
profile name when unknown), not under the profile name.

### Usage

- Per account: 5-hour and 7-day `used_percentage` and `resets_at`, from the
  most recent status line report of any session on that account, with the
  time it was seen. Shown with its age when older than 5 minutes; shown as
  `reset` once `resets_at` has passed; `—` when never seen.
- Accounts without subscription limits (API key, Console) show `—` for
  limits and the session's cost.
- Per session: context-window percentage and `cost.total_cost_usd`.
- Colour: amber from 75%, red from 90%.

There is no way to read an account's limits without a session running on it,
so an idle account shows its last-known numbers. This is a platform limit.

### Task progress

Counts from TaskCreated / TaskCompleted reports, as `done/total` plus the
subject of the task in progress. On resume, and for unmanaged sessions, the
counts are rebuilt by one parser that reads the task tool calls in the
transcript. That parser is the only code that knows the transcript format;
it has fixture tests and returns "unknown" on anything it does not
recognise. `—` when the session has no task list.

### Project-plan progress

Project root = the git top level of the session's directory, else the
directory. In order:

1. `plan.command` in the project's settings: a shell command run in the
   project root that prints `done total [label]`. Run at most once a minute
   with a 5-second timeout. This covers plans that are not checklists, such
   as a week-by-week table or a JSON tracker.
2. `plan.file` in the project's settings: count `- [x]` against all
   `- [ ]` / `- [x]` items.
3. Convention: the newest `docs/superpowers/plans/*.md`, else `PLAN.md`,
   `TODO.md`, `docs/plan.md` in the project root, counted the same way.
4. None: `—`.

### Per-project settings (all optional)

`cs project set` writes `~/.claude-sessions/projects.json`; nothing is added
to the project's repository.

- `plan.command`, `plan.file`: above.
- `default_account`.
- `allowed_accounts`: see below.
- `launch`: a command template with `{claude}` and `{config_dir}`
  placeholders, for projects that run Claude inside a wrapper, e.g.
  `nono run --allow-cwd --profile X -- {claude}`. Default is `claude` itself.

### Allowed accounts

Resuming a session under another account sends that project's whole
conversation, code included, through the other organization's account. By
default every account may run every project, as the owner asked. A project
with `allowed_accounts` set refuses any other account at `new`, `resume` and
`switch`, and the picker does not offer them.

### Dashboard

```
PROJECT  SESSION           STATE      ACCOUNT            5h    WEEK  CTX  TASK              PLAN
api      week-10 kind run  working    ana@acme   · Max   62%   31%   48%  3/5 fix relay     week 10/14
site     mailer follow-up  needs you  bo@other-org       8%    12%   12%  1/2 form test     —
api      ledger p99        idle       ana@acme   · Max   62%   31%   71%  —                 week 10/14

ACCOUNTS   ana@acme 62% / 31% (resets 14:10)   bo@other-org 8% / 12%   third@x — (never seen)
```

Redraws every 2 seconds from the state files. Running sessions first, then
sessions ended in the last 24 hours; `--all` lists every session in the
history. `--json` emits the same data for scripting. Read-only.

## Decided by the owner

- Approach A.
- Local sessions only; accounts are currently managed by hand.
- Works for any project.

## My calls (the owner can overrule)

- Standalone repository `claude-sessions`, one command `cs`.
- Go, standard library only, one static binary; plain ANSI redraw for the
  dashboard. The reporter runs on every status line refresh, so it must start
  in milliseconds and have no runtime to install.
- macOS first, Linux supported and used for CI.
- `~/.claude` is the shared store and the `default` account.
- Per-project settings live in `~/.claude-sessions`, not in the projects.
- Switching waits for idle; no automatic rotation.
- Dashboard is a terminal table, not a web page.

## Proof steps (first tasks of the plan, on the owner's Mac)

Each is a short manual script with a pass/fail result, run before any code
depends on it:

- R1: two profiles, shared `projects` symlink; start under A, resume under B,
  confirm the earlier conversation is present and one transcript file
  results. If it fails: try `--resume <absolute transcript path>`; if that
  fails too, stop and return to the owner, since approach A depends on it.
- R2: launch with hooks and status line in `--settings`; confirm both fire.
  Fallback is in "Reporting".
- R3: find the identity source. Fallback is the profile name with `?`.

## Testing

- Unit tests for state files, account resolution, plan counting, the
  transcript parser (fixtures), and the usage staleness rules.
- Wrapper and switch tests against a fake `claude` executable that records
  its arguments and environment and replays hook and status line calls. No
  test needs a real login or network.
- A manual acceptance checklist for the three proof steps and one real
  two-account switch.

## Left to the builder

Package layout, the exact state-file schema, signal choice for the wrapper,
picker presentation, colour codes, and install method (`go install` plus a
Makefile target is enough).

## Settled after review (my calls)

1. **Sessions `cs` holds no lock for.** Best effort, not a guarantee, because
   plain `claude` must stay untouched. Before resuming, `cs` refuses if the
   transcript was written in the last 60 seconds or a `claude` process has
   that session id in its arguments, and says which; `--force` overrides.
   Nothing stops plain `claude --resume` from opening a session `cs` is
   running; the README says so.
2. **Allowed accounts are matched on the real identity** (organization, then
   email), not the profile name. Entries in `allowed_accounts` may be a
   profile name, an email or an organization name; a profile name is resolved
   to its current identity at check time. If a project has an allow-list and
   the identity cannot be read, the launch is refused. A `/login` inside a
   session to a disallowed account cannot be prevented; the dashboard marks
   that row red with `not allowed`. Project root is the main repository of a
   git worktree (from `git rev-parse --git-common-dir`), so worktrees inherit
   the project's settings.
3. **Per-account `.claude.json` is not synced in v1.** Permissions and
   project-scope MCP servers already follow the session because
   `settings.json` and the project's `.mcp.json` are shared. User-scope and
   local-scope MCP servers added with `claude mcp add` live in
   `.claude.json` and are per account; a switched session will not see ones
   added under another account. Documented as a known limit; a sync is a
   later change if it bites.
4. **Credentials from outside the profile.** The wrapper removes
   `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN`
   from the session's environment and says so once, since they would
   override every profile's login. If the shared `settings.json` defines
   `apiKeyHelper`, `cs` refuses to launch and explains. v1 supports
   claude.ai subscription logins only; API-key and Console accounts are out
   of scope (Console sign-in is stored outside the config directory, so
   profiles cannot isolate it). The "accounts without subscription limits"
   line under Usage applies only to a login `cs` cannot classify.
5. **A session keeps its id across accounts.** R1 passes only if the resumed
   session has the same session id and appends to the same transcript file.
   A new id is an R1 failure and goes back to the owner; no lineage mapping
   is designed.

## Sandboxed projects (added 2026-11-14)

The `cs` wrapper always runs outside the sandbox and is the parent of the
project's launch command, so starting, stopping and switching are unaffected
by a sandbox. Two things run inside it, as children of `claude`:

- `claude` itself, which must read and write its config directory
  (`{config_dir}`) and the shared store it links to (`~/.claude`).
- `cs report`, called by the hooks and status line, which must be able to
  execute the `cs` binary and write `~/.claude-sessions/state`
  (`{state_dir}`).

The `launch` template therefore has `{claude}`, `{config_dir}` and
`{state_dir}` placeholders, so a sandbox can be granted exactly the active
account's directory and the state directory for that launch, and no other
account's directory. If `cs report` cannot write its state it stays silent
(it never fails the session); the dashboard then shows the session as
`running, no reports` from the wrapper's lock, with the account from the
wrapper's own launch record marked `?`.

The `default` account (`~/.claude`) needs no new grant in a sandbox that
already allows `~/.claude`, apart from `{state_dir}`.

## Account usage block (added 2026-11-14 at the owner's request)

The accounts section shows, per account, a header (account name, email,
organization, plan, age of the reading, an "active" mark when a session is
running on it) and one meter per limit: extra-usage spend against its
monthly limit, the 5-hour and 7-day windows, and any per-model weekly
windows, each with its percentage and reset time.

The status line only carries the 5-hour and 7-day figures, so `cs` reads the
rest by starting `claude` under the account's config directory in
`stream-json` mode and sending the `get_usage` control request with
`skip_behaviors`. That request answers from the claude.ai usage endpoint
without a model call. It is run with hooks disabled, in `~/.claude-sessions`,
with a 20 second limit, at most once per account every five minutes, in the
background while the dashboard is open. A failed probe keeps the last good
reading and records the reason. This supersedes the earlier limit that an
idle account could only show its last-known usage. `get_usage` is not in the
public documentation; if it goes away, the 5-hour and 7-day figures still
arrive from running sessions.

## Usage by model (added 2026-11-15 at the owner's request)

Two views, both read from the session histories on this machine:

- A MODELS column per session: each model family the session and its
  subagents used, with the cost from the latest `cost-state` record Claude
  Code writes in the transcript (exact, at API prices). Without such a
  record, output tokens are shown instead.
- Under each account, `models 5h` and `models 7d`: an estimate of what each
  model cost on that account within its current limit windows (the window
  length before the known reset time, else that long ago). A session's
  per-model cost is shared between its requests by price-weighted token
  count (output 5x, cache write 1.25x, cache read 0.1x of input) and each
  request is attributed to the account the session was on at that time.

To make that attribution possible, a session's state records an account
span each time it starts under a different account (launch, switch, or a
login inside the session). Requests from before the first span, and
sessions never run through `cs`, count against the default account.
Histories are read incrementally, so the live dashboard only parses what
was appended since the last redraw. Usage on other machines is not visible.
