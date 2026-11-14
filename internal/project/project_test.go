package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/store"
)

func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	d := t.TempDir()
	return paths.Paths{Home: filepath.Join(d, "cs"), ClaudeDir: filepath.Join(d, "claude"), ClaudeBin: "claude"}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func realpath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRootGitWorktreeAndSpaces(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := realpath(t, t.TempDir())
	repo := filepath.Join(base, "my proj")
	sub := filepath.Join(repo, "a", "b")
	os.MkdirAll(sub, 0o755)
	git(t, repo, "init", "-q")
	os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-qm", "x")
	wt := filepath.Join(base, "wt copy")
	git(t, repo, "worktree", "add", "-q", "-b", "wt", wt)

	for _, dir := range []string{repo, sub, wt} {
		if got := Root(dir); got != repo {
			t.Errorf("Root(%q) = %q, want %q", dir, got, repo)
		}
	}
	plain := filepath.Join(base, "plain dir")
	os.MkdirAll(plain, 0o755)
	if got := Root(plain); got != plain {
		t.Errorf("Root(non-git) = %q", got)
	}
}

func TestSettingsSetLoadClear(t *testing.T) {
	p := testPaths(t)
	root := "/some/my proj"
	for k, v := range map[string]string{
		"plan.command": "echo 1 2", "plan.file": "docs/p.md", "default_account": "work",
		"launch": "wrap {claude}", "allowed_accounts": "work, a@b.c ,Acme",
	} {
		if err := Set(p, root, k, v); err != nil {
			t.Fatal(k, err)
		}
	}
	s := Load(p, root)
	if s.PlanCommand != "echo 1 2" || s.PlanFile != "docs/p.md" || s.DefaultAccount != "work" || s.Launch != "wrap {claude}" {
		t.Errorf("got %+v", s)
	}
	if strings.Join(s.AllowedAccounts, "|") != "work|a@b.c|Acme" {
		t.Errorf("allowed = %q", s.AllowedAccounts)
	}
	if other := Load(p, "/elsewhere"); other.PlanFile != "" || len(other.AllowedAccounts) != 0 {
		t.Errorf("settings leaked to another project: %+v", other)
	}
	Set(p, root, "allowed_accounts", "")
	Set(p, root, "plan.file", "")
	s = Load(p, root)
	if len(s.AllowedAccounts) != 0 || s.PlanFile != "" || s.PlanCommand != "echo 1 2" {
		t.Errorf("after clear: %+v", s)
	}
	if err := Set(p, root, "nonsense", "x"); err == nil {
		t.Error("unknown key accepted")
	}
}

func TestAllowed(t *testing.T) {
	me := store.Identity{Email: "Me@Acme.com", OrgName: "Acme", OrgID: "org1", AccountID: "a1", Known: true}
	lookup := func(name string) (store.Identity, bool) {
		switch name {
		case "work":
			return store.Identity{OrgID: "org1", Known: true}, true
		case "other":
			return store.Identity{OrgID: "org2", Known: true}, true
		}
		return store.Identity{}, false
	}
	check := func(list []string, id store.Identity, want bool) string {
		t.Helper()
		ok, reason := Allowed(Settings{AllowedAccounts: list}, "/r", "prof", id, lookup)
		if ok != want {
			t.Errorf("Allowed(%v) = %v (%s), want %v", list, ok, reason, want)
		}
		return reason
	}
	check(nil, store.Identity{}, true)
	check([]string{"me@acme.com"}, me, true)
	check([]string{"acme"}, me, true)
	check([]string{"work"}, me, true)
	check([]string{"other"}, me, false)
	if r := check([]string{"someone@else.com"}, me, false); r != "Me@Acme.com is not allowed for /r" {
		t.Errorf("reason = %q", r)
	}
	if r := check([]string{"work"}, store.Identity{}, false); r != "account identity unknown; this project restricts accounts" {
		t.Errorf("reason = %q", r)
	}
}

func TestCountCheckboxes(t *testing.T) {
	in := "# Plan\n- [ ] one\n- [x] two\n  * [X] three\n\t- [ ] four\n```\n- [x] in code\n```\ntext [x] not an item\n-[x] no space\n"
	done, total := CountCheckboxes(strings.NewReader(in))
	if done != 3 || total != 5 {
		t.Errorf("got %d/%d, want 3/5", done, total)
	}
}

func write(t *testing.T, path, body string, mod time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if !mod.IsZero() {
		os.Chtimes(path, mod, mod)
	}
}

