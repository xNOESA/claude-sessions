package run

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/project"
	"claude-sessions/internal/store"
)

var csBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "cs-bin")
	if err != nil {
		panic(err)
	}
	csBin = filepath.Join(dir, "cs")
	if out, err := exec.Command("go", "build", "-o", csBin, "claude-sessions/cmd/cs").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build cs: %v\n%s", err, out))
	}
	// Tests stop sessions with SIGTERM to this process; never let it kill the test binary.
	signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// syncBuf is a bytes.Buffer safe to read while the wrapper writes.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	t        *testing.T
	r        *Runner
	p        paths.Paths
	log      string
	out, err *syncBuf
}

// newFixture makes an isolated home (with a space in it), a project
// directory, the fake claude, and accounts "default" and "work".
func newFixture(t *testing.T, extraEnv ...string) *fixture {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "fakeclaude"))
	p := paths.Paths{Home: filepath.Join(d, "cs home"), ClaudeDir: filepath.Join(d, "home", ".claude"), ClaudeBin: fake}
	os.MkdirAll(p.ClaudeDir, 0o700)
	cwd := filepath.Join(d, "my proj")
	os.MkdirAll(cwd, 0o755)
	for name, ids := range map[string][2]string{"default": {"d@acme.com", "org-acme"}, "work": {"w@other.org", "org-other"}} {
		var a accounts.Account
		if name == "default" {
			a, _ = accounts.Get(p, name)
		} else if a, err = accounts.Create(p, name); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]any{"oauthAccount": map[string]string{"emailAddress": ids[0], "accountUuid": "acc-" + name, "organizationUuid": ids[1]}})
		os.WriteFile(filepath.Join(a.ConfigDir, ".claude.json"), b, 0o600)
	}
	f := &fixture{t: t, p: p, log: filepath.Join(d, "fake.log"), out: &syncBuf{}, err: &syncBuf{}}
	env := append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(d, "home"), "FAKE_LOG=" + f.log}, extraEnv...)
	f.r = &Runner{P: p, Cwd: cwd, CSBin: csBin, Environ: env,
		Stdio: Stdio{In: strings.NewReader(""), Out: f.out, Err: f.err}}
	return f
}

func (f *fixture) launches() []string {
	b, _ := os.ReadFile(f.log)
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(l, "ARGS=") {
			out = append(out, l)
		}
	}
	return out
}

func field(line, key string) string {
	for _, kv := range strings.Split(line, "|") {
		if strings.HasPrefix(kv, key+"=") {
			return strings.TrimPrefix(kv, key+"=")
		}
	}
	return ""
}

func (f *fixture) onlySession() store.Session {
	f.t.Helper()
	ss := store.LoadSessions(f.p)
	if len(ss) != 1 {
		f.t.Fatalf("want one session, got %d (stderr: %s)", len(ss), f.err.String())
	}
	return ss[0]
}

// transcriptFor writes a minimal transcript for id in the shared store.
func (f *fixture) transcriptFor(id string, age time.Duration) string {
	f.t.Helper()
	dir := filepath.Join(f.p.ClaudeDir, "projects", "-my-proj")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, id+".jsonl")
	line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": id, "cwd": f.r.Cwd, "message": map[string]string{"role": "user", "content": "hi"}})
	if err := os.WriteFile(path, append(line, '\n'), 0o600); err != nil {
		f.t.Fatal(err)
	}
	when := time.Now().Add(-age)
	os.Chtimes(path, when, when)
	return path
}

const oldID = "aaaa1111-0000-4000-8000-000000000001"

func TestNewLaunchesUnderAccount(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	if code := f.r.New(Opts{Account: "work", Name: "demo", ClaudeArgs: []string{"--model", "opus"}}); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	l := f.launches()
	if len(l) != 1 {
		t.Fatalf("launches = %v", l)
	}
	s := f.onlySession()
	args := field(l[0], "ARGS")
	for _, want := range []string{"--session-id " + s.ID, "--name demo", "--settings ", "--model opus"} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q missing %q", args, want)
		}
	}
	if got := field(l[0], "CONFIG"); got != f.p.AccountDir("work") {
		t.Errorf("CLAUDE_CONFIG_DIR = %q", got)
	}
	if field(l[0], "CS_SESSION") != "1" || field(l[0], "PROFILE") != "work" || field(l[0], "PWD") != f.r.Cwd {
		t.Errorf("env/cwd wrong: %s", l[0])
	}
	if s.State != store.StateEnded || !s.Managed || s.Profile != "work" || s.Name != "demo" || s.Cwd != f.r.Cwd || s.ProjectRoot != f.r.Cwd {
		t.Errorf("session = %+v", s)
	}
	if s.Identity.Email != "w@other.org" {
		t.Errorf("identity = %+v", s.Identity)
	}
	if store.IsLocked(f.p, s.ID) {
		t.Error("lock still held after exit")
	}
	if len(s.ID) != 36 {
		t.Errorf("session id %q is not a UUID", s.ID)
	}
}

