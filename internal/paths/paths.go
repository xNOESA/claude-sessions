// Package paths is the single source of every filesystem location cs uses.
package paths

import (
	"os"
	"path/filepath"
)

// Paths holds the three roots everything else derives from.
type Paths struct {
	Home      string // cs's own directory
	ClaudeDir string // the shared store and the "default" account
	ClaudeBin string // the claude executable
}

// Resolve reads CS_HOME, CS_CLAUDE_DIR and CS_CLAUDE_BIN, falling back to
// ~/.claude-sessions, ~/.claude and "claude".
func Resolve() Paths {
	home, _ := os.UserHomeDir()
	p := Paths{
		Home:      os.Getenv("CS_HOME"),
		ClaudeDir: os.Getenv("CS_CLAUDE_DIR"),
		ClaudeBin: os.Getenv("CS_CLAUDE_BIN"),
	}
	if p.Home == "" {
		p.Home = filepath.Join(home, ".claude-sessions")
	}
	if p.ClaudeDir == "" {
		p.ClaudeDir = filepath.Join(home, ".claude")
	}
	if p.ClaudeBin == "" {
		p.ClaudeBin = "claude"
	}
	return p
}

// DefaultAccount is the reserved name of the account living in ClaudeDir.
const DefaultAccount = "default"

func (p Paths) AccountsFile() string { return filepath.Join(p.Home, "accounts.json") }
func (p Paths) ProjectsFile() string { return filepath.Join(p.Home, "projects.json") }
func (p Paths) LogFile() string      { return filepath.Join(p.Home, "cs.log") }
func (p Paths) StateDir() string     { return filepath.Join(p.Home, "state") }

func (p Paths) AccountDir(name string) string {
	if name == DefaultAccount {
		return p.ClaudeDir
	}
	return filepath.Join(p.Home, "accounts", name)
}

func (p Paths) SessionsDir() string { return filepath.Join(p.StateDir(), "sessions") }
func (p Paths) UsageDir() string    { return filepath.Join(p.StateDir(), "accounts") }
func (p Paths) PlansDir() string    { return filepath.Join(p.StateDir(), "plans") }

func (p Paths) SessionFile(id string) string { return filepath.Join(p.SessionsDir(), id+".json") }
func (p Paths) UsageFile(key string) string  { return filepath.Join(p.UsageDir(), key+".json") }
func (p Paths) LockFile(id string) string {
	return filepath.Join(p.StateDir(), "locks", id+".lock")
}
