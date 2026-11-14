// Package accounts manages one Claude Code config directory per account,
// each linked to the shared store in ~/.claude. It never touches a login:
// Claude Code keeps credentials itself.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/store"
)

// Account is one registered profile.
type Account struct {
	Name      string
	ConfigDir string
	AddedAt   time.Time
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Shared between every account. History must be shared or sessions fork, so
// a conflict there is an error. The other directories are created in the
// shared store up front, because Claude Code would otherwise create private
// ones in the account that later block linking. Files are linked once the
// shared store has them.
var (
	requiredDirs = []string{"projects", "tasks"}
	optionalDirs = []string{"skills", "agents", "commands", "plugins"}
	sharedFiles  = []string{"settings.json", "CLAUDE.md", "keybindings.json"}
)

// StrippedEnv are removed from every claude process cs starts: each would
// override the account's own login, for every profile.
var StrippedEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

var loggedInTimeout = 10 * time.Second

func load(p paths.Paths) map[string]Account {
	m := map[string]Account{}
	_ = store.ReadJSON(p.AccountsFile(), &m)
	return m
}

// List returns every account, "default" first, then by name.
func List(p paths.Paths) []Account {
	out := []Account{{Name: paths.DefaultAccount, ConfigDir: p.ClaudeDir}}
	m := load(p)
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		a := m[n]
		a.Name, a.ConfigDir = n, p.AccountDir(n)
		out = append(out, a)
	}
	return out
}

// Get returns one account by name.
func Get(p paths.Paths, name string) (Account, bool) {
	for _, a := range List(p) {
		if a.Name == name {
			return a, true
		}
	}
	return Account{}, false
}

// Create registers a new account, makes its config directory and links it
// to the shared store.
func Create(p paths.Paths, name string) (Account, error) {
	if name == paths.DefaultAccount {
		return Account{}, fmt.Errorf("%q is reserved for the login already in %s", name, p.ClaudeDir)
	}
	if !nameRE.MatchString(name) {
		return Account{}, fmt.Errorf("account name %q: use lowercase letters, digits, - and _, starting with a letter or digit, at most 32 characters", name)
	}
	m := load(p)
	if _, ok := m[name]; ok {
		return Account{}, fmt.Errorf("account %q already exists", name)
	}
	a := Account{Name: name, ConfigDir: p.AccountDir(name), AddedAt: time.Now()}
	if err := os.MkdirAll(a.ConfigDir, 0o700); err != nil {
		return Account{}, err
	}
	if err := EnsureLinks(p, a); err != nil {
		return Account{}, err
	}
	m[name] = a
	return a, store.WriteJSON(p.AccountsFile(), m)
}

// EnsureLinks adds any missing links from the account's config directory to
// the shared store, ignoring warnings.
func EnsureLinks(p paths.Paths, a Account) error {
	_, err := CheckLinks(p, a)
	return err
}

// CheckLinks adds any missing links and never replaces a real file or
// directory. A private history directory is an error; anything else in the
// way is returned as a warning, since the account still works without it.
func CheckLinks(p paths.Paths, a Account) (warnings []string, err error) {
	if a.ConfigDir == p.ClaudeDir {
		return nil, nil
	}
	if err := os.MkdirAll(a.ConfigDir, 0o700); err != nil {
		return nil, err
	}
	for _, n := range append(append([]string{}, requiredDirs...), optionalDirs...) {
		if err := os.MkdirAll(filepath.Join(p.ClaudeDir, n), 0o700); err != nil {
			return nil, err
		}
	}
	required := map[string]bool{}
	for _, n := range requiredDirs {
		required[n] = true
	}
	names := append(append(append([]string{}, requiredDirs...), optionalDirs...), sharedFiles...)
	for _, n := range names {
		target := filepath.Join(p.ClaudeDir, n)
		if _, err := os.Stat(target); err != nil {
			continue
		}
		link := filepath.Join(a.ConfigDir, n)
		st, err := os.Lstat(link)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Symlink(target, link); err != nil {
				return warnings, err
			}
			continue
		}
		if err != nil {
			return warnings, err
		}
		problem := ""
		if st.Mode()&os.ModeSymlink == 0 {
			problem = fmt.Sprintf("%s is not a link to the shared %s", link, target)
		} else if got, _ := os.Readlink(link); got != target {
			problem = fmt.Sprintf("%s links to %s, not the shared %s", link, got, target)
		}
		if problem == "" {
			continue
		}
		if required[n] {
			return warnings, fmt.Errorf("%s; this account would keep its own %s and sessions could not move between accounts. Move it aside and try again", problem, n)
		}
		warnings = append(warnings, problem+"; account "+a.Name+" uses its own copy")
	}
	return warnings, nil
}

