package paths

import (
	"path/filepath"
	"testing"
)

func TestResolveHonoursEnv(t *testing.T) {
	t.Setenv("CS_HOME", "/tmp/x/cs")
	t.Setenv("CS_CLAUDE_DIR", "/tmp/x/claude")
	t.Setenv("CS_CLAUDE_BIN", "/tmp/x/fake")
	p := Resolve()
	if p.Home != "/tmp/x/cs" || p.ClaudeDir != "/tmp/x/claude" || p.ClaudeBin != "/tmp/x/fake" {
		t.Fatalf("env not honoured: %+v", p)
	}
	if got := p.AccountDir("default"); got != "/tmp/x/claude" {
		t.Errorf("default account dir = %q", got)
	}
	if got := p.AccountDir("work"); got != filepath.Join("/tmp/x/cs", "accounts", "work") {
		t.Errorf("work account dir = %q", got)
	}
	if got := p.SessionFile("abc"); got != "/tmp/x/cs/state/sessions/abc.json" {
		t.Errorf("session file = %q", got)
	}
}

func TestResolveDefaults(t *testing.T) {
	t.Setenv("CS_HOME", "")
	t.Setenv("CS_CLAUDE_DIR", "")
	t.Setenv("CS_CLAUDE_BIN", "")
	t.Setenv("HOME", "/tmp/h")
	p := Resolve()
	if p.Home != "/tmp/h/.claude-sessions" || p.ClaudeDir != "/tmp/h/.claude" || p.ClaudeBin != "claude" {
		t.Fatalf("defaults wrong: %+v", p)
	}
}
