package run

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/project"
	"claude-sessions/internal/store"
)

// The fixture's "default" is in org-acme and "work" in org-other. teammate
// adds a second account in default's organization.
func (f *fixture) teammate() {
	f.t.Helper()
	a, err := accounts.Create(f.p, "team")
	if err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"oauthAccount": map[string]string{"emailAddress": "t@acme.com", "accountUuid": "acc-team", "organizationUuid": "org-acme"}})
	os.WriteFile(filepath.Join(a.ConfigDir, ".claude.json"), b, 0o600)
}

func (f *fixture) headless() []string {
	b, _ := os.ReadFile(f.log)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "HEADLESS|") {
			out = append(out, l)
		}
	}
	return out
}

func (f *fixture) logLines() []string {
	b, _ := os.ReadFile(f.log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestSwitchToAnotherOrganizationNeedsConsent(t *testing.T) {
	f := newFixture(t)
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })

	_, err := Switch(f.p, s.ID, "work", SwitchOpts{})
	var cross *CrossOrgError
	if !errors.As(err, &cross) {
		t.Fatalf("err = %v, want a CrossOrgError", err)
	}
	if cross.From != "d@acme.com" || cross.To != "w@other.org" {
		t.Errorf("error names %q -> %q", cross.From, cross.To)
	}
	for _, want := range []string{"thinking", "--compact"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message should mention %q: %s", want, err)
		}
	}
	time.Sleep(1200 * time.Millisecond)
	if got := f.onlySession(); got.SwitchTo != "" || got.Profile != "default" || len(f.launches()) != 1 {
		t.Errorf("a refused switch disturbed the session: %+v", got)
	}

	if msg, err := Switch(f.p, s.ID, "work", SwitchOpts{AllowCrossOrg: true}); err != nil || msg != "switching now" {
		t.Fatalf("with consent: %q %v", msg, err)
	}
	if l := f.waitLaunches(2, 8*time.Second); len(l) != 2 {
		t.Fatalf("did not switch: %v", l)
	}
}

func TestSwitchWithinAnOrganizationIsFree(t *testing.T) {
	f := newFixture(t)
	f.teammate()
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	if msg, err := Switch(f.p, s.ID, "team", SwitchOpts{}); err != nil || msg != "switching now" {
		t.Fatalf("same organization: %q %v", msg, err)
	}
	if l := f.waitLaunches(2, 8*time.Second); len(l) != 2 || field(l[1], "PROFILE") != "team" {
		t.Fatalf("launches = %v", l)
	}
	if len(f.headless()) != 0 {
		t.Error("compacted although the organization did not change")
	}
}

func TestSwitchUnknownOrganizationIsNotBlocked(t *testing.T) {
	f := newFixture(t)
	os.Remove(filepath.Join(f.p.AccountDir("work"), ".claude.json")) // cs cannot tell who "work" is
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	if _, err := Switch(f.p, s.ID, "work", SwitchOpts{}); err != nil {
		t.Fatalf("unknown organization should not block: %v", err)
	}
}

func TestSwitchCompactSummarisesUnderTheOldAccountFirst(t *testing.T) {
	f := newFixture(t)
	s, _ := f.startAsync(func() int {
		return f.r.New(Opts{Account: "default", ClaudeArgs: []string{"--model", "opus"}})
	})
	// --compact is the user's answer to the organization question.
	if _, err := Switch(f.p, s.ID, "work", SwitchOpts{Compact: true}); err != nil {
		t.Fatal(err)
	}
	if l := f.waitLaunches(2, 10*time.Second); len(l) != 2 {
		t.Fatalf("launches = %v (stderr %s)", l, f.err.String())
	}
	h := f.headless()
	if len(h) != 1 {
		t.Fatalf("headless runs = %v", h)
	}
	if field(h[0], "CONFIG") != f.p.ClaudeDir || field(h[0], "STDIN") != "/compact" {
		t.Errorf("compact ran as %s", h[0])
	}
	if argv := field(h[0], "ARGV"); !strings.HasPrefix(argv, "-p --resume "+s.ID) || !strings.Contains(argv, "--model opus") || strings.Contains(argv, "--settings") {
		t.Errorf("compact argv = %q", argv)
	}
	// Order: first launch, then the compact, then the relaunch on work.
	var order []string
	for _, l := range f.logLines() {
		switch {
		case strings.HasPrefix(l, "ARGS="):
			order = append(order, "launch:"+field(l, "PROFILE"))
		case strings.HasPrefix(l, "HEADLESS|"):
			order = append(order, "compact")
		}
	}
	if strings.Join(order, " ") != "launch:default compact launch:work" {
		t.Errorf("order = %v", order)
	}
	got := f.waitState(store.StateIdle)
	if got.Profile != "work" || got.SwitchCompact {
		t.Errorf("after switch: %+v", got)
	}
	if !strings.Contains(f.err.String(), "compact") {
		t.Errorf("nothing told the user a compact was running: %q", f.err.String())
	}
}

