# Claude Code Session Manager (`cs`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A single `cs` command that runs Claude Code sessions under any of several accounts, lets any account resume or take over any session, and shows a live dashboard of sessions, accounts, usage and progress.

**Architecture:** One config directory per account, each symlinked to the shared history in `~/.claude`. `cs new/resume` is a thin parent process around `claude` that owns a per-session lock and relaunches on a switch request. Hooks and the status line call `cs report`, which writes small JSON state files; the dashboard only reads those files and the shared history.

**Tech Stack:** Go 1.22+, standard library only. POSIX shell for proof scripts and the fake `claude` used in tests.

**Spec:** `docs/superpowers/specs/2026-11-14-session-manager-design.md`

## Global Constraints

- Go standard library only; no third-party modules. Module name `claude-sessions`, binary `cs`.
- macOS and Linux. No Windows.
- `cs` never reads, copies, prints or stores a credential. It never opens `.credentials.json` or the Keychain.
- Plain `claude` and `~/.claude` are never modified, except by the R2 fallback, which is built only if R2 fails.
- All paths come from `paths.Resolve()`. Tests set `CS_HOME`, `CS_CLAUDE_DIR`, `CS_CLAUDE_BIN`; no test touches the real home directory, a real login, or the network.
- State files are written temp-then-rename, mode 0600, in directories of mode 0700.
- `cs report` never exits non-zero and never writes to stderr; errors go to `cs.log`.
- No automatic account rotation. A switch happens only from `cs switch`.
- Exact values: recent-write guard 60 s; usage shown with age after 5 min; amber ≥ 75 %, red ≥ 90 %; dashboard redraw 2 s; plan command at most once per 60 s with a 5 s timeout; ended sessions listed for 24 h.
- Account names match `^[a-z0-9][a-z0-9_-]{0,31}$`; `default` is reserved for `~/.claude`.
- Environment variables removed from every managed session: `ANTHROPIC_API_KEY`, `ANTHROPIC_AUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`.
- Commit after every task: `<area>: <imperative summary>`.

## Review Focus

1. Two sessions report at the same instant (same account, same state directory): neither state file is truncated or loses the other's update. → Task 2.
2. A half-written or corrupt line at the end of a transcript, or a corrupt state file: the dashboard still renders every other row; the bad one shows `—`. → Tasks 2, 5, 9.
3. The target account of a switch is signed out or not allowed: the session keeps running on its current account and the reason is printed; it is never left stopped. → Task 8.
4. A project directory with spaces, outside git, or inside a git worktree: project root, settings lookup and the launch command all work. → Tasks 4, 7.
5. A terminal narrower than the table, or output piped to a file: columns truncate with `…` instead of wrapping, and no colour codes are written to a non-terminal. → Task 9.

## File Structure

```
go.mod
Makefile                         build, test, install
cmd/cs/main.go                   argument dispatch only
internal/paths/paths.go          every filesystem location
internal/store/store.go          atomic JSON, Session, Usage, locks
internal/accounts/accounts.go    registry, profile dirs and links, identity
internal/project/project.go      project root, settings, allow-list
internal/project/plan.go         plan progress
internal/transcript/transcript.go  list/find sessions, task parser (only code that knows the transcript format)
internal/report/report.go        hook and status line handling, injected settings
internal/run/run.go              new/resume wrapper, guards, launch template
internal/run/switch.go           switch request and relaunch
internal/dash/dash.go            rows, table, JSON
testdata/fakeclaude              shell stand-in for claude
proofs/r1-resume.sh r2-settings.sh r3-identity.sh
docs/acceptance.md               proof results and the manual two-account checklist
README.md
```

---

### Task 1: Scaffold and proof scripts

The proofs need real logins, so the owner runs them on their Mac. Later tasks do not wait for them: R1 is assumed, and R2/R3 each sit behind one function with a stated fallback. A failed R1 stops the project (spec, "Proof steps").

**Files:** Create `go.mod`, `Makefile`, `cmd/cs/main.go`, `proofs/r1-resume.sh`, `proofs/r2-settings.sh`, `proofs/r3-identity.sh`, `docs/acceptance.md`.

**Produces:** `make build` → `bin/cs`; `make test` → `go test ./...`; `make install` → `go install ./cmd/cs`. `cs version` prints `cs 0.1.0`.

