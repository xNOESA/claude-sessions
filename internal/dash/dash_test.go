package dash

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/project"
	"claude-sessions/internal/store"
)

var now = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func TestUsageCell(t *testing.T) {
	future := now.Add(time.Hour)
	for _, c := range []struct {
		name  string
		w     *store.Window
		seen  time.Time
		text  string
		level int
	}{
		{"never seen", nil, time.Time{}, "—", 0},
		{"fresh", &store.Window{UsedPct: 62, ResetsAt: future}, now.Add(-time.Minute), "62%", 0},
		{"amber at 75", &store.Window{UsedPct: 75, ResetsAt: future}, now, "75%", 1},
		{"below amber", &store.Window{UsedPct: 74.4, ResetsAt: future}, now, "74%", 0},
		{"red at 90", &store.Window{UsedPct: 90, ResetsAt: future}, now, "90%", 2},
		{"stale", &store.Window{UsedPct: 62, ResetsAt: future}, now.Add(-12 * time.Minute), "62% (12m)", 0},
		{"stale hours", &store.Window{UsedPct: 95, ResetsAt: future}, now.Add(-3 * time.Hour), "95% (3h)", 2},
		{"exactly 5 min is fresh", &store.Window{UsedPct: 10, ResetsAt: future}, now.Add(-5 * time.Minute), "10%", 0},
		{"window has reset", &store.Window{UsedPct: 99, ResetsAt: now.Add(-time.Second)}, now.Add(-time.Hour), "reset", 0},
		{"no reset time known", &store.Window{UsedPct: 50}, now, "50%", 0},
	} {
		text, level := UsageCell(c.w, c.seen, now)
		if text != c.text || level != c.level {
			t.Errorf("%s: got %q/%d, want %q/%d", c.name, text, level, c.text, c.level)
		}
	}
}

type world struct {
	t *testing.T
	p paths.Paths
	d string
}

func newWorld(t *testing.T) *world {
	d := t.TempDir()
	p := paths.Paths{Home: filepath.Join(d, "cs"), ClaudeDir: filepath.Join(d, "home", ".claude"), ClaudeBin: "claude"}
	os.MkdirAll(filepath.Join(p.ClaudeDir, "projects"), 0o700)
	w := &world{t, p, d}
	w.identity(p.ClaudeDir, "d@acme.com", "acc-d", "org-acme")
	return w
}

func (w *world) identity(cfg, email, acc, org string) {
	b, _ := json.Marshal(map[string]any{"oauthAccount": map[string]string{"emailAddress": email, "accountUuid": acc, "organizationUuid": org}})
	os.MkdirAll(cfg, 0o700)
	os.WriteFile(filepath.Join(cfg, ".claude.json"), b, 0o600)
}

func (w *world) session(id string, fn func(*store.Session)) {
	w.t.Helper()
	if err := store.UpdateSession(w.p, id, func(s *store.Session) {
		s.Managed, s.Profile, s.State = true, "default", store.StateIdle
		s.Cwd, s.ProjectRoot = filepath.Join(w.d, "proj"), filepath.Join(w.d, "proj")
		s.Identity = store.Identity{Email: "d@acme.com", OrgID: "org-acme", AccountID: "acc-d", Known: true}
		s.AccountKey = s.Identity.Key("default")
		fn(s)
	}); err != nil {
		w.t.Fatal(err)
	}
}

// backdate rewrites a state file's UpdatedAt (UpdateSession stamps "now").
func (w *world) backdate(id string, at time.Time) {
	s, _ := store.GetSession(w.p, id)
	s.UpdatedAt = at
	store.WriteJSON(w.p.SessionFile(id), &s)
}

func (w *world) lock(id string) {
	rel, err := store.AcquireLock(w.p, id)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(rel)
}

func (w *world) transcript(id, fixture string, mod time.Time) {
	b, err := os.ReadFile(filepath.Join("..", "transcript", "testdata", fixture))
	if err != nil {
		w.t.Fatal(err)
	}
	dir := filepath.Join(w.p.ClaudeDir, "projects", "-work-my-proj")
	os.MkdirAll(dir, 0o700)
	f := filepath.Join(dir, id+".jsonl")
	os.WriteFile(f, b, 0o600)
	os.Chtimes(f, mod, mod)
}

func byID(rows []Row) map[string]Row {
	m := map[string]Row{}
	for _, r := range rows {
		m[r.ID] = r
	}
	return m
}

