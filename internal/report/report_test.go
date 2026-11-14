package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/store"
)

const sid = "11111111-2222-4333-8444-555555555555"

type env map[string]string

func (e env) get(k string) string { return e[k] }

func setup(t *testing.T) (paths.Paths, string, env) {
	t.Helper()
	d := t.TempDir()
	p := paths.Paths{Home: filepath.Join(d, "cs"), ClaudeDir: filepath.Join(d, "claude"), ClaudeBin: "claude"}
	cfg := filepath.Join(d, "acct")
	os.MkdirAll(cfg, 0o700)
	writeIdentity(t, cfg, "a@b.c", "acc1", "org1")
	return p, cfg, env{"CS_SESSION": "1", "CLAUDE_CONFIG_DIR": cfg}
}

func writeIdentity(t *testing.T, cfg, email, acc, org string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"oauthAccount": map[string]string{"emailAddress": email, "accountUuid": acc, "organizationUuid": org}})
	if err := os.WriteFile(filepath.Join(cfg, ".claude.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func hookInput(transcript string) string {
	b, _ := json.Marshal(map[string]string{"session_id": sid, "transcript_path": transcript, "cwd": "/work/my proj"})
	return string(b)
}

func handle(p paths.Paths, event, stdin string, e env) string {
	var out bytes.Buffer
	Handle(p, event, strings.NewReader(stdin), &out, e.get)
	return out.String()
}

func session(t *testing.T, p paths.Paths) store.Session {
	t.Helper()
	s, ok := store.GetSession(p, sid)
	if !ok {
		t.Fatal("no session state written")
	}
	return s
}

func TestHandleIgnoredOutsideCS(t *testing.T) {
	p, _, e := setup(t)
	delete(e, "CS_SESSION")
	if out := handle(p, "stop", hookInput(""), e); out != "" {
		t.Errorf("output %q", out)
	}
	if _, err := os.Stat(p.Home); !os.IsNotExist(err) {
		t.Error("state written outside a cs session")
	}
}

func TestStateTransitions(t *testing.T) {
	p, _, e := setup(t)
	for _, c := range []struct{ event, want string }{
		{"session-start", store.StateIdle}, {"prompt", store.StateWorking}, {"stop", store.StateIdle},
		{"notification", store.StateNeedsYou}, {"prompt", store.StateWorking}, {"session-end", store.StateEnded},
	} {
		if out := handle(p, c.event, hookInput(""), e); out != "" {
			t.Errorf("%s printed %q", c.event, out)
		}
		s := session(t, p)
		if s.State != c.want {
			t.Errorf("after %s state = %q, want %q", c.event, s.State, c.want)
		}
		if s.Cwd != "/work/my proj" {
			t.Errorf("cwd = %q", s.Cwd)
		}
	}
}

func TestTasksReadFromTranscript(t *testing.T) {
	p, _, e := setup(t)
	tr, _ := filepath.Abs(filepath.Join("..", "transcript", "testdata", "tasks-create-update.jsonl"))
	handle(p, "tasks", hookInput(tr), e)
	s := session(t, p)
	if s.Tasks != (store.TaskProgress{Done: 1, Total: 3, Current: "beta", Known: true}) {
		t.Errorf("tasks = %+v", s.Tasks)
	}
	if s.State != "" {
		t.Errorf("tasks event changed state to %q", s.State)
	}
	if s.TranscriptPath != tr {
		t.Errorf("transcript path = %q", s.TranscriptPath)
	}
}

func statusInput(rate string) string {
	return `{"session_id":"` + sid + `","session_name":"demo","cwd":"/work/my proj","model":{"display_name":"Opus"},` +
		`"cost":{"total_cost_usd":1.25},"context_window":{"used_percentage":48}` + rate + `}`
}

func TestStatuslineRecordsIdentityAndUsage(t *testing.T) {
	p, _, e := setup(t)
	store.UpdateSession(p, sid, func(s *store.Session) { s.Profile = "work" })
	out := handle(p, "statusline", statusInput(`,"rate_limits":{"five_hour":{"used_percentage":62,"resets_at":1800000000},"seven_day":{"used_percentage":31.5,"resets_at":1800500000}}`), e)
	s := session(t, p)
	if !s.Identity.Known || s.Identity.Email != "a@b.c" || s.AccountKey != "org1_acc1" {
		t.Errorf("identity = %+v key %q", s.Identity, s.AccountKey)
	}
	if s.Name != "demo" || s.Model != "Opus" || s.ContextPct == nil || *s.ContextPct != 48 || s.CostUSD == nil || *s.CostUSD != 1.25 {
		t.Errorf("session = %+v", s)
	}
	us := store.LoadUsage(p)
	if len(us) != 1 {
		t.Fatalf("usage files = %d", len(us))
	}
	u := us[0]
	if u.FiveHour == nil || u.FiveHour.UsedPct != 62 || !u.FiveHour.ResetsAt.Equal(time.Unix(1800000000, 0)) {
		t.Errorf("five hour = %+v", u.FiveHour)
	}
	if u.SevenDay == nil || u.SevenDay.UsedPct != 31.5 || u.Profile != "work" || time.Since(u.SeenAt) > time.Minute {
		t.Errorf("usage = %+v", u)
	}
	if _, err := os.Stat(p.UsageFile("org1_acc1")); err != nil {
		t.Errorf("usage not stored under the identity key: %v", err)
	}
	if strings.TrimSpace(out) != "a@b.c · 5h 62% · wk 32%" {
		t.Errorf("status line = %q", out)
	}
}

func TestStatuslineWithoutRateLimits(t *testing.T) {
	p, _, e := setup(t)
	out := handle(p, "statusline", statusInput(""), e)
	if len(store.LoadUsage(p)) != 0 {
		t.Error("usage written without rate limits")
	}
	if strings.TrimSpace(out) != "a@b.c" {
		t.Errorf("status line = %q", out)
	}
}

func TestStatuslineKeepsOtherWindow(t *testing.T) {
	p, _, e := setup(t)
	handle(p, "statusline", statusInput(`,"rate_limits":{"five_hour":{"used_percentage":10,"resets_at":1800000000},"seven_day":{"used_percentage":20,"resets_at":1800500000}}`), e)
	handle(p, "statusline", statusInput(`,"rate_limits":{"five_hour":{"used_percentage":11,"resets_at":1800000000}}`), e)
	u := store.LoadUsage(p)[0]
	if u.FiveHour.UsedPct != 11 || u.SevenDay == nil || u.SevenDay.UsedPct != 20 {
		t.Errorf("usage = %+v %+v", u.FiveHour, u.SevenDay)
	}
}

func TestStatuslineUnknownIdentity(t *testing.T) {
	p, cfg, e := setup(t)
	os.Remove(filepath.Join(cfg, ".claude.json"))
	store.UpdateSession(p, sid, func(s *store.Session) { s.Profile = "work" })
	out := handle(p, "statusline", statusInput(""), e)
	s := session(t, p)
	if s.Identity.Known || s.AccountKey != "profile_work" || strings.TrimSpace(out) != "work ?" {
		t.Errorf("identity %+v key %q line %q", s.Identity, s.AccountKey, out)
	}
}

func TestStatuslineChainsUserCommand(t *testing.T) {
	p, _, e := setup(t)
	e["CS_USER_STATUSLINE"] = `grep -c session_id; echo mine`
	out := handle(p, "statusline", statusInput(""), e)
	if out != "1\nmine\n" {
		t.Errorf("status line = %q, want the user's command output fed the same stdin", out)
	}
	if session(t, p).Name != "demo" {
		t.Error("state not recorded when chaining")
	}
}

func TestIdentityFollowsRelogin(t *testing.T) {
	p, cfg, e := setup(t)
	handle(p, "statusline", statusInput(""), e)
	writeIdentity(t, cfg, "other@x.y", "acc2", "org2")
	handle(p, "statusline", statusInput(""), e)
	s := session(t, p)
	if s.Identity.Email != "other@x.y" || s.AccountKey != "org2_acc2" {
		t.Errorf("identity = %+v key %q", s.Identity, s.AccountKey)
	}
}

func TestHandleNeverFails(t *testing.T) {
	p, _, e := setup(t)
	for _, ev := range []string{"stop", "statusline", "nonsense", ""} {
		handle(p, ev, "not json at all", e)
		handle(p, ev, "", e)
	}
	// Unwritable home: a file where the directory should be.
	blocked := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocked, []byte("x"), 0o600)
	p.Home = filepath.Join(blocked, "cs")
	if out := handle(p, "stop", hookInput(""), e); out != "" {
		t.Errorf("hook printed %q", out)
	}
	handle(p, "statusline", statusInput(""), e)
}