- [ ] **Step 1:** `go.mod` (`module claude-sessions`, `go 1.22`), `Makefile`, and a `main.go` that handles `version` and prints usage with exit code 2 for anything else.
- [ ] **Step 2:** `proofs/r1-resume.sh`. Uses a temp dir `$T`; creates `$T/a` and `$T/b`, each with `projects -> ~/.claude/projects`. Prints the two `CLAUDE_CONFIG_DIR=… claude auth login` commands for the owner to run with two different accounts, waits for Enter. Then in a temp project dir: `CLAUDE_CONFIG_DIR=$T/a claude -p --session-id <uuid> "Remember the word heron. Reply OK."`, then `CLAUDE_CONFIG_DIR=$T/b claude -p --resume <uuid> "What word did I ask you to remember?"`. PASS only if the answer contains `heron`, exactly one `<uuid>.jsonl` exists under `~/.claude/projects/<encoded dir>/`, and no new `.jsonl` appeared beside it. On FAIL, repeats the resume with the absolute transcript path and reports that result separately.
- [ ] **Step 3:** `proofs/r2-settings.sh`. Runs `claude -p --settings '<json>' "Reply OK."` where the JSON has a `Stop` hook and a `statusLine` command that each append a line to `$T/fired`. PASS for hooks if the Stop line is present. The status line only runs interactively, so the script then prints a one-line interactive command for the owner and asks whether the marker text appeared.
- [ ] **Step 4:** `proofs/r3-identity.sh`. Prints only the key names (never values) of `oauthAccount` in `$CLAUDE_CONFIG_DIR/.claude.json` (default `~/.claude.json`), and the key names of `claude auth status` output. Expected keys include `emailAddress`, `accountUuid`, `organizationUuid`; note whether `organizationName` and a plan/subscription field exist.
- [ ] **Step 5:** `docs/acceptance.md` with a results table for R1–R3 (date, CLI version, PASS/FAIL, notes) and an empty "Two-account acceptance" section filled in Task 10.
- [ ] **Step 6:** `make build && bin/cs version` prints `cs 0.1.0`; `sh -n proofs/*.sh` is clean. Commit.

### Task 2: Paths and state store

**Files:** Create `internal/paths/paths.go`, `internal/store/store.go`, tests beside each.

**Produces:**

```go
// paths
type Paths struct{ Home, ClaudeDir, ClaudeBin string }
func Resolve() Paths            // CS_HOME (~/.claude-sessions), CS_CLAUDE_DIR (~/.claude), CS_CLAUDE_BIN ("claude")
func (p Paths) AccountsFile() string          // Home/accounts.json
func (p Paths) ProjectsFile() string          // Home/projects.json
func (p Paths) AccountDir(name string) string // Home/accounts/<name>; "default" -> ClaudeDir
func (p Paths) StateDir() string              // Home/state
func (p Paths) SessionFile(id string) string  // state/sessions/<id>.json
func (p Paths) UsageFile(key string) string   // state/accounts/<key>.json
func (p Paths) LockFile(id string) string     // state/locks/<id>.lock
func (p Paths) LogFile() string               // Home/cs.log

// store
type Identity struct{ Email, OrgName, OrgID, AccountID, Plan string; Known bool }
func (i Identity) Key(profile string) string  // OrgID+"_"+AccountID when Known, else "profile_"+profile
func (i Identity) Label(profile string) string // "email · Plan", "email", or profile+" ?"
type TaskProgress struct{ Done, Total int; Current string; Known bool }
type Session struct {
    ID, Name, Cwd, ProjectRoot, TranscriptPath, Profile, AccountKey, Model string
    Identity   Identity
    State      string    // "starting" | "working" | "idle" | "needs_you" | "ended"
    Managed    bool
    WrapperPID int
    StartedAt, UpdatedAt time.Time
    ContextPct, CostUSD *float64
    Tasks      TaskProgress
    SwitchTo   string
    SwitchForce bool
}
type Window struct{ UsedPct float64; ResetsAt time.Time }
type Usage struct{ FiveHour, SevenDay *Window; SeenAt time.Time; Identity Identity; Profile string }

func WriteJSON(path string, v any) error
func ReadJSON(path string, v any) error                 // returns os.ErrNotExist unchanged
func UpdateSession(p paths.Paths, id string, fn func(*Session)) error  // flock on <file>.lk around read-modify-write
func LoadSessions(p paths.Paths) []Session               // skips unreadable files
func LoadUsage(p paths.Paths) []Usage
func SaveUsage(p paths.Paths, u Usage) error
var ErrLocked = errors.New("session is already running")
func AcquireLock(p paths.Paths, id string) (release func(), err error)  // non-blocking flock, held for the process lifetime
func IsLocked(p paths.Paths, id string) bool
func Logf(p paths.Paths, format string, a ...any)
```

