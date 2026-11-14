# Acceptance

## Proof steps (owner's Mac)

Run each script in `proofs/` and record the result. R1 must pass before `cs`
is used for real; R2 and R3 have fallbacks (see the bottom of this file).

| Proof | Date | Claude Code version | Result | Notes |
| --- | --- | --- | --- | --- |
| R1 cross-account resume, same session id | | | | |
| R2 hooks via `--settings` | | | | |
| R2 status line via `--settings` | | | | |
| R3 identity keys in `.claude.json` | | | | |

Seen on a Linux cloud container, 2026-11-14, CLI 2.1.325: `oauthAccount` has
`accountUuid`, `emailAddress`, `organizationUuid`; no organization name or
plan key.

## Two-account acceptance

