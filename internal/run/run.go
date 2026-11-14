// Package run is the wrapper around a Claude Code session: it picks the
// account, guards against two writers, launches claude as a child in the
// user's terminal, and (switch.go) relaunches it under another account.
package run

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/project"
	"claude-sessions/internal/report"
	"claude-sessions/internal/store"
	"claude-sessions/internal/transcript"
)

// Opts are the user's choices for one launch.
type Opts struct {
	Account, Name, SessionRef string
	Force                     bool
	ClaudeArgs                []string
}

// Stdio is what the session's terminal is attached to.
type Stdio struct {
	In       io.Reader
	Out, Err io.Writer
}

// Choice is one account offered when cs has to ask.
type Choice struct{ Name, Label, Usage string }

// Runner carries everything a launch depends on, so tests can supply it.
type Runner struct {
	P       paths.Paths
	Cwd     string
	CSBin   string   // path to this executable, for the injected hooks
	Environ []string // base environment for the session
	Stdio   Stdio
	Ask     func([]Choice) string          // pick an account; nil uses a numbered prompt
	Pick    func([]transcript.Info) string // pick a session id; nil uses a numbered prompt
}

// StrippedEnv are removed from every session: each would override the
// account's own login for all profiles.
var StrippedEnv = []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"}

func (r *Runner) errf(format string, a ...any) int {
	fmt.Fprintf(r.Stdio.Err, "cs: "+format+"\n", a...)
	return 1
}

// New starts a new session in r.Cwd.
func (r *Runner) New(o Opts) int {
	id, err := newUUID()
	if err != nil {
		return r.errf("%v", err)
	}
	return r.launch(id, false, "", "", o)
}

// Resume continues an existing session, under any account.
func (r *Runner) Resume(o Opts) int {
	var info transcript.Info
	if o.SessionRef == "" {
		id, err := r.pickSession()
		if err != nil {
			return r.errf("%v", err)
		}
		o.SessionRef = id
	}
	info, err := transcript.Find(r.P.ClaudeDir, o.SessionRef)
	if errors.Is(err, transcript.ErrAmbiguous) {
		return r.errf("more than one session matches %q; use more of the id", o.SessionRef)
	}
	if err != nil {
		return r.errf("no session matches %q (try `cs status --all`)", o.SessionRef)
	}

	prev, hadState := store.GetSession(r.P, info.ID)
	if store.IsLocked(r.P, info.ID) {
		return r.errf("session %s is already running under cs; use `cs switch %s <account>` to change its account", short(info.ID), short(info.ID))
	}
	if !o.Force {
		cleanExit := hadState && prev.Managed && prev.State == store.StateEnded
		if (!cleanExit && transcript.RecentlyWritten(info.Path, time.Now())) || openElsewhere(info.ID) {
			return r.errf("session %s appears to be open in another terminal (its history changed in the last minute, or a claude process has it open). Close it there first, or pass --force.", short(info.ID))
		}
	}
	if o.Name == "" {
		o.Name = info.Name
	}
	if info.Cwd != "" {
		// A session resumes in the directory it was started in.
		if st, err := os.Stat(info.Cwd); err == nil && st.IsDir() {
			r.Cwd = info.Cwd
		}
	}
	return r.launch(info.ID, true, info.Path, prev.Profile, o)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// openElsewhere reports whether some claude process was started with this
// session id. Best effort: plain claude cannot be locked.
func openElsewhere(id string) bool {
	out, err := exec.Command("ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return false
	}
	self := strconv.Itoa(os.Getpid())
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == self {
			continue
		}
		if strings.Contains(line, id) && (strings.Contains(line, "--resume") || strings.Contains(line, "--session-id")) {
			return true
		}
	}
	return false
}

// identityLookup resolves a profile name to its current identity.
func identityLookup(p paths.Paths) func(string) (store.Identity, bool) {
	return func(name string) (store.Identity, bool) {
		a, ok := accounts.Get(p, name)
		if !ok {
			return store.Identity{}, false
		}
		return accounts.ResolveIdentity(a.ConfigDir), true
	}
}