- [ ] **Step 1: Failing tests.**
  - `TestResolveHonoursEnv`: the three env vars override defaults; `AccountDir("default") == ClaudeDir`.
  - `TestWriteJSONAtomicAndPrivate`: file mode is 0600, parent 0700, no `*.tmp` left behind.
  - `TestUpdateSessionConcurrent` (Review Focus 1): 50 goroutines each call `UpdateSession` setting a distinct key in a counter (`s.Tasks.Total++`); final `Total == 50` and the file parses.
  - `TestLoadSessionsSkipsCorrupt` (Review Focus 2): one valid file and one containing `{"ID":` → one session returned, no panic.
  - `TestLock`: second `AcquireLock` on the same id returns `ErrLocked`; after `release()` it succeeds; `IsLocked` tracks both.
  - `TestIdentityKeyAndLabel`: known → `"org1_acc1"`, `"a@b.c · Max"`; unknown → `"profile_work"`, `"work ?"`.
- [ ] **Step 2:** `go test ./internal/...` fails to compile.
- [ ] **Step 3:** Implement. Locks use `syscall.Flock(LOCK_EX|LOCK_NB)` so a crashed wrapper releases its lock.
- [ ] **Step 4:** `go test -race ./internal/...` passes. Commit.

### Task 3: Accounts

**Files:** Create `internal/accounts/accounts.go`, `accounts_test.go`; modify `cmd/cs/main.go`.

**Consumes:** Task 2. **Produces:**

```go
type Account struct{ Name, ConfigDir string; AddedAt time.Time }
func List(p paths.Paths) []Account                 // "default" first, always present
func Get(p paths.Paths, name string) (Account, bool)
func Create(p paths.Paths, name string) (Account, error) // validates name, makes dir, EnsureLinks, records in accounts.json
func EnsureLinks(p paths.Paths, a Account) error   // no-op for default
func Delete(p paths.Paths, name string) error      // refuses "default"; removes dir and registry entry
func ResolveIdentity(configDir string) store.Identity
func LoggedIn(p paths.Paths, a Account) bool       // runs ClaudeBin "auth status" with CLAUDE_CONFIG_DIR; parses {"loggedIn":bool}
```

Links: `projects` and `tasks` are always linked (created in `ClaudeDir` if missing). `settings.json`, `CLAUDE.md`, `skills`, `agents`, `commands`, `plugins`, `keybindings.json` are linked only when the target exists. `EnsureLinks` adds missing links and never replaces a real file or directory; it returns an error naming the path if one is in the way.

`ResolveIdentity` reads `<configDir>/.claude.json` (for the default account also try `<parent of ClaudeDir>/.claude.json`), key `oauthAccount`: `emailAddress`, `accountUuid`, `organizationUuid`, and if present `organizationName` and whichever plan field R3 found. Any failure returns `Identity{Known:false}`. This function is the only place that knows the identity source.

CLI: `cs account add <name>` = `Create` then exec `ClaudeBin auth login` with `CLAUDE_CONFIG_DIR` set and the terminal attached. `cs account list` prints name, `Identity.Label`, signed-in yes/no. `cs account remove <name>` runs `ClaudeBin auth logout` in the profile, then `Delete`.

- [ ] **Step 1: Failing tests** (temp `CS_HOME`/`CS_CLAUDE_DIR`):
  - `TestCreateLinksSharedStore`: with `settings.json` present and `skills` absent in the claude dir → `projects`, `tasks`, `settings.json` are symlinks to the claude dir; `skills` does not exist.
  - `TestEnsureLinksAddsLaterFiles`: create `CLAUDE.md` afterwards, call `EnsureLinks` → link appears.
  - `TestEnsureLinksRefusesRealFile`: a real `settings.json` in the profile → error contains the path; file untouched.
  - `TestNameRules`: `Work`, `-x`, `default`, 33 chars rejected; `work-2` accepted; duplicate rejected.
  - `TestResolveIdentity`: fixture `.claude.json` → fields populated, `Known`; missing file and malformed JSON → `Known == false`.
  - `TestLoggedInUsesConfigDir`: with `testdata/fakeclaude` (Task 7 creates it; for now a two-line script written by the test) asserting the env var was the profile dir.
