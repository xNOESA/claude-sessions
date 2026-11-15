# claude-sessions

`cs` runs Claude Code sessions under several Claude accounts on one machine.
Any account can start a session, continue one another account began, or take
over a running one, and a dashboard shows every session, its account, usage
and progress.

> **Status: early.** The three proof scripts in `proofs/` passed on the
> owner's Mac on 2026-11-14 (see `docs/acceptance.md`). The automated tests
> use a stand-in for `claude`; the two-account checklist in
> `docs/acceptance.md` has not been run yet. Re-run the proofs after a
> Claude Code upgrade: they cover behaviour its documentation does not
> promise.

## How it works

- `~/.claude` stays as it is. It is the shared history and the `default`
  account, so plain `claude` keeps working and sees the same sessions.
- Each extra account gets its own Claude Code config directory under
  `~/.claude-sessions/accounts/`, signed in once, with its history, task
  lists, settings, skills and plugins linked back to `~/.claude`.
- `cs new` and `cs resume` wrap `claude`. The wrapper holds a lock on the
  session and relaunches it under another account when you run `cs switch`.
- Hooks and the status line, added for that launch only, report state to
  `~/.claude-sessions/state/`. The dashboard reads those files.

`cs` never reads, copies or stores a login. Claude Code keeps its own.

## Install

Go 1.22 or newer; macOS or Linux.

```bash
go install ./cmd/cs
```

## First run

```bash
cs account add work
```

That creates the `work` account and runs `claude auth login` for it. Then,
in a project directory:

```bash
cs new -a work -n "first task"
```

In another terminal:

```bash
cs
```

## Commands

