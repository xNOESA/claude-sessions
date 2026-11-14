// Package project finds a session's project and holds its optional settings.
package project

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/store"
)

// Root is the project a directory belongs to: the main repository of its
// git checkout (so worktrees share their repository's settings), or the
// directory itself outside git.
func Root(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return cwd
	}
	common := strings.TrimRight(string(out), "\r\n")
	if !filepath.IsAbs(common) || filepath.Base(common) != ".git" {
		// A bare repository or an unusual layout: fall back to the checkout.
		top := exec.Command("git", "rev-parse", "--show-toplevel")
		top.Dir = cwd
		if o, err := top.Output(); err == nil {
			if t := strings.TrimRight(string(o), "\r\n"); filepath.IsAbs(t) {
				return t
			}
		}
		return cwd
	}
	return filepath.Dir(common)
}

// Settings are one project's optional refinements.
type Settings struct {
	PlanCommand     string   `json:",omitempty"`
	PlanFile        string   `json:",omitempty"`
	DefaultAccount  string   `json:",omitempty"`
	Launch          string   `json:",omitempty"`
	AllowedAccounts []string `json:",omitempty"`
}

func loadAll(p paths.Paths) map[string]Settings {
	m := map[string]Settings{}
	_ = store.ReadJSON(p.ProjectsFile(), &m)
	return m
}

// Load returns the settings for a project root (zero value if none).
func Load(p paths.Paths, root string) Settings { return loadAll(p)[root] }

// Keys lists the names Set accepts.
var Keys = []string{"plan.command", "plan.file", "default_account", "launch", "allowed_accounts"}

// Set changes one setting; an empty value clears it.
func Set(p paths.Paths, root, key, value string) error {
	m := loadAll(p)
	s := m[root]
	switch key {
	case "plan.command":
		s.PlanCommand = value
	case "plan.file":
		s.PlanFile = value
	case "default_account":
		s.DefaultAccount = value
	case "launch":
		if value != "" && !strings.Contains(value, "{claude}") {
			return fmt.Errorf("launch must contain {claude}, e.g. \"mywrapper -- {claude}\"")
		}
		s.Launch = value
	case "allowed_accounts":
		s.AllowedAccounts = nil
		for _, a := range strings.Split(value, ",") {
			if a = strings.TrimSpace(a); a != "" {
				s.AllowedAccounts = append(s.AllowedAccounts, a)
			}
		}
	default:
		return fmt.Errorf("unknown setting %q (known: %s)", key, strings.Join(Keys, ", "))
	}
	if s.PlanCommand == "" && s.PlanFile == "" && s.DefaultAccount == "" && s.Launch == "" && len(s.AllowedAccounts) == 0 {
		delete(m, root)
	} else {
		m[root] = s
	}
	return store.WriteJSON(p.ProjectsFile(), m)
}

// Allowed reports whether an account, by its real identity, may run this
// project. An entry may be an email, an organization name, or a profile
// name (resolved through lookup to that profile's current organization).
func Allowed(s Settings, root, profile string, id store.Identity,
	lookup func(name string) (store.Identity, bool)) (ok bool, reason string) {
	if len(s.AllowedAccounts) == 0 {
		return true, ""
	}
	if !id.Known {
		return false, "account identity unknown; this project restricts accounts"
	}
	for _, e := range s.AllowedAccounts {
		if strings.EqualFold(e, id.Email) || (id.OrgName != "" && strings.EqualFold(e, id.OrgName)) {
			return true, ""
		}
		if other, found := lookup(e); found && other.Known && other.OrgID != "" && other.OrgID == id.OrgID {
			return true, ""
		}
	}
	return false, fmt.Sprintf("%s is not allowed for %s", id.Label(profile), root)
}