- [ ] **Step 2:** Run, see failures. **Step 3:** Implement. **Step 4:** `go test ./...` passes. Commit.

### Task 4: Projects, allow-list, plan progress

**Files:** Create `internal/project/project.go`, `plan.go`, tests; modify `cmd/cs/main.go`.

**Consumes:** Tasks 2–3. **Produces:**

```go
func Root(cwd string) string   // parent of `git rev-parse --path-format=absolute --git-common-dir` when in git; else cwd
type Settings struct {
    PlanCommand, PlanFile, DefaultAccount, Launch string
    AllowedAccounts []string
}
func Load(p paths.Paths, root string) Settings
func Set(p paths.Paths, root, key, value string) error
    // keys: plan.command, plan.file, default_account, launch, allowed_accounts (comma-separated); empty value clears
func Allowed(s Settings, profile string, id store.Identity,
             lookup func(name string) (store.Identity, bool)) (ok bool, reason string)
type Plan struct{ Done, Total int; Label string; Known bool }
func Progress(p paths.Paths, root string, s Settings, now time.Time) Plan
func CountCheckboxes(r io.Reader) (done, total int)
```

`Allowed`: empty list → ok. Otherwise the session's identity must be `Known`, else `(false, "account identity unknown; this project restricts accounts")`. Each entry matches if it equals the identity's email or org name (case-insensitive), or is a profile name whose `lookup` identity has the same `OrgID`. No match → `(false, "<label> is not allowed for <root>")`.

`Progress` order: `PlanCommand` (run `sh -c` in root, 5 s timeout, first line `done total [label…]`, result cached 60 s in `state/plans/<sha1 of root>.json`); `PlanFile`; newest `docs/superpowers/plans/*.md`; `PLAN.md`; `TODO.md`; `docs/plan.md`. File modes count `- [x]`/`- [X]` over all `- [ ]`/`- [x]` (leading whitespace and `*` bullets allowed); a file with zero checkboxes is skipped. Default label is `done/total`; with a command label, `label done/total`.

CLI: `cs project set <key> <value>` and `cs project show`, for `Root(cwd)`.

- [ ] **Step 1: Failing tests.**
  - `TestRootGitWorktreeAndSpaces` (Review Focus 4): a repo at `…/my proj`, a subdirectory, and a `git worktree add` copy all return the main repo path; a non-git dir returns itself. Skip if `git` is missing.
  - `TestAllowed`: empty list ok; email match; org-name match; profile-name entry resolved through `lookup`; unknown identity refused with the exact reason above; different org refused.
  - `TestCountCheckboxes`: mixed `- [ ]`, `- [x]`, `* [X]`, indented items, and a `- [x]` inside a fenced code block (counted; documented simplification) → exact numbers.
  - `TestProgressOrder`: command beats file beats convention; newest plan file wins; file without checkboxes skipped; nothing found → `Known == false`.
  - `TestProgressCommandCacheAndTimeout`: a command that appends to a counter file is run once for two calls 10 s apart and again after 61 s; `sleep 10` → `Known == false` within 6 s.
- [ ] **Step 2–4:** Fail, implement, pass. Commit.

### Task 5: Transcripts

**Files:** Create `internal/transcript/transcript.go`, `transcript_test.go`, `internal/transcript/testdata/*.jsonl`.

**Produces:**

```go
type Info struct{ ID, Cwd, Name, Path string; ModTime time.Time }
func List(claudeDir string) []Info
func Find(claudeDir, ref string) (Info, error)   // exact id, unique id prefix, or exact name
var ErrNotFound, ErrAmbiguous error
func Tasks(path string) store.TaskProgress
func RecentlyWritten(path string, now time.Time) bool   // modified within 60 s
```

Format facts (CLI 2.1.325; internal, may change): one JSON object per line; records of type `user`/`assistant` carry `sessionId` and `cwd`; tool calls are `message.content[]` items with `type:"tool_use"`, `name`, `input`. `List` globs `projects/*/*.jsonl`, takes the id from the filename and `Cwd` from the first record that has one, reading at most the first 50 lines. `Name` comes from the last record that carries a session title, if any; the builder finds that record type by naming a real session with `claude -n` and inspecting it, and adds a fixture. If none is found, `Name` stays empty.