// checkAccount verifies an account exists, is linked to the shared store
// and may run this project.
func checkAccount(p paths.Paths, root, name string, s project.Settings) (accounts.Account, store.Identity, error) {
	a, ok := accounts.Get(p, name)
	if !ok {
		return a, store.Identity{}, fmt.Errorf("no account named %q (see `cs account list`)", name)
	}
	if err := accounts.EnsureLinks(p, a); err != nil {
		return a, store.Identity{}, err
	}
	id := accounts.ResolveIdentity(a.ConfigDir)
	if ok, reason := project.Allowed(s, root, a.Name, id, identityLookup(p)); !ok {
		return a, id, errors.New(reason)
	}
	return a, id, nil
}

// ChooseAccount picks the account for a launch: the one asked for, else the
// one the session last ran under, else the project's default, else ask.
func ChooseAccount(p paths.Paths, root string, o Opts, prev string, s project.Settings, ask func([]Choice) string) (string, error) {
	if o.Account != "" {
		return o.Account, nil
	}
	for _, name := range []string{prev, s.DefaultAccount} {
		if name == "" {
			continue
		}
		if _, ok := accounts.Get(p, name); ok {
			return name, nil
		}
	}
	usage := map[string]store.Usage{}
	for _, u := range store.LoadUsage(p) {
		usage[u.Identity.Key(u.Profile)] = u
	}
	var choices []Choice
	for _, a := range accounts.List(p) {
		id := accounts.ResolveIdentity(a.ConfigDir)
		if ok, _ := project.Allowed(s, root, a.Name, id, identityLookup(p)); !ok {
			continue
		}
		c := Choice{Name: a.Name, Label: id.Label(a.Name), Usage: "usage not seen yet"}
		if u, ok := usage[id.Key(a.Name)]; ok {
			c.Usage = usageText(u, time.Now())
		}
		choices = append(choices, c)
	}
	switch len(choices) {
	case 0:
		return "", errors.New("no account is allowed for this project (see `cs project show`)")
	case 1:
		return choices[0].Name, nil
	}
	name := ask(choices)
	if name == "" {
		return "", errors.New("no account chosen")
	}
	return name, nil
}

func usageText(u store.Usage, now time.Time) string {
	part := func(label string, w *store.Window) string {
		if w == nil {
			return ""
		}
		if !w.ResetsAt.IsZero() && now.After(w.ResetsAt) {
			return label + " reset"
		}
		return fmt.Sprintf("%s %.0f%%", label, w.UsedPct)
	}
	var out []string
	for _, s := range []string{part("5h", u.FiveHour), part("week", u.SevenDay)} {
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return "usage not seen yet"
	}
	return strings.Join(out, ", ")
}

func (r *Runner) ask(choices []Choice) string {
	if r.Ask != nil {
		return r.Ask(choices)
	}
	fmt.Fprintln(r.Stdio.Err, "Which account?")
	for i, c := range choices {
		fmt.Fprintf(r.Stdio.Err, "  %d) %-12s %-36s %s\n", i+1, c.Name, c.Label, c.Usage)
	}
	n := r.readNumber(len(choices))
	if n == 0 {
		return ""
	}
	return choices[n-1].Name
}

func (r *Runner) readNumber(max int) int {
	fmt.Fprint(r.Stdio.Err, "> ")
	var line string
	if _, err := fmt.Fscanln(r.Stdio.In, &line); err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 1 || n > max {
		return 0
	}
	return n
}

