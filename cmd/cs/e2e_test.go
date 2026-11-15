package main_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type status struct {
	Sessions []struct {
		ID, Project, Session, State, Account, Profile, Plan string
		Running                                             bool
	}
	Accounts []struct{ Name, Label string }
}

// TestEndToEnd drives the built binary the way a user would: add an
// account, start a session, switch its account, read the dashboard.
func TestEndToEnd(t *testing.T) {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(d, "cs")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	fake, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "fakeclaude"))
	home := filepath.Join(d, "home")
	claudeDir := filepath.Join(home, ".claude")
	proj := filepath.Join(d, "my proj")
	os.MkdirAll(claudeDir, 0o700)
	os.MkdirAll(proj, 0o755)
	os.WriteFile(filepath.Join(proj, "PLAN.md"), []byte("- [x] one\n- [ ] two\n"), 0o644)
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home,
		"CS_HOME=" + filepath.Join(d, "cs home"), "CS_CLAUDE_DIR=" + claudeDir, "CS_CLAUDE_BIN=" + fake,
		"FAKE_LOG=" + filepath.Join(d, "fake.log"),
	}
	cs := func(args ...string) (string, error) {
		cmd := exec.Command(bin, args...)
		cmd.Dir, cmd.Env = proj, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	read := func() status {
		t.Helper()
		out, err := cs("status", "--json")
		if err != nil {
			t.Fatalf("status: %v\n%s", err, out)
		}
		var s status
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatalf("status JSON: %v\n%s", err, out)
		}
		return s
	}
	waitFor := func(what string, ok func(status) bool) status {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for {
			s := read()
			if ok(s) {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s: %+v", what, s)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	if out, err := cs("version"); err != nil || !strings.HasPrefix(out, "cs ") {
		t.Fatalf("version: %q %v", out, err)
	}
	for _, h := range []string{"--help", "-h", "help"} {
		if out, err := cs(h); err != nil || !strings.Contains(out, "usage: cs <command>") {
			t.Errorf("cs %s: %v %q", h, err, out)
		}
	}
	if out, err := cs("--once", "--no-refresh"); err != nil || !strings.Contains(out, "ACCOUNTS") {
		t.Errorf("`cs --once` should be the dashboard: %v %q", err, out)
	}
	if out, err := cs("account", "add", "work"); err != nil {
		t.Fatalf("account add: %v\n%s", err, out)
	}
	if out, err := cs("account", "list"); err != nil || !strings.Contains(out, "default") || !strings.Contains(out, "work") || !strings.Contains(out, "signed in") {
		t.Fatalf("account list: %v\n%s", err, out)
	}
	if out, err := cs("account", "add", "Bad Name"); err == nil {
		t.Errorf("bad account name accepted: %s", out)
	}

	// Start a session; it stays up until we stop it.
	sess := exec.Command(bin, "new", "-a", "default", "-n", "demo")
	sess.Dir, sess.Env = proj, env
	var sessOut bytes.Buffer
	sess.Stdout, sess.Stderr = &sessOut, &sessOut
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- sess.Wait() }()
	defer sess.Process.Kill()

	s := waitFor("an idle session", func(s status) bool { return len(s.Sessions) == 1 && s.Sessions[0].State == "idle" })
	row := s.Sessions[0]
	if row.Session != "demo" || row.Profile != "default" || !row.Running || row.Project != "my proj" || row.Plan != "1/2" {
		t.Errorf("dashboard row = %+v", row)
	}
	id := row.ID

	// A second wrapper on the same session must be refused.
	if out, err := cs("resume", id, "-a", "work"); err == nil || !strings.Contains(out, "already running") {
		t.Errorf("second wrapper: %v %q", err, out)
	}

	if out, err := cs("switch", "demo", "work"); err != nil || !strings.Contains(out, "switching now") {
		t.Fatalf("switch: %v\n%s", err, out)
	}
	s = waitFor("the session on work", func(s status) bool {
		return len(s.Sessions) == 1 && s.Sessions[0].Profile == "work" && s.Sessions[0].State == "idle"
	})
	if s.Sessions[0].ID != id || !s.Sessions[0].Running {
		t.Errorf("after switch = %+v", s.Sessions[0])
	}
	log, _ := os.ReadFile(filepath.Join(d, "fake.log"))
	if !strings.Contains(string(log), "--resume "+id) {
		t.Errorf("claude was not resumed with the same session id:\n%s", log)
	}

	if out, err := cs("status", "--once"); err != nil || !strings.Contains(out, "demo") || !strings.Contains(out, "ACCOUNTS") || strings.Contains(out, "\x1b") {
		t.Errorf("status --once: %v\n%q", err, out)
	}
	if out, err := cs("project", "set", "plan.command", "echo 3 4 week"); err != nil {
		t.Fatalf("project set: %v %s", err, out)
	}
	if s = read(); s.Sessions[0].Plan != "week 3/4" {
		t.Errorf("plan.command not used: %+v", s.Sessions[0])
	}

	// Closing the terminal: the wrapper passes it on and records the end.
	sess.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("session did not exit; output:\n%s", sessOut.String())
	}
	s = read()
	if len(s.Sessions) != 1 || s.Sessions[0].State != "ended" || s.Sessions[0].Running {
		t.Errorf("after exit = %+v", s.Sessions)
	}

	if out, err := cs("account", "remove", "work"); err != nil {
		t.Fatalf("account remove: %v %s", err, out)
	}
	if out, _ := cs("account", "list"); strings.Contains(out, "work") {
		t.Errorf("account still listed: %s", out)
	}
	if out, err := cs("nonsense"); err == nil {
		t.Errorf("unknown command succeeded: %s", out)
	}
}
