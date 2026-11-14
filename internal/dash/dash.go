// Package dash builds and renders the dashboard. It only reads: state
// files written by sessions, the shared history, and project settings.
package dash

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
	"unsafe"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/project"
	"claude-sessions/internal/store"
	"claude-sessions/internal/transcript"
)

const (
	staleAfter  = 5 * time.Minute // usage older than this shows its age
	amberAt     = 75.0
	redAt       = 90.0
	recentFor   = 24 * time.Hour // ended sessions stay listed this long
	redrawEvery = 2 * time.Second
	dash        = "—"
)

// Row is one session on the dashboard.
type Row struct {
	ID, Project, Session, State, Account, Profile string
	FiveHour, Week, Ctx, Task, Plan               string
	FiveHourLevel, WeekLevel                      int
	Level                                         int // 0 normal, 1 amber, 2 red
	Running                                       bool
	NotAllowed                                    bool
}

// AccountRow is one registered account's last-known usage.
type AccountRow struct {
	Name, Label, FiveHour, Week, Resets string
	Level                               int
}

// UsageCell formats one rate-limit window: a dash when never seen, "reset"
// once the window has rolled over, otherwise the percentage, with its age
// when the reading is old.
func UsageCell(w *store.Window, seenAt, now time.Time) (text string, level int) {
	if w == nil {
		return dash, 0
	}
	if !w.ResetsAt.IsZero() && now.After(w.ResetsAt) {
		return "reset", 0
	}
	text = fmt.Sprintf("%.0f%%", w.UsedPct)
	switch {
	case w.UsedPct >= redAt:
		level = 2
	case w.UsedPct >= amberAt:
		level = 1
	}
	if age := now.Sub(seenAt); age > staleAfter {
		text += " (" + shortAge(age) + ")"
	}
	return text, level
}

func shortAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type entry struct {
	row    Row
	active time.Time
}

// Build gathers every session and account. Without all, it lists running
// sessions and those active in the last 24 hours.
func Build(p paths.Paths, now time.Time, all bool) ([]Row, []AccountRow) {
	usage := map[string]store.Usage{}
	for _, u := range store.LoadUsage(p) {
		usage[u.Identity.Key(u.Profile)] = u
	}
	lookup := func(name string) (store.Identity, bool) {
		a, ok := accounts.Get(p, name)
		if !ok {
			return store.Identity{}, false
		}
		return accounts.ResolveIdentity(a.ConfigDir), true
	}
	roots := map[string]string{}
	rootOf := func(cwd string) string {
		if cwd == "" {
			return ""
		}
		if r, ok := roots[cwd]; ok {
			return r
		}
		r := cwd
		if st, err := os.Stat(cwd); err == nil && st.IsDir() {
			r = project.Root(cwd)
		}
		roots[cwd] = r
		return r
	}
	type projInfo struct {
		settings project.Settings
		plan     string
	}
	projects := map[string]projInfo{}
	projOf := func(root string) projInfo {
		if pi, ok := projects[root]; ok {
			return pi
		}
		pi := projInfo{settings: project.Load(p, root), plan: dash}
		if root != "" {
			if pl := project.Progress(p, root, pi.settings, now); pl.Known {
				pi.plan = pl.Label
			}
		}
		projects[root] = pi
		return pi
	}
	taskCell := func(t store.TaskProgress) string {
		if !t.Known {
			return dash
		}
		s := fmt.Sprintf("%d/%d", t.Done, t.Total)
		if t.Current != "" {
			s += " " + t.Current
		}
		return s
	}

	transcripts := map[string]transcript.Info{}
	for _, i := range transcript.List(p.ClaudeDir) {
		transcripts[i.ID] = i
	}

	var entries []entry
	seen := map[string]bool{}
	for _, s := range store.LoadSessions(p) {
		seen[s.ID] = true
		info := transcripts[s.ID]
		running := s.Managed && store.IsLocked(p, s.ID)
		active := s.UpdatedAt
		if info.ModTime.After(active) {
			active = info.ModTime
		}
		if !running && !all && now.Sub(active) > recentFor {
			continue
		}
		root := s.ProjectRoot
		if root == "" {
			root = rootOf(s.Cwd)
		}
		pi := projOf(root)
		r := Row{ID: s.ID, Project: filepath.Base(root), Profile: s.Profile, Running: running,
			Ctx: dash, Task: taskCell(s.Tasks), Plan: pi.plan}
		if root == "" {
			r.Project = dash
		}
		r.Session = s.Name
		if r.Session == "" {
			r.Session = info.Name
		}
		if r.Session == "" {
			r.Session = shortID(s.ID)
		}
		switch {
		case !running:
			r.State = "ended"
		case s.State == store.StateStarting:
			r.State = "running, no reports"
		case s.State == store.StateWorking:
			r.State = "working"
		case s.State == store.StateNeedsYou:
			r.State = "needs you"
		default:
			r.State = "idle"
		}
		r.Account = s.Identity.Label(s.Profile)
		if ok, _ := project.Allowed(pi.settings, root, s.Profile, s.Identity, lookup); !ok {
			r.Account += " · not allowed"
			r.NotAllowed = true
			r.Level = 2
		}
		u, have := usage[s.AccountKey]
		if !have {
			u = usage[s.Identity.Key(s.Profile)]
		}
		r.FiveHour, r.FiveHourLevel = UsageCell(u.FiveHour, u.SeenAt, now)
		r.Week, r.WeekLevel = UsageCell(u.SevenDay, u.SeenAt, now)
		r.Level = max(r.Level, max(r.FiveHourLevel, r.WeekLevel))
		if s.ContextPct != nil {
			r.Ctx = fmt.Sprintf("%.0f%%", *s.ContextPct)
		}
		entries = append(entries, entry{r, active})
	}
	for id, info := range transcripts {
		if seen[id] || (!all && now.Sub(info.ModTime) > recentFor) {
			continue
		}
		root := rootOf(info.Cwd)
		r := Row{ID: id, Project: filepath.Base(root), Session: info.Name, State: "unmanaged",
			Account: dash, FiveHour: dash, Week: dash, Ctx: dash,
			Task: taskCell(transcript.Tasks(info.Path)), Plan: projOf(root).plan}
		if root == "" {
			r.Project = dash
		}
		if r.Session == "" {
			r.Session = shortID(id)
		}
		entries = append(entries, entry{r, info.ModTime})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.row.Running != b.row.Running {
			return a.row.Running
		}
		if !a.active.Equal(b.active) {
			return a.active.After(b.active)
		}
		return a.row.ID < b.row.ID
	})
	rows := make([]Row, len(entries))
	for i, e := range entries {
		rows[i] = e.row
	}

	var accts []AccountRow
	for _, a := range accounts.List(p) {
		id := accounts.ResolveIdentity(a.ConfigDir)
		ar := AccountRow{Name: a.Name, Label: id.Label(a.Name)}
		u := usage[id.Key(a.Name)]
		var l5, l7 int
		ar.FiveHour, l5 = UsageCell(u.FiveHour, u.SeenAt, now)
		ar.Week, l7 = UsageCell(u.SevenDay, u.SeenAt, now)
		ar.Level = max(l5, l7)
		if u.FiveHour != nil && u.FiveHour.ResetsAt.After(now) {
			ar.Resets = u.FiveHour.ResetsAt.Local().Format("15:04")
		}
		accts = append(accts, ar)
	}
	return rows, accts
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// RenderJSON writes the dashboard data for scripts.
func RenderJSON(w io.Writer, rows []Row, accts []AccountRow) error {
	if rows == nil {
		rows = []Row{}
	}
	if accts == nil {
		accts = []AccountRow{}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Sessions []Row
		Accounts []AccountRow
	}{rows, accts})
}