func TestSwitchCompactFailureStillSwitches(t *testing.T) {
	f := newFixture(t, "FAKE_COMPACT_EXIT=1")
	s, _ := f.startAsync(func() int { return f.r.New(Opts{Account: "default"}) })
	if _, err := Switch(f.p, s.ID, "work", SwitchOpts{Compact: true}); err != nil {
		t.Fatal(err)
	}
	if l := f.waitLaunches(2, 10*time.Second); len(l) != 2 || field(l[1], "PROFILE") != "work" {
		t.Fatalf("the session was left stopped after a failed compact: %v", l)
	}
	if !strings.Contains(f.err.String(), "could not compact") {
		t.Errorf("stderr = %q", f.err.String())
	}
}

func TestResumeAcrossOrganizations(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	if code := f.r.New(Opts{Account: "default"}); code != 0 {
		t.Fatal(f.err.String())
	}
	s := f.onlySession()
	f.transcriptFor(s.ID, time.Hour)

	// Without --compact: allowed, with a plain warning.
	f.err.b.Reset()
	if code := f.r.Resume(Opts{SessionRef: s.ID, Account: "work"}); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	if e := f.err.String(); !strings.Contains(e, "different organization") || !strings.Contains(e, "--compact") {
		t.Errorf("no warning about the organization change: %q", e)
	}
	if len(f.headless()) != 0 {
		t.Error("compacted without being asked")
	}

	// Back to default with --compact: summarise under work first.
	f.transcriptFor(s.ID, time.Hour)
	if code := f.r.Resume(Opts{SessionRef: s.ID, Account: "default", Compact: true}); code != 0 {
		t.Fatalf("exit %d: %s", code, f.err.String())
	}
	h := f.headless()
	if len(h) != 1 || field(h[0], "CONFIG") != f.p.AccountDir("work") || field(h[0], "STDIN") != "/compact" {
		t.Fatalf("headless = %v", h)
	}
	if l := f.launches(); len(l) != 3 || field(l[2], "PROFILE") != "default" {
		t.Errorf("launches = %v", l)
	}
}

func TestResumeSameAccountNeverCompactsOrWarns(t *testing.T) {
	f := newFixture(t, "FAKE_EXIT_NOW=1")
	f.r.New(Opts{Account: "default"})
	s := f.onlySession()
	f.transcriptFor(s.ID, time.Hour)
	f.err.b.Reset()
	if code := f.r.Resume(Opts{SessionRef: s.ID, Compact: true}); code != 0 {
		t.Fatal(f.err.String())
	}
	if len(f.headless()) != 0 || strings.Contains(f.err.String(), "organization") {
		t.Errorf("headless %v stderr %q", f.headless(), f.err.String())
	}
}

func TestPickerSaysWhichAccountsShareTheOrganization(t *testing.T) {
	f := newFixture(t)
	f.teammate()
	var got []Choice
	ChooseAccount(f.p, f.r.Cwd, Opts{}, "", project.Settings{}, func(c []Choice) string { got = c; return c[0].Name })
	for _, c := range got {
		if c.Note != "" {
			t.Errorf("a new session has no organization to compare with: %+v", c)
		}
	}
	// Resuming a session that last ran on default (org-acme).
	prevID := accounts.ResolveIdentity(f.p.ClaudeDir)
	notes := map[string]string{}
	for _, c := range OrgNotes(f.p, got, prevID) {
		notes[c.Name] = c.Note
	}
	want := map[string]string{"default": "same organization", "team": "same organization", "work": "other organization: earlier thinking is dropped"}
	for n, w := range want {
		if notes[n] != w {
			t.Errorf("%s note = %q, want %q", n, notes[n], w)
		}
	}
}
