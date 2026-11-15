package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"claude-sessions/internal/paths"
)

func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	d := t.TempDir()
	return paths.Paths{Home: filepath.Join(d, "cs home"), ClaudeDir: filepath.Join(d, "claude"), ClaudeBin: "claude"}
}

func TestWriteJSONAtomicAndPrivate(t *testing.T) {
	p := testPaths(t)
	f := p.SessionFile("s1")
	if err := WriteJSON(f, map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(f)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", st.Mode().Perm())
	}
	ds, _ := os.Stat(filepath.Dir(f))
	if ds.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", ds.Mode().Perm())
	}
	ents, _ := os.ReadDir(filepath.Dir(f))
	if len(ents) != 1 {
		t.Errorf("leftover files: %v", ents)
	}
	var got map[string]int
	if err := ReadJSON(f, &got); err != nil || got["a"] != 1 {
		t.Errorf("round trip: %v %v", got, err)
	}
	if err := ReadJSON(f+".missing", &got); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file error = %v", err)
	}
}

func TestUpdateSessionConcurrent(t *testing.T) {
	p := testPaths(t)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := UpdateSession(p, "s1", func(s *Session) { s.Tasks.Total++ }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ss := LoadSessions(p)
	if len(ss) != 1 || ss[0].Tasks.Total != 50 || ss[0].ID != "s1" {
		t.Fatalf("got %+v", ss)
	}
}

func TestLoadSessionsSkipsCorrupt(t *testing.T) {
	p := testPaths(t)
	if err := UpdateSession(p, "good", func(s *Session) { s.Name = "ok" }); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.SessionFile("bad"), []byte(`{"ID":`), 0o600); err != nil {
		t.Fatal(err)
	}
	ss := LoadSessions(p)
	if len(ss) != 1 || ss[0].ID != "good" {
		t.Fatalf("got %+v", ss)
	}
}

func TestLock(t *testing.T) {
	p := testPaths(t)
	if IsLocked(p, "s1") {
		t.Fatal("locked before acquire")
	}
	rel, err := AcquireLock(p, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !IsLocked(p, "s1") {
		t.Error("not locked after acquire")
	}
	if _, err := AcquireLock(p, "s1"); !errors.Is(err, ErrLocked) {
		t.Errorf("second acquire err = %v", err)
	}
	rel()
	if IsLocked(p, "s1") {
		t.Error("locked after release")
	}
	rel2, err := AcquireLock(p, "s1")
	if err != nil {
		t.Fatal(err)
	}
	rel2()
}

func TestIdentityKeyAndLabel(t *testing.T) {
	k := Identity{Email: "a@b.c", OrgID: "org1", AccountID: "acc1", Plan: "Max", Known: true}
	if k.Key("work") != "org1_acc1" || k.Label("work") != "a@b.c · Max" {
		t.Errorf("known: %q %q", k.Key("work"), k.Label("work"))
	}
	k.Plan = ""
	if k.Label("work") != "a@b.c" {
		t.Errorf("no plan label = %q", k.Label("work"))
	}
	var u Identity
	if u.Key("work") != "profile_work" || u.Label("work") != "work ?" {
		t.Errorf("unknown: %q %q", u.Key("work"), u.Label("work"))
	}
}

func TestUsageRoundTrip(t *testing.T) {
	p := testPaths(t)
	u := Usage{FiveHour: &Window{UsedPct: 62}, Identity: Identity{OrgID: "o", AccountID: "a", Known: true}, Profile: "work"}
	if err := SaveUsage(p, u); err != nil {
		t.Fatal(err)
	}
	us := LoadUsage(p)
	if len(us) != 1 || us[0].FiveHour.UsedPct != 62 || us[0].SevenDay != nil {
		t.Fatalf("got %+v", us)
	}
}

func TestNoteAccountRecordsChangesOnly(t *testing.T) {
	var s Session
	at := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	s.NoteAccount("a", at)
	s.NoteAccount("a", at.Add(time.Minute)) // same account again: no new span
	s.NoteAccount("b", at.Add(time.Hour))
	s.NoteAccount("", at.Add(2*time.Hour)) // unknown: ignored
	if len(s.Spans) != 2 || s.Spans[0].Key != "a" || !s.Spans[0].From.Equal(at) || s.Spans[1].Key != "b" {
		t.Fatalf("spans = %+v", s.Spans)
	}
	for _, c := range []struct {
		at   time.Time
		want string
	}{{at.Add(-time.Hour), ""}, {at, "a"}, {at.Add(30 * time.Minute), "a"}, {at.Add(time.Hour), "b"}, {at.Add(5 * time.Hour), "b"}} {
		if got := s.AccountAt(c.at); got != c.want {
			t.Errorf("AccountAt(%v) = %q, want %q", c.at.Sub(at), got, c.want)
		}
	}
}