// startAsync runs a launch in the background and waits for the fake to
// report idle. stop() ends the session the way the terminal closing would.
func (f *fixture) startAsync(launch func() int) (s store.Session, stop func() int) {
	f.t.Helper()
	codeCh := make(chan int, 1)
	go func() { codeCh <- launch() }()
	stopped := false
	stop = func() int {
		if stopped {
			return -1
		}
		stopped = true
		syscall.Kill(os.Getpid(), syscall.SIGTERM) // the wrapper forwards it to claude
		select {
		case c := <-codeCh:
			return c
		case <-time.After(10 * time.Second):
			f.t.Error("session did not stop")
			return -1
		}
	}
	f.t.Cleanup(func() { stop() })
	return f.waitState(store.StateIdle), stop
}

func (f *fixture) waitState(want string) store.Session {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ss := store.LoadSessions(f.p); len(ss) == 1 && ss[0].State == want {
			return ss[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("session never reached %q: %+v (stderr %s)", want, store.LoadSessions(f.p), f.err.String())
	return store.Session{}
}

func TestRunningSessionHoldsLockAndSettings(t *testing.T) {
	f := newFixture(t)
	s, stop := f.startAsync(func() int { return f.r.New(Opts{Account: "work"}) })
	if !store.IsLocked(f.p, s.ID) || s.WrapperPID != os.Getpid() {
		t.Errorf("running session not locked / no wrapper pid: %+v", s)
	}
	settings := filepath.Join(f.p.StateDir(), "settings", s.ID+".json")
	if !strings.Contains(field(f.launches()[0], "ARGS"), "--settings "+settings) {
		t.Errorf("args = %q", field(f.launches()[0], "ARGS"))
	}
	b, err := os.ReadFile(settings)
	var d map[string]any
	if err != nil || json.Unmarshal(b, &d) != nil || d["hooks"] == nil || d["statusLine"] == nil {
		t.Errorf("settings file: %v %s", err, b)
	}
	if code := stop(); code != 0 {
		t.Errorf("exit = %d", code)
	}
	if got := f.onlySession(); got.State != store.StateEnded || store.IsLocked(f.p, s.ID) {
		t.Errorf("after stop: %+v", got)
	}
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Error("settings file left behind")
	}
}

func TestChildExitCodeIsReturned(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1", "FAKE_EXIT_CODE=7")
	if code := f.r.New(Opts{Account: "work"}); code != 7 {
		t.Errorf("exit = %d, want 7", code)
	}
}

func TestDefaultAccountUsesSharedStore(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	if code := f.r.New(Opts{Account: "default"}); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if got := field(f.launches()[0], "CONFIG"); got != f.p.ClaudeDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q, want the shared store", got)
	}
}