// Delete removes an account's registration and config directory. Links are
// removed as links; the shared store is never followed.
func Delete(p paths.Paths, name string) error {
	if name == paths.DefaultAccount {
		return errors.New("the default account is ~/.claude itself and cannot be removed")
	}
	m := load(p)
	if _, ok := m[name]; !ok {
		return fmt.Errorf("no account named %q", name)
	}
	if err := os.RemoveAll(p.AccountDir(name)); err != nil {
		return err
	}
	delete(m, name)
	return store.WriteJSON(p.AccountsFile(), m)
}

// ResolveIdentity reads which account a config directory is signed in to.
// It is the only place that knows where Claude Code records that; on any
// failure it returns an unknown identity. It reads no credential.
func ResolveIdentity(configDir string) store.Identity {
	for _, f := range identityFiles(configDir) {
		var d struct {
			OAuthAccount *struct {
				EmailAddress     string `json:"emailAddress"`
				AccountUUID      string `json:"accountUuid"`
				OrganizationUUID string `json:"organizationUuid"`
				OrganizationName string `json:"organizationName"`
			} `json:"oauthAccount"`
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if json.Unmarshal(b, &d) != nil || d.OAuthAccount == nil || d.OAuthAccount.EmailAddress == "" {
			return store.Identity{}
		}
		o := d.OAuthAccount
		return store.Identity{Email: o.EmailAddress, AccountID: o.AccountUUID, OrgID: o.OrganizationUUID, OrgName: o.OrganizationName, Known: true}
	}
	return store.Identity{}
}

// IdentityFile is the file ResolveIdentity would read, for change detection.
func identityFiles(configDir string) []string {
	// Without CLAUDE_CONFIG_DIR, Claude Code keeps this file beside ~/.claude.
	beside := filepath.Join(filepath.Dir(configDir), ".claude.json")
	if home, err := os.UserHomeDir(); err == nil && configDir == filepath.Join(home, ".claude") {
		// The real default runs with the variable unset (see ConfigEnv), so
		// only the home file is current; one inside ~/.claude would be stale.
		return []string{beside}
	}
	return []string{filepath.Join(configDir, ".claude.json"), beside}
}

// ConfigEnv returns the CLAUDE_CONFIG_DIR value to launch an account with.
// The real ~/.claude must run with the variable unset: setting it would move
// .claude.json and re-key the macOS Keychain entry, signing the user out.
func ConfigEnv(p paths.Paths, a Account) (value string, set bool) {
	if a.ConfigDir == p.ClaudeDir {
		if home, err := os.UserHomeDir(); err == nil && p.ClaudeDir == filepath.Join(home, ".claude") {
			return "", false
		}
	}
	return a.ConfigDir, true
}

// Environ returns env with CLAUDE_CONFIG_DIR set or removed for the account
// and the credential variables in StrippedEnv removed.
func Environ(p paths.Paths, a Account, env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDE_CONFIG_DIR" || isStripped(k) {
			continue
		}
		out = append(out, kv)
	}
	if v, ok := ConfigEnv(p, a); ok {
		out = append(out, "CLAUDE_CONFIG_DIR="+v)
	}
	return out
}

func isStripped(key string) bool {
	for _, s := range StrippedEnv {
		if key == s {
			return true
		}
	}
	return false
}

// LoggedIn asks Claude Code whether the account itself is signed in. The
// check runs without credential variables (which would report "signed in"
// for any profile) and gives up after a few seconds.
func LoggedIn(p paths.Paths, a Account) bool {
	ctx, cancel := context.WithTimeout(context.Background(), loggedInTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.ClaudeBin, "auth", "status")
	cmd.Env = Environ(p, a, os.Environ())
	cmd.WaitDelay = 500 * time.Millisecond
	out, _ := cmd.Output()
	var d struct {
		LoggedIn bool `json:"loggedIn"`
	}
	return json.Unmarshal(out, &d) == nil && d.LoggedIn
}