`Tasks` replays tool calls in order:
- `TodoWrite` with `input.todos[]` of `{content,status}` replaces the whole list.
- `TaskCreate` with `input.subject` appends a task, id = count so far, status `pending`.
- `TaskUpdate` with `input.taskId`, `input.status` sets that task's status; `deleted` removes it.

`Done` = status `completed`; `Current` = subject of the first `in_progress` task. Returns `Known:false` when there are no task calls or a `TaskUpdate` names an unknown id. Unparseable lines are skipped.

- [ ] **Step 1: Failing tests** with hand-written fixtures in the shape above:
  - `TestListReadsIDAndCwd`; `TestFindByPrefixNameAndAmbiguity`.
  - `TestTasksTodoWrite`: two writes, last one 2 completed of 3 with one in progress → `{2,3,"<content>",true}`.
  - `TestTasksCreateUpdate`: 3 creates, updates `1→completed`, `2→in_progress` → `{1,3,"<subject 2>",true}`.
  - `TestTasksUnknownID` → `Known == false`. `TestTasksNone` → `Known == false`.
  - `TestTruncatedLastLine` (Review Focus 2): fixture whose final line is cut mid-object → same result as without that line.
- [ ] **Step 2–4:** Fail, implement, pass. Commit.

### Task 6: Reporter

**Files:** Create `internal/report/report.go`, `report_test.go`; modify `cmd/cs/main.go`.

**Consumes:** Tasks 2, 3, 5. **Produces:**

```go
func Handle(p paths.Paths, event string, stdin io.Reader, stdout io.Writer, getenv func(string) string)
func InjectedSettings(csBin, userStatusLine string) []byte
func UserStatusLine(claudeDir string) string   // statusLine.command from settings.json, or ""
```

Events (hook → event): `SessionStart→session-start`, `UserPromptSubmit→prompt`, `Stop→stop`, `Notification→notification`, `SessionEnd→session-end`, `TaskCreated`/`TaskCompleted→tasks`, status line `→statusline`. `InjectedSettings` returns settings JSON with one command hook per event running `<csBin> report <event>` and `statusLine: {type:"command", command:"<csBin> report statusline"}`.

`Handle` does nothing unless `getenv("CS_SESSION") == "1"`. It parses stdin (`session_id`, `transcript_path`, `cwd`, and for statusline the fields listed in the spec), then `UpdateSession`:
- state: `prompt→working`, `stop→idle`, `notification→needs_you`, `session-end→ended`, `session-start→idle`.
- `stop` and `tasks`: `Tasks = transcript.Tasks(transcript_path)`.
- `stop`: if `SwitchTo != ""` and `WrapperPID > 0`, send that pid `SIGUSR1`.
- `statusline`: set `Name`, `Model`, `ContextPct`, `CostUSD`; `Identity = accounts.ResolveIdentity(getenv("CLAUDE_CONFIG_DIR"))`, `AccountKey = Identity.Key(Profile)`; if `rate_limits` is present, `SaveUsage` under that key with `SeenAt = now`. Then output: if `getenv("CS_USER_STATUSLINE")` is non-empty, run it with the same stdin (2 s timeout) and print its output; otherwise print `<Identity.Label> · 5h NN% · wk NN%`, omitting absent parts.

Every error is logged with `store.Logf` and swallowed. `main.go` wires `cs report <event>` and always exits 0.

- [ ] **Step 1: Failing tests.**
  - `TestHandleIgnoredOutsideCS`: no `CS_SESSION` → no file created, no output.
  - `TestStateTransitions`: each event produces the state above.
  - `TestStatuslineRecordsIdentityAndUsage`: stdin with `rate_limits.five_hour.used_percentage: 62`, `resets_at` epoch; fixture `.claude.json` in the config dir → session identity set, usage file exists under `org_acc` key with 62 and the right reset time.
  - `TestStatuslineWithoutRateLimits`: no usage file written; output has no `5h`.
  - `TestStatuslineChainsUserCommand`: `CS_USER_STATUSLINE='cat >/dev/null; echo mine'` → stdout `mine`.
  - `TestIdentityFollowsRelogin`: second report after rewriting `.claude.json` with another account → identity and key change.
  - `TestHandleNeverFails`: garbage stdin, unwritable `CS_HOME` → returns, nothing on stdout for hook events.
  - `TestInjectedSettingsShape`: parses as JSON; has the seven hook events and the status line command.
