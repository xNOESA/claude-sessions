package run

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/project"
	"claude-sessions/internal/store"
	"claude-sessions/internal/transcript"
)

const (
	stopGrace      = 10 * time.Second // SIGTERM, then SIGKILL after this
	fallbackWindow = 5 * time.Second  // a failure this soon after a switch falls back
)

// validateTarget checks an account can take over a session in this project:
// it exists, is allowed, and is signed in.
func validateTarget(p paths.Paths, root, name string, s project.Settings) (accounts.Account, store.Identity, error) {
	a, id, err := checkAccount(p, root, name, s)
	if err != nil {
		return a, id, err
	}
	if !accounts.LoggedIn(p, a) {
		return a, id, fmt.Errorf("account %q is not signed in; run `cs account login %s`", name, name)
	}
	return a, id, nil
}

// findRunning resolves ref to a session running under a cs wrapper.
func findRunning(p paths.Paths, ref string) (store.Session, error) {
	var running []store.Session
	for _, s := range store.LoadSessions(p) {
		if s.Managed && store.IsLocked(p, s.ID) {
			running = append(running, s)
		}
	}
	var hits []store.Session
	for _, match := range []func(store.Session) bool{
		func(s store.Session) bool { return s.ID == ref },
		func(s store.Session) bool { return s.Name != "" && s.Name == ref },
		func(s store.Session) bool { return ref != "" && strings.HasPrefix(s.ID, ref) },
	} {
		for _, s := range running {
			if match(s) {
				hits = append(hits, s)
			}
		}
		if len(hits) > 0 {
			break
		}
	}
	switch {
	case len(hits) == 1:
		return hits[0], nil
	case len(hits) > 1:
		return store.Session{}, fmt.Errorf("more than one running session matches %q; use more of the id", ref)
	}
	if info, err := transcript.Find(p.ClaudeDir, ref); err == nil {
		return store.Session{}, fmt.Errorf("session %s is not running under cs; use `cs resume %s -a <account>` to open it under another account", short(info.ID), short(info.ID))
	}
	return store.Session{}, fmt.Errorf("no running session matches %q (see `cs status`)", ref)
}

// SwitchOpts are the choices for one switch.
type SwitchOpts struct {
	Force         bool // do not wait for the current turn to finish
	Compact       bool // summarise under the current account first
	AllowCrossOrg bool // the user accepts losing earlier thinking
}

// CrossOrgError is returned when a switch would move a session to another
// organization and the user has not said how to handle that.
type CrossOrgError struct{ From, To string }

func (e *CrossOrgError) Error() string {
	return fmt.Sprintf("%s is in a different organization from %s. %s. Pass --compact to summarise the session first, or --yes to switch as it is.",
		e.To, e.From, crossOrgWhy)
}

// Switch asks the wrapper running a session to move it to another account.
// It validates first, so a refused switch never disturbs the session.
func Switch(p paths.Paths, ref, account string, o SwitchOpts) (string, error) {
	s, err := findRunning(p, ref)
	if err != nil {
		return "", err
	}
	if s.Profile == account {
		return "", fmt.Errorf("session %s is already on account %q", short(s.ID), account)
	}
	root := s.ProjectRoot
	if root == "" {
		root = project.Root(s.Cwd)
	}
	_, ident, err := validateTarget(p, root, account, project.Load(p, root))
	if err != nil {
		return "", err
	}
	if crossOrg(s.Identity, ident) && !o.AllowCrossOrg && !o.Compact {
		return "", &CrossOrgError{From: s.Identity.Label(s.Profile), To: ident.Label(account)}
	}
	var pid int
	var state string
	if err := store.UpdateSession(p, s.ID, func(x *store.Session) {
		x.SwitchTo, x.SwitchForce, x.SwitchCompact = account, o.Force, o.Compact
		pid, state = x.WrapperPID, x.State
	}); err != nil {
		return "", err
	}
	if pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGUSR1)
	}
	if o.Force || state == store.StateIdle {
		return "switching now", nil
	}
	return "will switch when the current turn finishes", nil
}