func (r *Runner) pickSession() (string, error) {
	root := project.Root(r.Cwd)
	var here []transcript.Info
	for _, i := range transcript.List(r.P.ClaudeDir) {
		if i.Cwd == root || strings.HasPrefix(i.Cwd, root+string(filepath.Separator)) || (i.Cwd != "" && project.Root(i.Cwd) == root) {
			here = append(here, i)
		}
		if len(here) == 15 {
			break
		}
	}
	if len(here) == 0 {
		return "", fmt.Errorf("no sessions found for %s; start one with `cs new`", root)
	}
	if r.Pick != nil {
		return r.Pick(here), nil
	}
	fmt.Fprintf(r.Stdio.Err, "Sessions in %s:\n", root)
	for n, i := range here {
		name := i.Name
		if name == "" {
			name = "(unnamed)"
		}
		fmt.Fprintf(r.Stdio.Err, "  %d) %s  %-28s %s\n", n+1, short(i.ID), name, i.ModTime.Format("Jan 2 15:04"))
	}
	n := r.readNumber(len(here))
	if n == 0 {
		return "", errors.New("no session chosen")
	}
	return here[n-1].ID, nil
}

// wrapper is one running session.
type wrapper struct {
	r        *Runner
	id       string
	root     string
	settings project.Settings
	name     string
	extra    []string // the user's own claude arguments
}

func (r *Runner) launch(id string, resume bool, transcriptPath, prevProfile string, o Opts) int {
	root := project.Root(r.Cwd)
	settings := project.Load(r.P, root)

	name, err := ChooseAccount(r.P, root, o, prevProfile, settings, r.ask)
	if err != nil {
		return r.errf("%v", err)
	}
	acct, ident, err := checkAccount(r.P, root, name, settings)
	if err != nil {
		return r.errf("%v", err)
	}
	if helper := apiKeyHelper(r.P.ClaudeDir); helper {
		return r.errf("%s sets apiKeyHelper, which overrides every account's login, so cs cannot tell accounts apart. Remove it to use cs.", filepath.Join(r.P.ClaudeDir, "settings.json"))
	}

	release, err := store.AcquireLock(r.P, id)
	if err != nil {
		return r.errf("session %s is already running under cs", short(id))
	}
	defer release()

	w := &wrapper{r: r, id: id, root: root, settings: settings, name: o.Name, extra: o.ClaudeArgs}
	if err := os.MkdirAll(filepath.Dir(w.settingsFile()), 0o700); err != nil {
		return r.errf("%v", err)
	}
	if err := os.WriteFile(w.settingsFile(), report.InjectedSettings(r.CSBin), 0o600); err != nil {
		return r.errf("%v", err)
	}
	err = store.UpdateSession(r.P, id, func(s *store.Session) {
		s.Cwd, s.ProjectRoot = r.Cwd, root
		s.Profile, s.Identity, s.AccountKey = acct.Name, ident, ident.Key(acct.Name)
		s.Managed, s.WrapperPID = true, os.Getpid()
		s.State, s.StartedAt = store.StateStarting, time.Now()
		s.SwitchTo, s.SwitchForce = "", false
		if o.Name != "" {
			s.Name = o.Name
		}
		if transcriptPath != "" {
			s.TranscriptPath = transcriptPath
		}
	})
	if err != nil {
		return r.errf("%v", err)
	}
	if removed := r.strippedPresent(); len(removed) > 0 {
		fmt.Fprintf(r.Stdio.Err, "cs: not passing %s to the session: it would override the account's login\n", strings.Join(removed, ", "))
	}
	return w.run(acct, resume)
}

func (w *wrapper) settingsFile() string {
	return filepath.Join(w.r.P.StateDir(), "settings", w.id+".json")
}

func apiKeyHelper(claudeDir string) bool {
	var d map[string]any
	if store.ReadJSON(filepath.Join(claudeDir, "settings.json"), &d) != nil {
		return false
	}
	_, ok := d["apiKeyHelper"]
	return ok
}

func (r *Runner) strippedPresent() []string {
	var out []string
	for _, kv := range r.Environ {
		k, v, _ := strings.Cut(kv, "=")
		for _, s := range StrippedEnv {
			if k == s && v != "" {
				out = append(out, k)
			}
		}
	}
	return out
}

