package transcript

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claude-sessions/internal/store"
)

// store builds a claude dir holding copies of fixtures under given ids.
func claudeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for id, fixture := range files {
		b, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(d, "projects", "-work-my-proj")
		os.MkdirAll(dir, 0o700)
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func TestListReadsIDCwdAndName(t *testing.T) {
	d := claudeDir(t, map[string]string{"aaaa1111-0000-4000-8000-000000000001": "tasks-create-update.jsonl"})
	l := List(d)
	if len(l) != 1 {
		t.Fatalf("got %d sessions", len(l))
	}
	i := l[0]
	if i.ID != "aaaa1111-0000-4000-8000-000000000001" || i.Cwd != "/work/my proj" || i.Name != "demo-name" {
		t.Errorf("got %+v", i)
	}
	if i.Path == "" || i.ModTime.IsZero() {
		t.Errorf("path/modtime missing: %+v", i)
	}
	if len(List(t.TempDir())) != 0 {
		t.Error("empty store listed sessions")
	}
}

func TestFindByPrefixNameAndAmbiguity(t *testing.T) {
	d := claudeDir(t, map[string]string{
		"aaaa1111-0000-4000-8000-000000000001": "tasks-create-update.jsonl",
		"aaaa2222-0000-4000-8000-000000000002": "no-tasks.jsonl",
		"bbbb3333-0000-4000-8000-000000000003": "no-tasks.jsonl",
	})
	if i, err := Find(d, "aaaa1111-0000-4000-8000-000000000001"); err != nil || i.Name != "demo-name" {
		t.Errorf("exact id: %+v %v", i, err)
	}
	if i, err := Find(d, "bbbb"); err != nil || i.ID != "bbbb3333-0000-4000-8000-000000000003" {
		t.Errorf("prefix: %+v %v", i, err)
	}
	if i, err := Find(d, "demo-name"); err != nil || i.ID != "aaaa1111-0000-4000-8000-000000000001" {
		t.Errorf("name: %+v %v", i, err)
	}
	if _, err := Find(d, "aaaa"); !errors.Is(err, ErrAmbiguous) {
		t.Errorf("ambiguous prefix err = %v", err)
	}
	if _, err := Find(d, "zzzz"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing err = %v", err)
	}
	if _, err := Find(d, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty ref err = %v", err)
	}
}

func tasks(t *testing.T, fixture string) store.TaskProgress {
	t.Helper()
	return Tasks(filepath.Join("testdata", fixture))
}

func TestTasksCreateUpdate(t *testing.T) {
	// Captured from a real session: alpha completed, beta in progress, gamma pending.
	want := store.TaskProgress{Done: 1, Total: 3, Current: "beta", Known: true}
	if got := tasks(t, "tasks-create-update.jsonl"); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestTasksTodoWrite(t *testing.T) {
	want := store.TaskProgress{Done: 2, Total: 3, Current: "three", Known: true}
	if got := tasks(t, "todowrite.jsonl"); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestTasksDeleted(t *testing.T) {
	want := store.TaskProgress{Done: 1, Total: 1, Known: true}
	if got := tasks(t, "deleted.jsonl"); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestTasksUnknownID(t *testing.T) {
	if got := tasks(t, "unknown-id.jsonl"); got.Known {
		t.Errorf("got %+v, want unknown", got)
	}
}

func TestTasksNone(t *testing.T) {
	if got := tasks(t, "no-tasks.jsonl"); got.Known {
		t.Errorf("got %+v, want unknown", got)
	}
	if got := Tasks(filepath.Join(t.TempDir(), "missing.jsonl")); got.Known {
		t.Errorf("missing file: %+v", got)
	}
}

func TestTruncatedLastLine(t *testing.T) {
	if got, want := tasks(t, "truncated.jsonl"), tasks(t, "tasks-create-update.jsonl"); got != want || !got.Known {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestRecentlyWritten(t *testing.T) {
	f := filepath.Join(t.TempDir(), "x.jsonl")
	os.WriteFile(f, []byte("{}\n"), 0o600)
	now := time.Now()
	if !RecentlyWritten(f, now) {
		t.Error("fresh file not recent")
	}
	if RecentlyWritten(f, now.Add(61*time.Second)) {
		t.Error("61s-old file is recent")
	}
	if RecentlyWritten(f+".missing", now) {
		t.Error("missing file is recent")
	}
}
