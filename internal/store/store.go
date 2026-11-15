// Package store holds cs's state files: one JSON file per session, one per
// account's usage, and one lock per running session.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"claude-sessions/internal/paths"
)

// Identity is the account a config directory is really signed in to.
type Identity struct {
	Email     string
	OrgName   string
	OrgID     string
	AccountID string
	Plan      string
	Known     bool
}

// Key is the stable name usage is stored under.
func (i Identity) Key(profile string) string {
	if i.Known && (i.OrgID != "" || i.AccountID != "") {
		return safe(i.OrgID + "_" + i.AccountID)
	}
	return "profile_" + safe(profile)
}

// Label is what the dashboard shows for the account.
func (i Identity) Label(profile string) string {
	if !i.Known || i.Email == "" {
		return profile + " ?"
	}
	if i.Plan != "" {
		return i.Email + " · " + i.Plan
	}
	return i.Email
}

func safe(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		}
		return '-'
	}, s)
}

// TaskProgress is a session's to-do list summary.
type TaskProgress struct {
	Done, Total int
	Current     string
	Known       bool
}

// Session states.
const (
	StateStarting = "starting"
	StateWorking  = "working"
	StateIdle     = "idle"
	StateNeedsYou = "needs_you"
	StateEnded    = "ended"
)

// Session is the last known state of one Claude Code session.
type Session struct {
	ID, Name, Cwd, ProjectRoot, TranscriptPath string
	Profile, AccountKey, Model                 string
	Identity                                   Identity
	State                                      string
	Managed                                    bool
	WrapperPID                                 int
	StartedAt, UpdatedAt                       time.Time
	ContextPct, CostUSD                        *float64
	Tasks                                      TaskProgress
	SwitchTo                                   string
	SwitchForce                                bool
	// Spans records which account the session ran under from when, so its
	// usage can be attributed after it moves between accounts.
	Spans []AccountSpan `json:",omitempty"`
}

// AccountSpan marks the moment a session started running under an account.
type AccountSpan struct {
	Key  string
	From time.Time
}

// NoteAccount records that the session is on the account with this usage
// key from at onwards. Repeating the current account adds nothing.
func (s *Session) NoteAccount(key string, at time.Time) {
	if key == "" {
		return
	}
	if n := len(s.Spans); n > 0 && s.Spans[n-1].Key == key {
		return
	}
	s.Spans = append(s.Spans, AccountSpan{Key: key, From: at})
}

// AccountAt returns the usage key of the account the session was on at t,
// or "" for a time before cs first saw the session.
func (s *Session) AccountAt(t time.Time) string {
	key := ""
	for _, sp := range s.Spans {
		if sp.From.After(t) {
			break
		}
		key = sp.Key
	}
	return key
}

// Window is one rate-limit window.
type Window struct {
	UsedPct  float64
	ResetsAt time.Time
}

// ScopedWindow is a per-model weekly limit, labelled by the server.
type ScopedWindow struct {
	Name string
	Window
}

// ExtraUsage is pay-as-you-go spend beyond the plan, in currency units.
type ExtraUsage struct {
	Enabled     bool
	UsedPct     float64
	Used, Limit float64
	Currency    string
}

// Usage is the last limit usage seen for one real account. FiveHour and
// SevenDay come from running sessions' status lines and from usage probes;
// the rest only from probes.
type Usage struct {
	FiveHour, SevenDay *Window
	SeenAt             time.Time // when any reading last arrived
	Identity           Identity
	Profile            string

	Plan       string // subscription type, e.g. "max"
	Available  bool   // plan limits apply to this account
	Scoped     []ScopedWindow
	Extra      *ExtraUsage
	FetchedAt  time.Time // last successful probe
	TriedAt    time.Time // last probe attempt
	ProbeError string    // why the last probe failed, if it did
}

// WriteJSON writes v to path atomically (temp file then rename), mode 0600,
// creating parent directories with mode 0700.
func WriteJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, 0o600)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		os.Remove(name)
	}
	return werr
}

// ReadJSON reads path into v. A missing file returns os.ErrNotExist.
func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// withFileLock runs fn while holding an exclusive flock on path+".lk".
func withFileLock(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".lk", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// UpdateSession applies fn to the stored session (or a new one) under a
// file lock, so concurrent reporters never lose each other's updates.
func UpdateSession(p paths.Paths, id string, fn func(*Session)) error {
	if id == "" || id != filepath.Base(id) {
		return fmt.Errorf("bad session id %q", id)
	}
	path := p.SessionFile(id)
	return withFileLock(path, func() error {
		var s Session
		if err := ReadJSON(path, &s); err != nil && !errors.Is(err, os.ErrNotExist) {
			s = Session{} // corrupt: start again rather than fail forever
		}
		s.ID = id
		fn(&s)
		s.UpdatedAt = time.Now()
		return WriteJSON(path, &s)
	})
}

// GetSession returns one stored session.
func GetSession(p paths.Paths, id string) (Session, bool) {
	var s Session
	if err := ReadJSON(p.SessionFile(id), &s); err != nil || s.ID == "" {
		return Session{}, false
	}
	return s, true
}

// LoadSessions returns every readable session state file.
func LoadSessions(p paths.Paths) []Session {
	var out []Session
	for _, f := range jsonFiles(p.SessionsDir()) {
		var s Session
		if err := ReadJSON(f, &s); err == nil && s.ID != "" {
			out = append(out, s)
		}
	}
	return out
}

// SaveUsage stores u under its identity's key.
func SaveUsage(p paths.Paths, u Usage) error {
	return WriteJSON(p.UsageFile(u.Identity.Key(u.Profile)), &u)
}

// LoadUsage returns every readable usage file.
func LoadUsage(p paths.Paths) []Usage {
	var out []Usage
	for _, f := range jsonFiles(p.UsageDir()) {
		var u Usage
		if err := ReadJSON(f, &u); err == nil {
			out = append(out, u)
		}
	}
	return out
}

func jsonFiles(dir string) []string {
	m, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	return m
}

// ErrLocked means another cs wrapper is running the session.
var ErrLocked = errors.New("session is already running")

// AcquireLock takes the session's lock without blocking. The lock is an
// flock, so it disappears with the process even if release is never called.
func AcquireLock(p paths.Paths, id string) (release func(), err error) {
	path := p.LockFile(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrLocked
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// IsLocked reports whether some process holds the session's lock.
func IsLocked(p paths.Paths, id string) bool {
	f, err := os.OpenFile(p.LockFile(id), os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}

// Logf appends a line to cs.log. It never fails.
func Logf(p paths.Paths, format string, a ...any) {
	if os.MkdirAll(p.Home, 0o700) != nil {
		return
	}
	f, err := os.OpenFile(p.LogFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, a...)...)
}
