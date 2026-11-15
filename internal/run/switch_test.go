package run

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-sessions/internal/project"
	"claude-sessions/internal/store"
)

func (f *fixture) waitLaunches(n int, within time.Duration) []string {
	f.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if l := f.launches(); len(l) >= n {
			return l
		}
		time.Sleep(20 * time.Millisecond)
	}
	return f.launches()
}

func TestSwitchIdleSession(t *testing.T) {
	f := newFixture(t)
	s, stop := f.startAsync(func() int { return f.r.New(Opts{Account: "default", Name: "demo"}) })

	msg, err := Switch(f.p, "demo", "work", false)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "switching now" {
		t.Errorf("message = %q", msg)
	}
	l := f.waitLaunches(2, 8*time.Second)
	if len(l) != 2 {
		t.Fatalf("launches = %v (stderr %s)", l, f.err.String())
	}
	if !strings.Contains(field(l[1], "ARGS"), "--resume "+s.ID) || field(l[1], "CONFIG") != f.p.AccountDir("work") || field(l[1], "PROFILE") != "work" {
		t.Errorf("relaunch = %s", l[1])
	}
	got := f.waitState(store.StateIdle)
	if got.ID != s.ID || got.Profile != "work" || got.Identity.Email != "w@other.org" || got.SwitchTo != "" {
		t.Errorf("after switch: %+v", got)
	}
	if !store.IsLocked(f.p, s.ID) {
		t.Error("lock dropped during the switch")
	}
	if !strings.Contains(f.err.String(), "now on w@other.org") {
		t.Errorf("no confirmation printed: %q", f.err.String())
	}
	if code := stop(); code != 0 {
		t.Errorf("exit = %d", code)
	}
}

func TestSwitchWaitsForIdle(t *testing.T) {
	f := newFixture(t)
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	store.UpdateSession(f.p, s.ID, func(x *store.Session) { x.State = store.StateWorking })

	msg, err := Switch(f.p, s.ID, "work", false)
	if err != nil {
		t.Fatal(err)
	}
	if msg != "will switch when the current turn finishes" {
		t.Errorf("message = %q", msg)
	}
	if l := f.waitLaunches(2, 2*time.Second); len(l) != 1 {
		t.Fatalf("switched in the middle of a turn: %v", l)
	}
	// The turn ends: the Stop hook reports, which also wakes the wrapper.
	store.UpdateSession(f.p, s.ID, func(x *store.Session) { x.State = store.StateIdle })
	if l := f.waitLaunches(2, 8*time.Second); len(l) != 2 || field(l[1], "PROFILE") != "work" {
		t.Fatalf("did not switch after the turn ended: %v", l)
	}
}

func TestSwitchForce(t *testing.T) {
	f := newFixture(t)
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	store.UpdateSession(f.p, s.ID, func(x *store.Session) { x.State = store.StateWorking })
	msg, err := Switch(f.p, s.ID[:8], "work", true)
	if err != nil || msg != "switching now" {
		t.Fatalf("%q %v", msg, err)
	}
	if l := f.waitLaunches(2, 8*time.Second); len(l) != 2 {
		t.Fatalf("forced switch did not happen: %v", l)
	}
}

func TestSwitchToSignedOutAccount(t *testing.T) {
	f := newFixture(t)
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	os.WriteFile(filepath.Join(f.p.AccountDir("work"), "loggedout"), nil, 0o600)

	_, err := Switch(f.p, s.ID, "work", false)
	if err == nil || !strings.Contains(err.Error(), "work") || !strings.Contains(err.Error(), "signed in") {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if l := f.launches(); len(l) != 1 {
		t.Errorf("session was relaunched: %v", l)
	}
	if got := f.onlySession(); got.Profile != "default" || got.SwitchTo != "" || got.State != store.StateIdle {
		t.Errorf("session disturbed: %+v", got)
	}
}

func TestSwitchRefusals(t *testing.T) {
	f := newFixture(t)
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })

	if _, err := Switch(f.p, s.ID, "nope", false); err == nil || !strings.Contains(err.Error(), `"nope"`) {
		t.Errorf("unknown account: %v", err)
	}
	if _, err := Switch(f.p, s.ID, "default", false); err == nil || !strings.Contains(err.Error(), "already on") {
		t.Errorf("same account: %v", err)
	}
	project.Set(f.p, f.r.Cwd, "allowed_accounts", "d@acme.com")
	if _, err := Switch(f.p, s.ID, "work", false); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("disallowed account: %v", err)
	}
	if _, err := Switch(f.p, "no-such-session", "work", false); err == nil {
		t.Error("unknown session accepted")
	}
	if got := f.onlySession(); got.SwitchTo != "" {
		t.Errorf("a refused switch left a request: %+v", got)
	}
}