- [ ] **Step 2–4:** Fail, implement, pass. Commit.

### Task 7: Wrapper — `cs new` and `cs resume`

**Files:** Create `internal/run/run.go`, `run_test.go`, `testdata/fakeclaude`; modify `cmd/cs/main.go`.

**Consumes:** Tasks 2–6. **Produces:**

```go
type Opts struct{ Account, Name, SessionRef string; Force bool; ClaudeArgs []string }
func New(p paths.Paths, cwd string, o Opts, stdio Stdio) int      // exit code
func Resume(p paths.Paths, cwd string, o Opts, stdio Stdio) int
type Stdio struct{ In io.Reader; Out, Err io.Writer }
type Choice struct{ Name, Label, Usage string }   // one offered account in the picker
func BuildCommand(p paths.Paths, a accounts.Account, s project.Settings, claudeArgs []string) *exec.Cmd
func ChooseAccount(p paths.Paths, o Opts, prev string, s project.Settings, ask func([]Choice) string) (string, error)
```

`testdata/fakeclaude`: a shell script. For `auth status` it prints `{"loggedIn":true}` unless a file `$CLAUDE_CONFIG_DIR/loggedout` exists. Otherwise it appends its arguments and `CLAUDE_CONFIG_DIR`, `CS_SESSION`, `ANTHROPIC_API_KEY` values to `$FAKE_LOG`, pipes a canned Stop payload (session id taken from `--session-id`/`--resume`, `transcript_path`, `cwd`) into `$CS_BIN report stop`, then sleeps until signalled, or exits at once when `$FAKE_EXIT_NOW` is set.

Launch sequence for both commands:
1. Session id: new → fresh UUID (v4 from `crypto/rand`); resume → `transcript.Find`, or, with no ref, a numbered picker of sessions whose `Cwd` is under `project.Root(cwd)`, newest first.
2. Account: `ChooseAccount` — `o.Account`, else the session's last `Profile`, else `DefaultAccount`, else `ask` (each choice shows label and last-known usage; disallowed accounts are not offered).
3. Checks, each failing with one clear line and exit code 1: account exists; `EnsureLinks`; `project.Allowed`; shared `settings.json` has no `apiKeyHelper`; on resume, unless `Force`, `IsLocked` → "already running", `RecentlyWritten` or a `ps -axo args` line containing the id → "appears to be open in another terminal".
4. `AcquireLock`; write the session record (`State:"starting"`, `Managed:true`, `WrapperPID`, `Profile`, `Cwd`, `ProjectRoot`, `StartedAt`).
5. `BuildCommand`: argv is `ClaudeBin --session-id <id>` or `--resume <id>`, `--name <n>` when given, `--settings <temp file from InjectedSettings>`, then `ClaudeArgs`. With `s.Launch` set, run `sh -c '<template> "$@"' cs <args…>` after replacing `{claude}`, `{config_dir}`, `{state_dir}` with shell-quoted values. Env: inherited minus the three stripped variables (print one notice naming any that were removed), plus `CLAUDE_CONFIG_DIR`, `CS_SESSION=1`, `CS_USER_STATUSLINE`. Working directory `cwd`; stdio attached.
6. Ignore `SIGINT`/`SIGQUIT` in the wrapper (the terminal delivers them to `claude`); forward `SIGTERM`/`SIGHUP`. Wait.
7. On exit: if a switch is pending, hand over to Task 8's `relaunch`; else set `State:"ended"`, release the lock, return the child's exit code.

CLI: `cs new [-a account] [-n name] [-- claude args]`, `cs resume [session] [-a account] [--force] [-- claude args]`.