func TestProgressOrder(t *testing.T) {
	p := testPaths(t)
	root := t.TempDir()
	now := time.Now()

	if pl := Progress(p, root, Settings{}, now); pl.Known {
		t.Errorf("empty project has a plan: %+v", pl)
	}
	write(t, filepath.Join(root, "TODO.md"), "- [x] a\n- [ ] b\n- [ ] c\n", time.Time{})
	if pl := Progress(p, root, Settings{}, now); !pl.Known || pl.Done != 1 || pl.Total != 3 || pl.Label != "1/3" {
		t.Errorf("TODO.md: %+v", pl)
	}
	write(t, filepath.Join(root, "PLAN.md"), "no boxes here\n", time.Time{})
	if pl := Progress(p, root, Settings{}, now); pl.Total != 3 {
		t.Errorf("PLAN.md without boxes should be skipped: %+v", pl)
	}
	write(t, filepath.Join(root, "PLAN.md"), "- [x] a\n- [x] b\n", time.Time{})
	if pl := Progress(p, root, Settings{}, now); pl.Done != 2 || pl.Total != 2 {
		t.Errorf("PLAN.md beats TODO.md: %+v", pl)
	}
	plans := filepath.Join(root, "docs", "superpowers", "plans")
	write(t, filepath.Join(plans, "old.md"), "- [ ] a\n", now.Add(-time.Hour))
	write(t, filepath.Join(plans, "new.md"), "- [x] a\n- [ ] b\n- [ ] c\n- [ ] d\n", now)
	if pl := Progress(p, root, Settings{}, now); pl.Done != 1 || pl.Total != 4 {
		t.Errorf("newest plan file: %+v", pl)
	}
	write(t, filepath.Join(root, "my plan.md"), "- [x] a\n- [x] b\n- [x] c\n- [ ] d\n- [ ] e\n", time.Time{})
	if pl := Progress(p, root, Settings{PlanFile: "my plan.md"}, now); pl.Done != 3 || pl.Total != 5 {
		t.Errorf("plan.file: %+v", pl)
	}
	s := Settings{PlanFile: "my plan.md", PlanCommand: "echo 10 14 week"}
	if pl := Progress(p, root, s, now); pl.Done != 10 || pl.Total != 14 || pl.Label != "week 10/14" {
		t.Errorf("plan.command: %+v", pl)
	}
}

func TestProgressCommandRunsInRoot(t *testing.T) {
	p := testPaths(t)
	root := filepath.Join(t.TempDir(), "my proj")
	write(t, filepath.Join(root, "n"), "7\n", time.Time{})
	pl := Progress(p, root, Settings{PlanCommand: "echo $(cat n) 9"}, time.Now())
	if !pl.Known || pl.Done != 7 || pl.Total != 9 || pl.Label != "7/9" {
		t.Errorf("got %+v", pl)
	}
}

func TestProgressCommandCacheAndTimeout(t *testing.T) {
	p := testPaths(t)
	root := t.TempDir()
	s := Settings{PlanCommand: "echo x >> runs; echo 1 2"}
	t0 := time.Now()
	Progress(p, root, s, t0)
	Progress(p, root, s, t0.Add(10*time.Second))
	if b, _ := os.ReadFile(filepath.Join(root, "runs")); strings.Count(string(b), "x") != 1 {
		t.Errorf("ran %d times within 60s, want 1", strings.Count(string(b), "x"))
	}
	Progress(p, root, s, t0.Add(61*time.Second))
	if b, _ := os.ReadFile(filepath.Join(root, "runs")); strings.Count(string(b), "x") != 2 {
		t.Errorf("ran %d times after 61s, want 2", strings.Count(string(b), "x"))
	}

	start := time.Now()
	pl := Progress(p, t.TempDir(), Settings{PlanCommand: "sleep 10; echo 1 2"}, time.Now())
	if pl.Known {
		t.Errorf("timed-out command gave a plan: %+v", pl)
	}
	if d := time.Since(start); d > 6*time.Second {
		t.Errorf("took %v, want under 6s", d)
	}

	for _, bad := range []string{"echo hello", "echo 3", "echo 5 2", "exit 1", "echo -1 4"} {
		if pl := Progress(p, t.TempDir(), Settings{PlanCommand: bad}, time.Now()); pl.Known {
			t.Errorf("%q gave a plan: %+v", bad, pl)
		}
	}
}