func TestBuildStates(t *testing.T) {
	w := newWorld(t)
	real := time.Now()
	w.session("s-working", func(s *store.Session) { s.State = store.StateWorking; s.Name = "busy" })
	w.lock("s-working")
	w.session("s-needs", func(s *store.Session) { s.State = store.StateNeedsYou })
	w.lock("s-needs")
	w.session("s-noreports", func(s *store.Session) { s.State = store.StateStarting })
	w.lock("s-noreports")
	w.session("s-crashed", func(s *store.Session) { s.State = store.StateWorking }) // lock gone: wrapper died
	w.transcript("aaaa1111-0000-4000-8000-000000000001", "tasks-create-update.jsonl", real.Add(-time.Hour))

	rows, _ := Build(w.p, real, false)
	m := byID(rows)
	want := map[string]string{
		"s-working": "working", "s-needs": "needs you", "s-noreports": "running, no reports",
		"s-crashed": "ended", "aaaa1111-0000-4000-8000-000000000001": "unmanaged",
	}
	if len(rows) != len(want) {
		t.Fatalf("rows = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for id, state := range want {
		if m[id].State != state {
			t.Errorf("%s state = %q, want %q", id, m[id].State, state)
		}
	}
	for i, r := range rows {
		if i < 3 && !r.Running || i >= 3 && r.Running {
			t.Errorf("running sessions must come first: row %d = %+v", i, r)
		}
	}
	if r := m["s-working"]; r.Session != "busy" || r.Project != "proj" || r.Account != "d@acme.com" {
		t.Errorf("working row = %+v", r)
	}
	if r := m["s-needs"]; r.Session != "s-needs" {
		t.Errorf("unnamed session should show its short id: %+v", r)
	}
	u := m["aaaa1111-0000-4000-8000-000000000001"]
	if u.Session != "demo-name" || u.Task != "1/3 beta" || u.Account != "—" || u.Project != "my proj" {
		t.Errorf("unmanaged row = %+v", u)
	}
}

func TestBuildUsageTaskPlanCells(t *testing.T) {
	w := newWorld(t)
	real := time.Now()
	root := filepath.Join(w.d, "proj")
	os.MkdirAll(root, 0o755)
	os.WriteFile(filepath.Join(root, "PLAN.md"), []byte("- [x] a\n- [ ] b\n"), 0o644)
	ctx := 48.2
	w.session("s1", func(s *store.Session) {
		s.ContextPct = &ctx
		s.Tasks = store.TaskProgress{Done: 3, Total: 5, Current: "fix relay", Known: true}
	})
	w.lock("s1")
	id := store.Identity{Email: "d@acme.com", OrgID: "org-acme", AccountID: "acc-d", Known: true}
	store.SaveUsage(w.p, store.Usage{Identity: id, Profile: "default", SeenAt: real,
		FiveHour: &store.Window{UsedPct: 92, ResetsAt: real.Add(time.Hour)},
		SevenDay: &store.Window{UsedPct: 31, ResetsAt: real.Add(48 * time.Hour)}})

	rows, accts := Build(w.p, real, false)
	r := rows[0]
	if r.FiveHour != "92%" || r.Week != "31%" || r.Level != 2 || r.Ctx != "48%" || r.Task != "3/5 fix relay" || r.Plan != "1/2" {
		t.Errorf("row = %+v", r)
	}
	if len(accts) != 1 || accts[0].Label != "d@acme.com" || accts[0].FiveHour != "92%" || accts[0].Week != "31%" || accts[0].Level != 2 || accts[0].Name != "default" {
		t.Errorf("accounts = %+v", accts)
	}

	// An account never seen shows dashes.
	a, _ := accounts.Create(w.p, "work")
	w.identity(a.ConfigDir, "w@other.org", "acc-w", "org-other")
	_, accts = Build(w.p, real, false)
	if len(accts) != 2 || accts[1].Name != "work" || accts[1].FiveHour != "—" || accts[1].Week != "—" || accts[1].Label != "w@other.org" {
		t.Errorf("accounts = %+v", accts)
	}
}

func TestBuildSkipsCorruptState(t *testing.T) {
	w := newWorld(t)
	w.session("good", func(s *store.Session) {})
	w.lock("good")
	os.WriteFile(w.p.SessionFile("bad"), []byte(`{"ID": "bad", "State":`), 0o600)
	os.WriteFile(filepath.Join(w.p.UsageDir(), "junk.json"), []byte(`{{{`), 0o600)
	os.MkdirAll(w.p.UsageDir(), 0o700)
	os.WriteFile(filepath.Join(w.p.UsageDir(), "junk.json"), []byte(`{{{`), 0o600)
	rows, _ := Build(w.p, time.Now(), false)
	if len(rows) != 1 || rows[0].ID != "good" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestBuildMarksDisallowedAccount(t *testing.T) {
	w := newWorld(t)
	w.session("s1", func(s *store.Session) {})
	w.lock("s1")
	project.Set(w.p, filepath.Join(w.d, "proj"), "allowed_accounts", "someone@else.com")
	rows, _ := Build(w.p, time.Now(), false)
	if r := rows[0]; r.Level != 2 || r.Account != "d@acme.com · not allowed" {
		t.Errorf("row = %+v", r)
	}
}

func TestBuild24hWindow(t *testing.T) {
	w := newWorld(t)
	real := time.Now()
	w.session("old", func(s *store.Session) { s.State = store.StateEnded })
	w.backdate("old", real.Add(-30*time.Hour))
	w.session("recent", func(s *store.Session) { s.State = store.StateEnded })
	w.backdate("recent", real.Add(-23*time.Hour))
	w.transcript("aaaa1111-0000-4000-8000-000000000001", "no-tasks.jsonl", real.Add(-30*time.Hour))

	rows, _ := Build(w.p, real, false)
	if len(rows) != 1 || rows[0].ID != "recent" {
		t.Errorf("default view = %+v", rows)
	}
	rows, _ = Build(w.p, real, true)
	if len(rows) != 3 {
		t.Errorf("--all view has %d rows, want 3", len(rows))
	}
}

func sampleRows() ([]Row, []AccountRow) {
	return []Row{
			{ID: "1", Project: "api", Session: "week-10 kind run with a very long descriptive name", State: "working",
				Account: "ana.somebody@a-long-organization-name.example · Max", FiveHour: "92%", FiveHourLevel: 2, Week: "76%", WeekLevel: 1, Ctx: "48%",
				Task: "3/5 fix the outbox relay so that it retries on conflict", Plan: "week 10/14", Level: 2, Running: true},
			{ID: "2", Project: "site", Session: "mailer", State: "ended", Account: "bo@other.org", FiveHour: "8%", Week: "12%", Ctx: "—", Task: "—", Plan: "—"},
		}, []AccountRow{
			{Name: "default", Label: "ana.somebody@a-long-organization-name.example · Max", FiveHour: "92%", Week: "76%", Resets: "14:10", Level: 2},
			{Name: "work", Label: "bo@other.org", FiveHour: "—", Week: "—"},
		}
}

func TestRenderNarrowAndPlain(t *testing.T) {
	rows, accts := sampleRows()
	var b bytes.Buffer
	RenderTable(&b, rows, accts, 60, false)
	out := b.String()
	if strings.ContainsRune(out, 0x1b) {
		t.Error("colour codes written with color=false")
	}
	if !strings.Contains(out, "…") {
		t.Error("nothing was shortened")
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if n := utf8.RuneCountInString(line); n > 60 {
			t.Errorf("line is %d wide, limit 60: %q", n, line)
		}
	}
	for _, must := range []string{"PROJECT", "api", "working", "92%", "week 10/14", "site", "ACCOUNTS"} {
		if !strings.Contains(out, must) {
			t.Errorf("narrow output lost %q:\n%s", must, out)
		}
	}
}

func TestRenderWideKeepsEverything(t *testing.T) {
	rows, accts := sampleRows()
	var b bytes.Buffer
	RenderTable(&b, rows, accts, 400, false)
	out := b.String()
	if strings.Contains(out, "…") {
		t.Errorf("shortened although there was room:\n%s", out)
	}
	for _, must := range []string{"week-10 kind run with a very long descriptive name", "3/5 fix the outbox relay so that it retries on conflict", "resets 14:10"} {
		if !strings.Contains(out, must) {
			t.Errorf("missing %q:\n%s", must, out)
		}
	}
}

func TestRenderColor(t *testing.T) {
	rows, accts := sampleRows()
	var b bytes.Buffer
	RenderTable(&b, rows, accts, 400, true)
	out := b.String()
	if !strings.Contains(out, "\x1b[31m92%") || !strings.Contains(out, "\x1b[33m76%") {
		t.Errorf("usage cells not coloured by level:\n%q", out)
	}
}

func TestRenderEmpty(t *testing.T) {
	var b bytes.Buffer
	RenderTable(&b, nil, []AccountRow{{Name: "default", Label: "default ?", FiveHour: "—", Week: "—"}}, 80, false)
	if !strings.Contains(b.String(), "cs new") {
		t.Errorf("empty dashboard should say how to start: %q", b.String())
	}
}

func TestRenderJSONRoundTrips(t *testing.T) {
	rows, accts := sampleRows()
	var b bytes.Buffer
	if err := RenderJSON(&b, rows, accts); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sessions []Row
		Accounts []AccountRow
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 2 || got.Sessions[0] != rows[0] || len(got.Accounts) != 2 || got.Accounts[0] != accts[0] {
		t.Errorf("round trip changed data: %+v", got)
	}
	b.Reset()
	RenderJSON(&b, nil, nil)
	if !strings.Contains(b.String(), `"Sessions": []`) {
		t.Errorf("empty JSON should have empty arrays, got %s", b.String())
	}
}

func TestRenderStripsControlCharacters(t *testing.T) {
	rows := []Row{{ID: "1", Project: "p", Session: "evil\x1b[2Jname\ttab", State: "idle", Account: "a@b.c",
		FiveHour: "—", Week: "—", Ctx: "—", Task: "1/2 line one\nline two\x07", Plan: "—", Running: true}}
	var b bytes.Buffer
	RenderTable(&b, rows, nil, 200, false)
	out := b.String()
	if strings.ContainsAny(out, "\x1b\x07\t") {
		t.Errorf("control characters reached the terminal: %q", out)
	}
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n"); n != 1 {
		t.Errorf("a newline in a task split the row: %q", out)
	}
	if !strings.Contains(out, "evil") || !strings.Contains(out, "line two") {
		t.Errorf("text lost: %q", out)
	}
}