- [ ] **Step 1: Failing tests** (all with `CS_CLAUDE_BIN=testdata/fakeclaude`, `FAKE_EXIT_NOW=1`):
  - `TestNewLaunchesUnderAccount`: log shows `--session-id`, `--settings`, `CLAUDE_CONFIG_DIR` = profile dir, `CS_SESSION=1`; session file ends `ended`, `Managed`, `Profile` set; lock released.
  - `TestStripsCredentialEnv`: with `ANTHROPIC_API_KEY=x` in the parent env, the fake logs it empty and stderr names the variable.
  - `TestRefusesApiKeyHelper`: shared settings contain `apiKeyHelper` → exit 1, fake never ran.
  - `TestResumeGuards`: held lock → "already running"; transcript touched now → "appears to be open"; `Force` passes both.
  - `TestAllowListEnforced`: project restricted to another org → exit 1 with `project.Allowed`'s reason.
  - `TestChooseAccountOrder`: explicit > previous > project default > ask.
  - `TestLaunchTemplateWithSpaces` (Review Focus 4): cwd and `CS_HOME` containing spaces, `Launch = "env WRAPPED=1 {claude}"` → fake runs, args intact, `{config_dir}`/`{state_dir}` substitute to the right single arguments.
  - `TestResumeFindsSessionFromOtherAccount`: transcript created under the shared store, resume with `-a second` → `--resume <id>` with the second profile's config dir.
- [ ] **Step 2–4:** Fail, implement, pass. Commit.

### Task 8: Switch

**Files:** Create `internal/run/switch.go`, `switch_test.go`; modify `run.go`, `cmd/cs/main.go`.

**Consumes:** Task 7. **Produces:**

```go
func Switch(p paths.Paths, ref, account string, force bool) error   // the `cs switch` side
func (w *wrapper) relaunch() (next *exec.Cmd, ok bool)              // the wrapper side
```

`Switch` resolves the session (managed and running, else error "not running under cs; use cs resume -a"), validates the account now (exists, `Allowed`, `LoggedIn`) and returns that error without touching the session, then sets `SwitchTo`/`SwitchForce` and sends `SIGUSR1` to `WrapperPID`. Prints `switching now` when the session is idle or forced, else `will switch when the current turn finishes`.

Wrapper, on `SIGUSR1` or a 1 s tick: if `SwitchTo != ""` and (`State == "idle"` or `SwitchForce`): re-validate the target; if invalid, clear the request, print the reason, keep running. Otherwise send the child `SIGTERM`, wait up to 10 s, then `SIGKILL`; clear the request; set `Profile` to the new account; start `BuildCommand(... --resume <id>)` in the same terminal; print `→ now on <label>`. If the new process exits non-zero within 5 s, print the reason, restore the previous `Profile`, and relaunch under the previous account once.

- [ ] **Step 1: Failing tests** (fake claude sleeping until signalled):
  - `TestSwitchIdleSession`: start under `a`; wait for `idle`; `Switch(…, "b")` → fake log gains a second entry with `--resume <same id>` and profile `b`'s config dir; session `Profile == "b"`, same id, lock still held.
  - `TestSwitchWaitsForIdle`: state forced to `working` → no relaunch within 2 s; after `cs report stop` → relaunch.
  - `TestSwitchForce`: `working` + force → relaunch at once.
  - `TestSwitchToSignedOutAccount` (Review Focus 3): `b/loggedout` exists → `Switch` returns an error naming `b`; child pid unchanged and alive.
  - `TestSwitchFallsBack` (Review Focus 3): fake exits 1 at once when `CLAUDE_CONFIG_DIR` is `b` (flag file) → third log entry is under `a`; `Profile == "a"`; message printed.
  - `TestSwitchUnmanaged`: error text above.
- [ ] **Step 2–4:** Fail, implement, pass with `-race`. Commit.

### Task 9: Dashboard

**Files:** Create `internal/dash/dash.go`, `dash_test.go`; modify `cmd/cs/main.go`.

**Consumes:** Tasks 2–5. **Produces:**

```go
type Row struct {
    Project, Session, State, Account, FiveHour, Week, Ctx, Task, Plan string
    Level   int   // 0 normal, 1 amber, 2 red (highest of the row's usage cells; 2 for "not allowed")
    Running bool
}
type AccountRow struct{ Label, FiveHour, Week, Resets string; Level int }
func Build(p paths.Paths, now time.Time, all bool) ([]Row, []AccountRow)
func UsageCell(w *store.Window, seenAt, now time.Time) (text string, level int)
func RenderTable(w io.Writer, rows []Row, accts []AccountRow, width int, color bool)
func RenderJSON(w io.Writer, rows []Row, accts []AccountRow) error
func Watch(p paths.Paths, out *os.File, all bool)   // redraw every 2 s until Ctrl-C
```

