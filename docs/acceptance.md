# Acceptance

## Proof steps

Run each script in `proofs/` on a machine with two signed-in accounts and
record the result. R1 must pass before `cs` is used for real; R2 and R3 have
fallbacks (see the bottom of this file).

| Proof | Date | Claude Code version | Result | Notes |
| --- | --- | --- | --- | --- |
| R1 cross-account resume, same session id | 2026-11-14 | not recorded | PASS | macOS |
| R2 hooks via `--settings` | 2026-11-14 | not recorded | PASS | macOS |
| R2 status line via `--settings` | 2026-11-14 | not recorded | PASS | macOS |
| R3 identity keys in `.claude.json` | 2026-11-14 | not recorded | PASS | macOS |

Seen on a Linux cloud container, 2026-11-14, CLI 2.1.325: `oauthAccount` has
`accountUuid`, `emailAddress`, `organizationUuid`; no organization name or
plan key.

## Two-account acceptance

Run after the proof steps pass. Tick each as it holds.

- [ ] `cs account add <second>` signs a second account in; `cs account list`
      shows both, each with the right email.
- [ ] `cs new -a default -n accept` starts a session; `cs` shows it with the
      default account's email and, after the first reply, its 5h and week
      usage.
- [ ] Ask it to remember a word. `cs switch accept <second>`: the terminal
      shows `→ now on <second's email>`, the session reopens, and it still
      knows the word.
- [ ] `cs` now shows the second account on that row and that account's usage.
- [ ] Quit the session; `cs resume accept -a default` continues it under the
      first account.
- [ ] Inside a session, `/login` to the other account: the row's account
      changes within a few seconds.
- [ ] `cs project set allowed_accounts <first email>`, then
      `cs switch accept <second>` is refused and the session is undisturbed.
- [ ] A session started with plain `claude` shows as `unmanaged` and
      `cs resume` picks it up.
- [ ] Task progress appears when the session uses its to-do list; plan
      progress appears in a project with a `PLAN.md` checklist.
- [ ] The accounts block shows each account's meters, including one with no
      session running, and the `$$` amounts match Claude's own `/usage`.

## If a proof step fails

- **R1** (resume under another account): stop. The design depends on it.
- **R2** (hooks or status line through `--settings`): the reporter already
  ignores sessions not started by `cs`, so the same hooks can be installed
  in the shared `settings.json` instead. That needs a small `cs hooks
  install` command, not yet written.
- **R3** (identity keys): only `accounts.ResolveIdentity` in
  `internal/accounts/accounts.go` needs to change. Until then rows show the
  account name with a `?`.