| Command | What it does |
| --- | --- |
| `cs` or `cs status` | Live dashboard, drawn on a black background for contrast (`--no-black` keeps your terminal's own). `--once` prints it once, `--json` for scripts, `--all` includes sessions older than 24 hours. |
| `cs account add <name>` | Create an account and sign in to it. |
| `cs account login <name>` | Sign in again. |
| `cs account list` | Accounts, who each is signed in as, and whether the login is current. |
| `cs account remove <name>` | Sign out and delete the account's directory. Shared history is untouched. |
| `cs new [-a account] [-n name] [--prompt text] [-- claude flags]` | Start a session in the current directory. |
| `cs resume [session] [-a account] [--force] [-- claude flags]` | Continue a session under any account. With no session, pick from this project's. |
| `cs switch <session> <account> [--force]` | Move a running session to another account. Waits for the current turn to finish unless forced. |
| `cs project show` / `cs project set <key> <value>` | Optional settings for the current project. |

Flags after `--` go to `claude` and are repeated every time the session is
relaunched for a switch, so put only flags there (`-- --model opus`). Give a
first message with `--prompt`; it is sent once. `cs` sets `--resume`,
`--session-id` and `--settings` itself and refuses them after `--`.

A session is named by its id, the start of its id, or its name. With no
`-a`, `cs` uses the account the session last ran under, then the project's
default, then asks.

## The dashboard

```
PROJECT  SESSION      STATE      ACCOUNT       5h   WEEK  CTX  MODELS                      TASK           PLAN
api      week-10 run  working    ana@acme.com  16%  52%   48%  Fable $33.83 · Haiku $0.06  3/5 fix relay  week 10/14
site     mailer       needs you  bo@other.org  8%   3%    12%  Sonnet $1.12                1/2 form test  —

ACCOUNTS
default  ana@acme.com  [Acme] · max  · 4m ago   ● active
   $$    ━━━━━━━━━━━━━━━━━━━━━━──────────────────  55%  $21.99 / $40.00
   5h    ━━━━━━──────────────────────────────────  16%  resets 3h 32m · 00:10
   7d    ━━━━━━━━━━━━━━━━━━━━━───────────────────  52%  resets 1d 7h · Nov 15 03:59
   Fable ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━──────  85%  resets 1d 7h · Nov 15 04:00
   models 5h  Fable ≈$33.83 · Haiku ≈$0.06
   models 7d  Fable ≈$212.40 · Sonnet ≈$4.10 · Haiku ≈$0.92

work     bo@other.org  [Other Org] · max  · 4m ago   ● active
   5h    ━━━─────────────────────────────────────   8%  resets 1h 2m · 21:40
   7d    ━───────────────────────────────────────   3%  resets 4d 22h · Nov 18 19:00
   models 5h  Sonnet ≈$1.12
```

Sessions:

- **ACCOUNT** is the login the session is really using, re-read on every
  status line refresh, so a `/login` inside a session shows up.
- **5h / WEEK** are that account's limits. Amber from 75%, red from 90%.
- **MODELS** lists each model the session has used, subagents included,
  with the cost Claude Code recorded for it. Costs are at API prices, so on
  a subscription they measure how much was used, not what you paid. The
  figure can trail a running session by a few turns.
- **TASK** is the session's own to-do list.
- **PLAN** is the project's plan, when one is found.
- **STATE** `unmanaged` means the session was started with plain `claude`;
  `cs resume` picks it up.

Accounts, one block each, headed by the name you pass to `-a`:

- **$$** is extra-usage spend against its monthly limit, when enabled.
- **5h**, **7d** and any per-model weekly limits (e.g. **Fable**) show how
  full each window is and when it resets.
- **● active** marks accounts with a running session; `· 4m ago` is the age
  of the reading.
- **models 5h / 7d** estimate what each model cost on that account in its
  current 5-hour and 7-day windows (`≈`). They come from the session
  histories on this machine only, so use on other devices or on claude.ai is
  not included. A session that moved between accounts is split at the
  moment of each switch; anything from before `cs` first ran a session, and
  sessions started with plain `claude`, count against the `default` account.

Account usage is read by asking Claude Code itself: every five minutes while
the dashboard is open, `cs` starts `claude` briefly under each account and
sends its `get_usage` request. That makes no model call and costs nothing,
and it works for accounts with no session running. `cs` still never touches
a login. `cs status --no-refresh` skips the check and shows what is stored.

## Per-project settings

All optional, stored in `~/.claude-sessions/projects.json`, nothing is added
to the project. A project is the main repository of the current git checkout
(worktrees share their repository's settings), or the directory itself.

| Key | Meaning |
| --- | --- |
| `plan.file` | Markdown file whose `- [ ]` / `- [x]` items are counted. |
| `plan.command` | Shell command, run in the project root, that prints `done total [label]`. At most once a minute, 5 second limit. |
| `default_account` | Account used when none is given. |
| `allowed_accounts` | Comma-separated emails, organization names or account names. Any other account is refused. |
| `launch` | Command that wraps Claude, with `{claude}`, `{config_dir}` and `{state_dir}` placeholders. |

Without settings, `cs` looks for the newest `docs/superpowers/plans/*.md`,
then `PLAN.md`, `TODO.md` and `docs/plan.md`.

A plan that is not a checklist, for example a JSON array of items with a
`status` field, needs a command. Put it in a small script:

```bash
#!/bin/sh
# plan-progress.sh: prints "done total"
jq -r '"\([.[] | select(.status == "Done")] | length) \(length)"' tracker.json
```

```bash
cs project set plan.command "sh plan-progress.sh"
```

### Allowed accounts

Resuming or switching a session sends that project's whole conversation,
code included, through the other account's organization. By default every
account may run every project. To restrict one:

```bash
cs project set allowed_accounts "ana@acme.com"
```

### Sandboxed projects

If a project runs Claude inside a sandbox wrapper, set `launch`. `cs` itself
stays outside the sandbox. Inside it, Claude needs its config directory and
the reporter needs the state directory, so grant exactly those:

```bash
cs project set launch "nono run --allow-cwd --profile claude --allow {config_dir} --allow {state_dir} -- {claude}"
```

Check your sandbox's own flags; the ones above are an illustration. The
sandbox must also be able to execute the `cs` binary. If the reporter cannot
write, the session still runs and the dashboard shows it as
`running, no reports`.

## Limits

- Subscription (claude.ai) logins only. API-key and Console accounts are not
  supported. `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN` and
  `CLAUDE_CODE_OAUTH_TOKEN` are removed from sessions, and `cs` refuses to
  launch if the shared settings define `apiKeyHelper`.
- A session restarts to change account. It keeps its conversation.
- Plain `claude --resume` on a session `cs` is running is not prevented.
  `cs` refuses to resume a session that looks open elsewhere, on a best
  effort basis.
- MCP servers added with `claude mcp add` at user or local scope are per
  account. Project-level servers and permissions follow the session.
- A project may ask for trust once per account.
- If an account's directory already holds its own `settings.json` or
  similar where a link to the shared one belongs, `cs` warns and carries on
  with the private copy. A private history directory is refused.
- No automatic account rotation: every switch is your command.
- The `get_usage` request is how Claude Code's own usage screen gets its
  numbers, but it is not in the public documentation and could change. If it
  stops answering, the account shows "usage unavailable" and the 5h and week
  figures fall back to what running sessions report.
- The transcript format is internal to Claude Code. If it changes, task
  progress shows `—` until the parser is updated.

## Development

```bash
make test
```

Tests use `testdata/fakeclaude`; none needs a login or the network. The
design is in `docs/superpowers/specs/`.