func TestInjectedSettingsShape(t *testing.T) {
	var d struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
		StatusLine struct{ Type, Command string }
	}
	if err := json.Unmarshal(InjectedSettings("/opt/my tools/cs"), &d); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SessionStart": "session-start", "UserPromptSubmit": "prompt", "Stop": "stop", "Notification": "notification",
		"SessionEnd": "session-end", "TaskCreated": "tasks", "TaskCompleted": "tasks",
	}
	if len(d.Hooks) != len(want) {
		t.Errorf("hook events = %d, want %d", len(d.Hooks), len(want))
	}
	for ev, arg := range want {
		h := d.Hooks[ev]
		if len(h) != 1 || len(h[0].Hooks) != 1 || h[0].Hooks[0].Type != "command" || h[0].Hooks[0].Command != `'/opt/my tools/cs' report `+arg {
			t.Errorf("%s hook = %+v", ev, h)
		}
	}
	if d.StatusLine.Type != "command" || d.StatusLine.Command != `'/opt/my tools/cs' report statusline` {
		t.Errorf("status line = %+v", d.StatusLine)
	}
}

func TestUserStatusLine(t *testing.T) {
	d := t.TempDir()
	if UserStatusLine(d) != "" {
		t.Error("no settings should give no command")
	}
	os.WriteFile(filepath.Join(d, "settings.json"), []byte(`{"statusLine":{"type":"command","command":"~/bin/line.sh"}}`), 0o600)
	if got := UserStatusLine(d); got != "~/bin/line.sh" {
		t.Errorf("got %q", got)
	}
}