func TestStripsCredentialEnv(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1", "ANTHROPIC_API_KEY=sk-x", "CLAUDE_CODE_OAUTH_TOKEN=tok")
	if code := f.r.New(Opts{Account: "work"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got := field(f.launches()[0], "APIKEY"); got != "" {
		t.Errorf("ANTHROPIC_API_KEY reached the session: %q", got)
	}
	e := f.err.String()
	if !strings.Contains(e, "ANTHROPIC_API_KEY") || !strings.Contains(e, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Errorf("no notice about removed variables: %q", e)
	}
	if strings.Contains(e, "sk-x") || strings.Contains(e, "tok") {
		t.Errorf("notice leaked a value: %q", e)
	}
}

func TestRefusesApiKeyHelper(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	os.WriteFile(filepath.Join(f.p.ClaudeDir, "settings.json"), []byte(`{"apiKeyHelper":"/bin/key.sh"}`), 0o600)
	if code := f.r.New(Opts{Account: "work"}); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if len(f.launches()) != 0 {
		t.Error("claude ran despite apiKeyHelper")
	}
	if !strings.Contains(f.err.String(), "apiKeyHelper") {
		t.Errorf("stderr = %q", f.err.String())
	}
}

func TestUnknownAccount(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	if code := f.r.New(Opts{Account: "nope"}); code != 1 || !strings.Contains(f.err.String(), `"nope"`) {
		t.Errorf("exit %d stderr %q", code, f.err.String())
	}
}

func TestResumeFindsSessionFromOtherAccount(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	f.transcriptFor(oldID, time.Hour)
	if code := f.r.Resume(Opts{SessionRef: "aaaa1111", Account: "work"}); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	l := f.launches()[0]
	if !strings.Contains(field(l, "ARGS"), "--resume "+oldID) || strings.Contains(field(l, "ARGS"), "--session-id") {
		t.Errorf("args = %q", field(l, "ARGS"))
	}
	if field(l, "CONFIG") != f.p.AccountDir("work") {
		t.Errorf("config = %q", field(l, "CONFIG"))
	}
	if s := f.onlySession(); s.ID != oldID || s.Profile != "work" || !s.Managed {
		t.Errorf("session = %+v", s)
	}
}

func TestResumeUnknownSession(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	if code := f.r.Resume(Opts{SessionRef: "zzzz", Account: "work"}); code != 1 || len(f.launches()) != 0 {
		t.Errorf("exit %d launches %v", code, f.launches())
	}
}

func TestResumeGuards(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	f.transcriptFor(oldID, time.Hour)

	release, err := store.AcquireLock(f.p, oldID)
	if err != nil {
		t.Fatal(err)
	}
	if code := f.r.Resume(Opts{SessionRef: oldID, Account: "work"}); code != 1 || !strings.Contains(f.err.String(), "already running") {
		t.Errorf("locked: exit %d stderr %q", code, f.err.String())
	}
	if code := f.r.Resume(Opts{SessionRef: oldID, Account: "work", Force: true}); code != 1 {
		t.Errorf("--force must not break another wrapper's lock: exit %d", code)
	}
	release()

	f.transcriptFor(oldID, 0) // written just now by something that is not cs
	f.err.b.Reset()
	if code := f.r.Resume(Opts{SessionRef: oldID, Account: "work"}); code != 1 || !strings.Contains(f.err.String(), "appears to be open") {
		t.Errorf("recent: exit %d stderr %q", code, f.err.String())
	}
	if len(f.launches()) != 0 {
		t.Fatalf("claude ran despite guards: %v", f.launches())
	}
	if code := f.r.Resume(Opts{SessionRef: oldID, Account: "work", Force: true}); code != 0 {
		t.Errorf("forced: exit %d stderr %q", code, f.err.String())
	}
}

func TestResumeRightAfterCleanExitIsNotBlocked(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	if code := f.r.New(Opts{Account: "default"}); code != 0 {
		t.Fatal(f.err.String())
	}
	s := f.onlySession()
	f.transcriptFor(s.ID, 0) // the session we just closed wrote its transcript seconds ago
	if code := f.r.Resume(Opts{SessionRef: s.ID, Account: "work"}); code != 0 {
		t.Errorf("exit %d: %s", code, f.err.String())
	}
}

func TestAllowListEnforced(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	project.Set(f.p, f.r.Cwd, "allowed_accounts", "default")
	if code := f.r.New(Opts{Account: "work"}); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if want := "w@other.org is not allowed for " + f.r.Cwd; !strings.Contains(f.err.String(), want) {
		t.Errorf("stderr = %q, want %q", f.err.String(), want)
	}
	if len(f.launches()) != 0 {
		t.Error("claude ran for a disallowed account")
	}
	if code := f.r.New(Opts{Account: "default"}); code != 0 {
		t.Errorf("allowed account refused: %s", f.err.String())
	}
}

func TestChooseAccountOrder(t *testing.T) {
	f := newFixture(t)
	asked := 0
	ask := func(c []Choice) string { asked++; return c[len(c)-1].Name }
	choose := func(o Opts, prev string, s project.Settings) string {
		t.Helper()
		got, err := ChooseAccount(f.p, f.r.Cwd, o, prev, s, ask)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	s := project.Settings{DefaultAccount: "default"}
	if got := choose(Opts{Account: "work"}, "default", s); got != "work" {
		t.Errorf("explicit: %q", got)
	}
	if got := choose(Opts{}, "work", s); got != "work" {
		t.Errorf("previous: %q", got)
	}
	if got := choose(Opts{}, "", s); got != "default" {
		t.Errorf("project default: %q", got)
	}
	if asked != 0 {
		t.Errorf("asked %d times before it was needed", asked)
	}
	if got := choose(Opts{}, "", project.Settings{}); got != "work" || asked != 1 {
		t.Errorf("ask: %q asked=%d", got, asked)
	}
	// A previous account that no longer exists falls through.
	if got := choose(Opts{}, "gone", s); got != "default" {
		t.Errorf("stale previous: %q", got)
	}
	// Disallowed accounts are not offered.
	asked = 0
	only := project.Settings{AllowedAccounts: []string{"w@other.org"}}
	if got := choose(Opts{}, "", only); got != "work" || asked != 0 {
		t.Errorf("single allowed account: %q asked=%d, want work without asking", got, asked)
	}
	if _, err := ChooseAccount(f.p, f.r.Cwd, Opts{}, "", project.Settings{AllowedAccounts: []string{"nobody@x.y"}}, ask); err == nil {
		t.Error("no allowed account should be an error")
	}
}

func TestLaunchTemplateWithSpaces(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	marker := filepath.Join(f.p.Home, "marker")
	project.Set(f.p, f.r.Cwd, "launch", `env WRAPPED=1 SEEN_CFG={config_dir} sh -c 'printf "%s|%s" "$SEEN_CFG" "$0" > "$1"; shift; exec "$@"' {state_dir} `+"'"+marker+"'"+` {claude}`)
	if code := f.r.New(Opts{Account: "work", Name: "two words"}); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	l := f.launches()
	if len(l) != 1 || field(l[0], "WRAPPED") != "1" {
		t.Fatalf("launches = %v", l)
	}
	if !strings.Contains(field(l[0], "ARGS"), "--name two words") {
		t.Errorf("args = %q", field(l[0], "ARGS"))
	}
	b, _ := os.ReadFile(marker)
	if want := f.p.AccountDir("work") + "|" + f.p.StateDir(); string(b) != want {
		t.Errorf("placeholders = %q, want %q", b, want)
	}
}

func TestChildDoesNotInheritIgnoredInterrupt(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	marker := filepath.Join(f.p.Home, "survived-sigint")
	// A child that inherits SIGINT as "ignored" cannot be interrupted, and
	// neither can anything it starts. The inner shell interrupts itself.
	project.Set(f.p, f.r.Cwd, "launch", `sh -c 'sh -c "kill -INT \$\$; echo ignored > \"\$0\""  "$0"; shift 0; exec "$@"' `+"'"+marker+"'"+` {claude}`)
	f.r.New(Opts{Account: "work"})
	if _, err := os.Stat(marker); err == nil {
		t.Error("the session's process started with Ctrl-C ignored")
	}
}

func TestPromptOnlyOnFirstLaunch(t *testing.T) {
	f := newFixture(t)
	s, _ := f.startAsync(func() int {
		return f.r.New(Opts{Account: "default", Prompt: "fix the bug", ClaudeArgs: []string{"--model", "opus"}})
	})
	if _, err := Switch(f.p, s.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	l := f.waitLaunches(2, 8*time.Second)
	if len(l) != 2 {
		t.Fatalf("launches = %v", l)
	}
	if !strings.HasSuffix(field(l[0], "ARGS"), "--model opus fix the bug") {
		t.Errorf("first launch args = %q", field(l[0], "ARGS"))
	}
	if strings.Contains(field(l[1], "ARGS"), "fix the bug") || !strings.Contains(field(l[1], "ARGS"), "--model opus") {
		t.Errorf("relaunch must keep flags and not resubmit the prompt: %q", field(l[1], "ARGS"))
	}
}

func TestRejectsClaudeArgsThatCsOwns(t *testing.T) {
	for _, bad := range []string{"--resume", "-r", "--session-id", "--settings", "-c", "--continue", "-p", "--print", "--fork-session"} {
		f := newFixture(t, "FAKE_EXIT_NOW=1")
		if code := f.r.New(Opts{Account: "work", ClaudeArgs: []string{bad, "x"}}); code != 1 || len(f.launches()) != 0 {
			t.Errorf("%s accepted (exit %d)", bad, code)
		}
		if !strings.Contains(f.err.String(), bad) {
			t.Errorf("%s: stderr %q", bad, f.err.String())
		}
	}
}

func TestCleanExitDoesNotExcuseLaterWrites(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	if code := f.r.New(Opts{Account: "default"}); code != 0 {
		t.Fatal(f.err.String())
	}
	s := f.onlySession()
	// cs recorded the end a while ago; something else has written since.
	s.UpdatedAt = time.Now().Add(-30 * time.Second)
	store.WriteJSON(f.p.SessionFile(s.ID), &s)
	f.transcriptFor(s.ID, 0)
	if code := f.r.Resume(Opts{SessionRef: s.ID, Account: "work"}); code != 1 || !strings.Contains(f.err.String(), "appears to be open") {
		t.Errorf("exit %d stderr %q", code, f.err.String())
	}
}
