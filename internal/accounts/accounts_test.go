package accounts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claude-sessions/internal/paths"
)

func testPaths(t *testing.T) paths.Paths {
	t.Helper()
	d := t.TempDir()
	p := paths.Paths{Home: filepath.Join(d, "cs"), ClaudeDir: filepath.Join(d, "home", ".claude"), ClaudeBin: "claude"}
	if err := os.MkdirAll(p.ClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func isLinkTo(t *testing.T, link, target string) bool {
	t.Helper()
	got, err := os.Readlink(link)
	return err == nil && got == target
}

func TestCreateLinksSharedStore(t *testing.T) {
	p := testPaths(t)
	os.WriteFile(filepath.Join(p.ClaudeDir, "settings.json"), []byte("{}"), 0o600)
	a, err := Create(p, "work")
	if err != nil {
		t.Fatal(err)
	}
	if a.ConfigDir != p.AccountDir("work") {
		t.Errorf("config dir = %q", a.ConfigDir)
	}
	for _, n := range []string{"projects", "tasks", "settings.json"} {
		if !isLinkTo(t, filepath.Join(a.ConfigDir, n), filepath.Join(p.ClaudeDir, n)) {
			t.Errorf("%s is not a link to the shared store", n)
		}
	}
	if _, err := os.Lstat(filepath.Join(a.ConfigDir, "skills")); !os.IsNotExist(err) {
		t.Errorf("skills should not exist: %v", err)
	}
	if st, err := os.Stat(filepath.Join(p.ClaudeDir, "projects")); err != nil || !st.IsDir() {
		t.Errorf("shared projects dir not created: %v", err)
	}
	names := []string{}
	for _, x := range List(p) {
		names = append(names, x.Name)
	}
	if strings.Join(names, ",") != "default,work" {
		t.Errorf("list = %v", names)
	}
	if d, ok := Get(p, "default"); !ok || d.ConfigDir != p.ClaudeDir {
		t.Errorf("default = %+v %v", d, ok)
	}
}

func TestEnsureLinksAddsLaterFiles(t *testing.T) {
	p := testPaths(t)
	a, err := Create(p, "work")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.ClaudeDir, "CLAUDE.md"), []byte("x"), 0o600)
	if err := EnsureLinks(p, a); err != nil {
		t.Fatal(err)
	}
	if !isLinkTo(t, filepath.Join(a.ConfigDir, "CLAUDE.md"), filepath.Join(p.ClaudeDir, "CLAUDE.md")) {
		t.Error("CLAUDE.md link missing")
	}
}

func TestEnsureLinksRefusesRealFile(t *testing.T) {
	p := testPaths(t)
	a, err := Create(p, "work")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(p.ClaudeDir, "settings.json"), []byte("{}"), 0o600)
	real := filepath.Join(a.ConfigDir, "settings.json")
	os.WriteFile(real, []byte("mine"), 0o600)
	err = EnsureLinks(p, a)
	if err == nil || !strings.Contains(err.Error(), real) {
		t.Fatalf("err = %v, want one naming %s", err, real)
	}
	if b, _ := os.ReadFile(real); string(b) != "mine" {
		t.Error("real file was touched")
	}
}

func TestEnsureLinksDefaultIsNoOp(t *testing.T) {
	p := testPaths(t)
	d, _ := Get(p, "default")
	if err := EnsureLinks(p, d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(p.ClaudeDir, "projects")); err == nil {
		if _, lerr := os.Readlink(filepath.Join(p.ClaudeDir, "projects")); lerr == nil {
			t.Error("default got a self-link")
		}
	}
}

func TestNameRules(t *testing.T) {
	p := testPaths(t)
	for _, bad := range []string{"Work", "-x", "default", strings.Repeat("a", 33), "", "a b", "../x"} {
		if _, err := Create(p, bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	if _, err := Create(p, "work-2"); err != nil {
		t.Errorf("work-2 rejected: %v", err)
	}
	if _, err := Create(p, "work-2"); err == nil {
		t.Error("duplicate accepted")
	}
}

func TestDelete(t *testing.T) {
	p := testPaths(t)
	a, _ := Create(p, "work")
	os.WriteFile(filepath.Join(p.ClaudeDir, "projects", "keep"), []byte("x"), 0o600)
	if err := Delete(p, "default"); err == nil {
		t.Error("default deleted")
	}
	if err := Delete(p, "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.ConfigDir); !os.IsNotExist(err) {
		t.Error("profile dir still there")
	}
	if _, err := os.Stat(filepath.Join(p.ClaudeDir, "projects", "keep")); err != nil {
		t.Error("deleting a profile removed shared history")
	}
	if _, ok := Get(p, "work"); ok {
		t.Error("still registered")
	}
}

func TestResolveIdentity(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"a@b.c","accountUuid":"acc1","organizationUuid":"org1","organizationName":"Acme"}}`), 0o600)
	id := ResolveIdentity(d)
	if !id.Known || id.Email != "a@b.c" || id.AccountID != "acc1" || id.OrgID != "org1" || id.OrgName != "Acme" {
		t.Errorf("got %+v", id)
	}
	if ResolveIdentity(t.TempDir()).Known {
		t.Error("missing file gave a known identity")
	}
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, ".claude.json"), []byte(`{"oauthAccount":`), 0o600)
	if ResolveIdentity(bad).Known {
		t.Error("malformed file gave a known identity")
	}
	empty := t.TempDir()
	os.WriteFile(filepath.Join(empty, ".claude.json"), []byte(`{"userID":"x"}`), 0o600)
	if ResolveIdentity(empty).Known {
		t.Error("file without oauthAccount gave a known identity")
	}
}

func TestResolveIdentityDefaultUsesHomeFile(t *testing.T) {
	p := testPaths(t)
	// ~/.claude.json sits beside ~/.claude for the default account.
	os.WriteFile(filepath.Join(filepath.Dir(p.ClaudeDir), ".claude.json"), []byte(`{"oauthAccount":{"emailAddress":"d@b.c","accountUuid":"a","organizationUuid":"o"}}`), 0o600)
	if id := ResolveIdentity(p.ClaudeDir); !id.Known || id.Email != "d@b.c" {
		t.Errorf("got %+v", id)
	}
}

func TestLoggedInUsesConfigDir(t *testing.T) {
	p := testPaths(t)
	a, _ := Create(p, "work")
	fake := filepath.Join(t.TempDir(), "fake")
	os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$1 $2\" = \"auth status\" ] || exit 9\nif [ \"$CLAUDE_CONFIG_DIR\" = \""+a.ConfigDir+"\" ]; then echo '{\"loggedIn\": true}'; else echo '{\"loggedIn\": false}'; exit 1; fi\n"), 0o700)
	p.ClaudeBin = fake
	if !LoggedIn(p, a) {
		t.Error("work should be logged in")
	}
	d, _ := Get(p, "default")
	if LoggedIn(p, d) {
		t.Error("default should not be logged in")
	}
}