const (
	ansiReset = "\x1b[0m"
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
	ansiAmber = "\x1b[33m"
	ansiRed   = "\x1b[31m"
)

func levelColor(level int) string {
	switch level {
	case 1:
		return ansiAmber
	case 2:
		return ansiRed
	}
	return ""
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// clean makes text safe for one table cell: session names and task subjects
// come from the user and the model, and must not carry terminal control
// sequences or line breaks onto the screen.
func clean(s string) string {
	if !strings.ContainsFunc(s, unicode.IsControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t' || r == '\r':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// fit shortens s to at most n characters, marking the cut with an ellipsis.
func fit(s string, n int) string {
	if runeLen(s) <= n {
		return s
	}
	if n <= 1 {
		return "…"[:len("…")*n]
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// RenderTable writes the dashboard as a table no wider than width. Long
// session names, tasks and accounts are shortened; nothing wraps.
func RenderTable(w io.Writer, rows []Row, accts []AccountRow, width int, color bool) {
	if width < 20 {
		width = 20
	}
	paint := func(s, code string) string {
		if !color || code == "" {
			return s
		}
		return code + s + ansiReset
	}
	line := func(s string) { fmt.Fprintln(w, strings.TrimRight(s, " ")) }

	if len(rows) == 0 {
		line(fit("No sessions yet. Start one in a project directory with `cs new`.", width))
	} else {
		head := []string{"PROJECT", "SESSION", "STATE", "ACCOUNT", "5h", "WEEK", "CTX", "TASK", "PLAN"}
		const (
			cProject = iota
			cSession
			cState
			cAccount
			c5h
			cWeek
			cCtx
			cTask
			cPlan
		)
		cells := make([][]string, len(rows))
		widths := make([]int, len(head))
		for i, h := range head {
			widths[i] = runeLen(h)
		}
		for i, r := range rows {
			cells[i] = []string{r.Project, r.Session, r.State, r.Account, r.FiveHour, r.Week, r.Ctx, r.Task, r.Plan}
			for c := range cells[i] {
				cells[i][c] = clean(cells[i][c])
			}
			for c, v := range cells[i] {
				widths[c] = max(widths[c], runeLen(v))
			}
		}
		hidden := map[int]bool{}
		total := func() int {
			t, n := 0, 0
			for c, x := range widths {
				if !hidden[c] {
					t += x
					n++
				}
			}
			return t + 2*(n-1)
		}
		shrink := func(group []int, min int) {
			for total() > width {
				widest := -1
				for _, c := range group {
					if widths[c] > min && (widest < 0 || widths[c] > widths[widest]) {
						widest = c
					}
				}
				if widest < 0 {
					return
				}
				widths[widest]--
			}
		}
		// Shorten free text first; on a narrow terminal give up the context
		// column before squeezing harder, so account, usage, task and plan stay.
		free := []int{cTask, cSession, cAccount}
		shrink(free, 8)
		if total() > width {
			hidden[cCtx] = true
		}
		shrink(free, 5)
		shrink([]int{cTask, cSession, cAccount, cProject, cPlan, cState}, 4)
		render := func(vals []string, codes []string) string {
			var b strings.Builder
			used := 0
			first := true
			for c, v := range vals {
				if hidden[c] {
					continue
				}
				if !first {
					b.WriteString("  ")
					used += 2
				}
				first = false
				v = fit(v, widths[c])
				if used+runeLen(v) > width { // last resort on very narrow terminals
					v = fit(v, max(width-used, 0))
				}
				pad := widths[c] - runeLen(v)
				if used+runeLen(v)+pad > width {
					pad = max(width-used-runeLen(v), 0)
				}
				code := ""
				if codes != nil {
					code = codes[c]
				}
				b.WriteString(paint(v, code))
				b.WriteString(strings.Repeat(" ", pad))
				used += runeLen(v) + pad
				if used >= width {
					break
				}
			}
			return b.String()
		}
		bold := make([]string, len(head))
		for i := range bold {
			bold[i] = ansiBold
		}
		line(render(head, bold))
		for i, r := range rows {
			codes := make([]string, len(head))
			if !r.Running {
				for c := range codes {
					codes[c] = ansiDim
				}
			}
			if r.State == "needs you" {
				codes[cState] = ansiAmber
			}
			if r.NotAllowed {
				codes[cAccount] = ansiRed
			}
			if c := levelColor(r.FiveHourLevel); c != "" {
				codes[c5h] = c
			}
			if c := levelColor(r.WeekLevel); c != "" {
				codes[cWeek] = c
			}
			line(render(cells[i], codes))
		}
	}

	if len(accts) == 0 {
		return
	}
	line("")
	line(paint("ACCOUNTS", ansiBold))
	nameW, labelW := 0, 0
	for _, a := range accts {
		nameW, labelW = max(nameW, runeLen(a.Name)), max(labelW, runeLen(a.Label))
	}
	for _, a := range accts {
		tail := fmt.Sprintf("5h %s  week %s", a.FiveHour, a.Week)
		if a.Resets != "" {
			tail += "  resets " + a.Resets
		}
		if a.FiveHour == dash && a.Week == dash {
			tail = "usage not seen yet"
		}
		// Keep the usage visible: shorten the label if the line is too long.
		room := width - 2 - nameW - 2 - 2 - runeLen(tail)
		lw := labelW
		if lw > room {
			lw = max(room, 4)
		}
		label := fit(clean(a.Label), lw)
		s := fmt.Sprintf("  %-*s  %s%s  ", nameW, a.Name, label, strings.Repeat(" ", lw-runeLen(label)))
		s = fit(s+tail, width)
		if color && a.Level > 0 && strings.HasSuffix(s, tail) {
			s = strings.TrimSuffix(s, tail) + paint(tail, levelColor(a.Level))
		}
		line(s)
	}
}

type winsize struct{ Row, Col, X, Y uint16 }

// termWidth returns the terminal width of f, or 0 if f is not a terminal.
func termWidth(f *os.File) int {
	var ws winsize
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	if ws.Col == 0 {
		return 80
	}
	return int(ws.Col)
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool { return termWidth(f) > 0 }

// Width picks the table width: the terminal's, else $COLUMNS, else 120.
func Width(f *os.File) int {
	if w := termWidth(f); w > 0 {
		return w
	}
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	return 120
}

// Once prints the table a single time.
func Once(p paths.Paths, out *os.File, all bool) {
	rows, accts := Build(p, time.Now(), all)
	RenderTable(out, rows, accts, Width(out), IsTerminal(out) && os.Getenv("NO_COLOR") == "")
}

// Watch redraws the dashboard until interrupted.
func Watch(p paths.Paths, out *os.File, all bool) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stop)
	color := os.Getenv("NO_COLOR") == ""
	tick := time.NewTicker(redrawEvery)
	defer tick.Stop()
	for {
		rows, accts := Build(p, time.Now(), all)
		var b strings.Builder
		b.WriteString("\x1b[H\x1b[2J")
		RenderTable(&b, rows, accts, Width(out), color)
		fmt.Fprintf(&b, "\n%s", time.Now().Format("15:04:05")+"  ctrl-c to quit")
		io.WriteString(out, b.String())
		select {
		case <-stop:
			fmt.Fprintln(out)
			return
		case <-tick.C:
		}
	}
}