// command builds the process for one launch of the session under acct.
func (w *wrapper) command(acct accounts.Account, resume bool) *exec.Cmd {
	r := w.r
	args := []string{"--session-id", w.id}
	if resume {
		args = []string{"--resume", w.id}
	}
	if w.name != "" {
		args = append(args, "--name", w.name)
	}
	args = append(args, "--settings", w.settingsFile())
	args = append(args, w.extra...)

	var cmd *exec.Cmd
	if tmpl := w.settings.Launch; tmpl != "" {
		cfg, _ := accounts.ConfigEnv(r.P, acct)
		if cfg == "" {
			cfg = acct.ConfigDir
		}
		script := strings.NewReplacer(
			"{claude}", report.ShellQuote(r.P.ClaudeBin),
			"{config_dir}", report.ShellQuote(cfg),
			"{state_dir}", report.ShellQuote(r.P.StateDir()),
		).Replace(tmpl)
		cmd = exec.Command("sh", append([]string{"-c", "exec " + script + ` "$@"`, "cs"}, args...)...)
	} else {
		cmd = exec.Command(r.P.ClaudeBin, args...)
	}
	cmd.Dir = r.Cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = r.Stdio.In, r.Stdio.Out, r.Stdio.Err

	env := make([]string, 0, len(r.Environ)+6)
	for _, kv := range r.Environ {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, s := range append([]string{"CS_SESSION", "CS_PROFILE", "CS_USER_STATUSLINE", "CS_HOME", "CS_CLAUDE_DIR", "CS_BIN"}, StrippedEnv...) {
			if k == s {
				drop = true
			}
		}
		if !drop {
			env = append(env, kv)
		}
	}
	env = accounts.Environ(r.P, acct, env)
	env = append(env,
		"CS_SESSION=1",
		"CS_PROFILE="+acct.Name,
		"CS_HOME="+r.P.Home,
		"CS_CLAUDE_DIR="+r.P.ClaudeDir,
		"CS_BIN="+r.CSBin,
	)
	if user := report.UserStatusLine(r.P.ClaudeDir); user != "" {
		env = append(env, "CS_USER_STATUSLINE="+user)
	}
	cmd.Env = env
	return cmd
}

// run launches the session and stays its parent until it ends.
func (w *wrapper) run(acct accounts.Account, resume bool) int {
	r := w.r
	// The terminal sends Ctrl-C and Ctrl-\ to the whole foreground group;
	// they are for claude, not for the wrapper.
	signal.Ignore(syscall.SIGINT, syscall.SIGQUIT)
	defer signal.Reset(syscall.SIGINT, syscall.SIGQUIT)
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGUSR1)
	defer signal.Stop(sigs)

	cmd := w.command(acct, resume)
	if err := cmd.Start(); err != nil {
		w.finish()
		return r.errf("cannot start %s: %v", r.P.ClaudeBin, err)
	}
	done := waitChan(cmd)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()

	for {
		select {
		case err := <-done:
			w.finish()
			return exitCode(err)
		case s := <-sigs:
			if s == syscall.SIGUSR1 {
				continue
			}
			_ = cmd.Process.Signal(s)
		case <-tick.C:
		}
	}
}

func (w *wrapper) finish() {
	_ = store.UpdateSession(w.r.P, w.id, func(s *store.Session) {
		s.State, s.WrapperPID = store.StateEnded, 0
		s.SwitchTo, s.SwitchForce = "", false
	})
	_ = os.Remove(w.settingsFile())
}

func waitChan(cmd *exec.Cmd) chan error {
	c := make(chan error, 1)
	go func() { c <- cmd.Wait() }()
	return c
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ee.ExitCode() >= 0 {
			return ee.ExitCode()
		}
		return 1 // killed by a signal
	}
	return 1
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