`Build`: merge `store.LoadSessions` with `transcript.List`. A managed session whose lock is not held is `ended`. A locked session still in `starting` is `running, no reports`. Transcripts with no state file are `unmanaged` (tasks from `transcript.Tasks`, account `—`). Default set: running sessions, then sessions ended or last written within 24 h; `all` lists everything. Order: running first, then newest. `Project` = base name of the project root. `Session` = name, else the first 8 characters of the id. State text: `working`, `idle`, `needs you`, `ended`, `unmanaged`, `running, no reports`. If the project has an allow-list and the row's identity fails `project.Allowed`, append ` · not allowed` to the account and set level 2. `Task` = `done/total current` or `—`. `Plan` from `project.Progress` or `—`. One `AccountRow` per registered account: its identity's usage file if any, else `— (never seen)`.

`UsageCell`: nil → `—`; `now` after `ResetsAt` → `reset`; else `NN%`, plus ` (12m)` when `seenAt` is more than 5 min old; level 1 at ≥ 75, 2 at ≥ 90.

`RenderTable`: header and columns as in the spec's dashboard sketch; each column sized to its content, then `Session`, `Task`, `Account` shortened with `…` until the row fits `width`; never wraps. Colour codes only when `color` is true. CLI: `cs` / `cs status` → `Watch` when stdout is a terminal, otherwise one plain table; `--once`, `--json`, `--all`. Width from `COLUMNS` or `stty size`, default 120.

- [ ] **Step 1: Failing tests.**
  - `TestUsageCell`: nil; 62 % fresh → `62%`,0; 80 % → level 1; 90 % → level 2; seen 12 min ago → `62% (12m)`; reset time passed → `reset`,0.
  - `TestBuildStates`: fixtures for managed running, managed with released lock, `starting` with lock, transcript only → the four state strings; ordering running-first.
  - `TestBuildSkipsCorruptState` (Review Focus 2): a corrupt session file beside a good one → good row present.
  - `TestBuildMarksDisallowedAccount`: level 2 and `not allowed` in the account cell.
  - `TestBuild24hWindow`: a 30-hour-old ended session is hidden, shown with `all`.
  - `TestRenderNarrowAndPlain` (Review Focus 5): width 60 with long names → every line ≤ 60 runes, contains `…`, and with `color=false` no byte `0x1b`.
  - `TestRenderJSONRoundTrips`.
- [ ] **Step 2–4:** Fail, implement, pass. Commit.

### Task 10: End-to-end test, README, acceptance

**Files:** Create `e2e_test.go` (package `main_test` in `cmd/cs`), `README.md`; modify `docs/acceptance.md`.

- [ ] **Step 1:** `TestEndToEnd`: build `bin/cs`; with temp homes and the fake claude: `cs account add work` (fake `auth login` succeeds), `cs new -a default -n demo` in a git repo containing `PLAN.md` with 1 of 2 boxes ticked, wait for `idle`, `cs switch demo work`, then `cs status --json` → one running row, session `demo`, profile `work`, plan `1/2`. Kill the wrapper; `cs status --json` shows `ended`.
- [ ] **Step 2:** Run `go test -race ./...` and `go vet ./...`; both clean.
- [ ] **Step 3:** `README.md`: install; first run (`cs account add`, `cs new`, `cs`); the command list from the spec; per-project settings with examples (`plan.command` counting done entries in a JSON tracker, and a `launch` template for a sandbox wrapper using `{config_dir}` and `{state_dir}`); known limits (usage of idle accounts is last-known; per-account MCP servers; plain `claude --resume` is not guarded; subscription logins only; a switch sends the conversation through the new account's organization).
- [ ] **Step 4:** Add to `docs/acceptance.md` the owner's two-account checklist: run R1–R3 and record results; add a second account; start a session on one, switch to the other, confirm the conversation continues and the dashboard shows the new account and its usage; `/login` inside a session and confirm the row changes; set `allowed_accounts` and confirm a refusal. Note which fallback applies if R2 or R3 failed: R2 → hooks in shared `settings.json` guarded by `CS_SESSION` (already guarded in `report.Handle`), installed by a new `cs hooks install`; R3 → change `accounts.ResolveIdentity` only.
- [ ] **Step 5:** Commit.

---

## Not in this plan

- Any project's sandbox profile changes (each project's own task).
- The R2 fallback command, built only if R2 fails.