// live is the wrapper's view of the process it is currently parenting.
type live struct {
	cmd        *exec.Cmd
	done       chan error
	acct       accounts.Account
	prev       *accounts.Account // set after a switch, until the new launch has proved itself
	switchedAt time.Time
	closing    bool // the terminal is going away; whatever claude exits with, we are done
}

func (w *wrapper) say(format string, a ...any) {
	fmt.Fprintf(w.r.Stdio.Err, "cs: "+format+"\n", a...)
}

// start launches the session under acct and records the account.
func (w *wrapper) start(l *live, acct accounts.Account, resume bool) error {
	ident := accounts.ResolveIdentity(acct.ConfigDir)
	_ = store.UpdateSession(w.r.P, w.id, func(s *store.Session) {
		s.Profile, s.Identity, s.AccountKey = acct.Name, ident, ident.Key(acct.Name)
		s.NoteAccount(s.AccountKey, time.Now())
		s.State = store.StateStarting
		s.SwitchTo, s.SwitchForce, s.SwitchCompact = "", false, false
	})
	cmd := w.command(acct, resume)
	if err := cmd.Start(); err != nil {
		return err
	}
	l.cmd, l.done, l.acct = cmd, waitChan(cmd), acct
	return nil
}

// stop ends the running claude the way closing its terminal would.
func (l *live) stop() {
	_ = l.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-l.done:
	case <-time.After(stopGrace):
		_ = l.cmd.Process.Kill()
		<-l.done
	}
}

// maybeSwitch acts on a pending switch request once the session is idle
// (or at once when forced).
func (w *wrapper) maybeSwitch(l *live) {
	if l.closing {
		return
	}
	s, ok := store.GetSession(w.r.P, w.id)
	if !ok || s.SwitchTo == "" {
		return
	}
	if s.State != store.StateIdle && !s.SwitchForce {
		return
	}
	clear := func() {
		_ = store.UpdateSession(w.r.P, w.id, func(x *store.Session) { x.SwitchTo, x.SwitchForce, x.SwitchCompact = "", false, false })
	}
	// Settings may have changed since the request; check again right before acting.
	target, ident, err := validateTarget(w.r.P, w.root, s.SwitchTo, project.Load(w.r.P, w.root))
	if err != nil {
		clear()
		w.say("not switching: %v", err)
		return
	}
	if target.Name == l.acct.Name {
		clear()
		return
	}
	from := l.acct
	l.stop()
	// Within one organization the earlier thinking stays usable, so there is
	// nothing to gain from summarising.
	if s.SwitchCompact && !sameOrg(accounts.ResolveIdentity(from.ConfigDir), ident) {
		w.say("compacting the session under %s before the switch…", from.Name)
		if err := w.compact(from); err != nil {
			w.say("could not compact (%v); switching anyway", err)
		}
	}
	if err := w.start(l, target, true); err != nil {
		w.say("could not start under %s (%v); back on %s", target.Name, err, from.Name)
		if err := w.start(l, from, true); err != nil {
			w.say("could not restart the session: %v", err)
			l.done = closedChan(err)
		}
		return
	}
	l.prev, l.switchedAt = &from, time.Now()
	w.say("→ now on %s", ident.Label(target.Name))
}

// fallBack handles the new account failing right after a switch: put the
// session back on the account it came from. Reports whether it did.
func (w *wrapper) fallBack(l *live, exitErr error) bool {
	if l.closing || l.prev == nil || exitCode(exitErr) == 0 || time.Since(l.switchedAt) > fallbackWindow {
		return false
	}
	from := *l.prev
	l.prev = nil
	w.say("claude exited with code %d right after switching to %s; back on %s", exitCode(exitErr), l.acct.Name, from.Name)
	if err := w.start(l, from, true); err != nil {
		w.say("could not restart the session: %v", err)
		return false
	}
	return true
}

func closedChan(err error) chan error {
	c := make(chan error, 1)
	if err == nil {
		err = errors.New("not running")
	}
	c <- err
	return c
}