func TestSwitchFallsBack(t *testing.T) {
	f := newFixture(t)
	s, stop := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	// "work" passes the sign-in check but claude fails to start under it.
	os.WriteFile(filepath.Join(f.p.AccountDir("work"), "failstart"), nil, 0o600)

	if _, err := Switch(f.p, s.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	l := f.waitLaunches(3, 10*time.Second)
	if len(l) != 3 {
		t.Fatalf("launches = %v (stderr %s)", l, f.err.String())
	}
	if field(l[1], "PROFILE") != "work" || field(l[2], "PROFILE") != "default" || !strings.Contains(field(l[2], "ARGS"), "--resume "+s.ID) {
		t.Errorf("fallback sequence wrong: %v", l[1:])
	}
	got := f.waitState(store.StateIdle)
	if got.Profile != "default" || got.Identity.Email != "d@acme.com" || !store.IsLocked(f.p, s.ID) {
		t.Errorf("after fallback: %+v", got)
	}
	if !strings.Contains(f.err.String(), "back on") {
		t.Errorf("no explanation printed: %q", f.err.String())
	}
	if code := stop(); code != 0 {
		t.Errorf("exit = %d", code)
	}
}

func TestSwitchUnmanaged(t *testing.T) {
	f := newFixture(t)
	f.transcriptFor(oldID, time.Hour)
	_, err := Switch(f.p, oldID, "work", false)
	if err == nil || !strings.Contains(err.Error(), "not running under cs") || !strings.Contains(err.Error(), "cs resume") {
		t.Errorf("err = %v", err)
	}
}

func TestTerminalClosedRightAfterSwitchDoesNotRelaunch(t *testing.T) {
	// claude exits non-zero when its terminal goes away; that must not be
	// mistaken for the new account failing to start.
	f := newFixture(t, "FAKE_TERM_EXIT=143")
	s, stop := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	if _, err := Switch(f.p, s.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	if l := f.waitLaunches(2, 8*time.Second); len(l) != 2 {
		t.Fatalf("launches = %v", l)
	}
	f.waitState(store.StateIdle)
	if code := stop(); code == -1 {
		t.Fatal("the session kept running after its terminal was closed")
	}
	time.Sleep(300 * time.Millisecond)
	if l := f.launches(); len(l) != 2 {
		t.Errorf("claude was relaunched after the terminal closed: %v", l)
	}
	if got := f.onlySession(); got.State != store.StateEnded || got.Profile != "work" {
		t.Errorf("session = %+v", got)
	}
}

func TestSwitchRecordsWhichAccountWhen(t *testing.T) {
	f := newFixture(t)
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	if len(s.Spans) != 1 || s.Spans[0].Key != s.AccountKey || s.AccountKey == "" {
		t.Fatalf("at launch: key %q spans %+v", s.AccountKey, s.Spans)
	}
	first := s.Spans[0]
	if _, err := Switch(f.p, s.ID, "work", false); err != nil {
		t.Fatal(err)
	}
	f.waitLaunches(2, 8*time.Second)
	got := f.waitState(store.StateIdle)
	if len(got.Spans) != 2 || got.Spans[0] != first || got.Spans[1].Key != got.AccountKey || got.Spans[1].Key == first.Key || got.Spans[1].From.Before(first.From) {
		t.Errorf("after switch: key %q spans %+v", got.AccountKey, got.Spans)
	}
}
