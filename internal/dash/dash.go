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
	"sync/atomic"
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
	Models                                        string // models the session used, with cost
	FiveHourLevel, WeekLevel                      int
	Level                                         int // 0 normal, 1 amber, 2 red
	Running                                       bool
	NotAllowed                                    bool
}

// Bar is one usage meter under an account: a limit window or extra spend.
type Bar struct {
	Label  string
	Pct    float64
	Level  int    // 0 normal, 1 amber, 2 red
	Detail string // when it resets, or the amounts
}

// AccountRow is one registered account and its usage.
type AccountRow struct {
	Name, Label, Org, Plan string
	Seen                   string // how long ago the usage was read, e.g. "4m ago"
	FiveHour, Week, Resets string
	Level                  int
	Active                 int    // running sessions on this account
	Note                   string // shown instead of bars when there is nothing to draw
	Bars                   []Bar
	ByModel                []ModelWindow // what each model cost on this account, per window
}

// ModelWindow is one account's usage by model over one limit window,
// estimated from the session histories on this machine.
type ModelWindow struct {
	Label string // "5h" or "7d"
	Text  string
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
	key    string // the account's usage key, for counting active sessions
}

func levelFor(pct float64) int {
	switch {
	case pct >= redAt:
		return 2
	case pct >= amberAt:
		return 1
	}
	return 0
}

// windowBar describes one limit window: how full, and when it resets.
func windowBar(label string, w store.Window, now time.Time) Bar {
	if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
		return Bar{Label: label, Detail: "reset"} // rolled over since the reading
	}
	b := Bar{Label: label, Pct: w.UsedPct, Level: levelFor(w.UsedPct)}
	if !w.ResetsAt.IsZero() {
		at := w.ResetsAt.In(now.Location())
		layout := "15:04"
		if y, m, d := at.Date(); func() bool { ny, nm, nd := now.Date(); return y != ny || m != nm || d != nd }() {
			layout = "Jan 2 15:04"
		}
		b.Detail = "resets " + untilText(w.ResetsAt.Sub(now)) + " · " + at.Format(layout)
	}
	return b
}

func untilText(d time.Duration) string {
	d = d.Round(time.Minute)
	days, hours, mins := int(d/(24*time.Hour)), int(d/time.Hour)%24, int(d/time.Minute)%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	return fmt.Sprintf("%dm", mins)
}

// dollars formats a cost; anything under a cent is shown as such, not $0.00.
func dollars(v float64) string {
	if v > 0 && v < 0.005 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", v)
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}

// modelUse is one model family's accumulated cost and output tokens.
type modelUse struct {
	cost    float64
	hasCost bool
	out     int64
}

// modelsText lists model families, most expensive first. Costs are what
// Claude Code computes at API prices; approx marks estimated shares.
func modelsText(by map[string]*modelUse, approx bool) string {
	names := make([]string, 0, len(by))
	for n := range by {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := by[names[i]], by[names[j]]
		if a.cost != b.cost {
			return a.cost > b.cost
		}
		if a.out != b.out {
			return a.out > b.out
		}
		return names[i] < names[j]
	})
	mark := ""
	if approx {
		mark = "≈"
	}
	parts := make([]string, 0, len(names))
	for _, n := range names {
		u := by[n]
		if u.hasCost {
			parts = append(parts, n+" "+mark+dollars(u.cost))
		} else {
			parts = append(parts, n+" "+tokens(u.out)+" out")
		}
	}
	return strings.Join(parts, " · ")
}

// sessionModels is the MODELS cell for one session.
func sessionModels(path string) string {
	if path == "" {
		return dash
	}
	by := map[string]*modelUse{}
	for _, t := range transcript.SessionModels(path) {
		u := by[transcript.ModelName(t.Model)]
		if u == nil {
			u = &modelUse{}
			by[transcript.ModelName(t.Model)] = u
		}
		u.cost += t.CostUSD
		u.hasCost = u.hasCost || t.HasCost
		u.out += t.Out
	}
	if len(by) == 0 {
		return dash
	}
	return modelsText(by, false)
}

const (
	fiveHours = 5 * time.Hour
	sevenDays = 7 * 24 * time.Hour
)

// windowStart is when a limit window began: its length before the reset
// when that is known, otherwise that long ago.
func windowStart(w *store.Window, length time.Duration, now time.Time) time.Time {
	if w != nil && w.ResetsAt.After(now) {
		return w.ResetsAt.Add(-length)
	}
	return now.Add(-length)
}

// byModel works out, for each account, what each model cost in its 5-hour
// and 7-day windows. A session's cost per model is known exactly; it is
// shared between requests by their size, and each request goes to the
// account the session was on at the time. Sessions cs never ran are counted
// against the default account, where plain claude runs.
func byModel(sessions map[string]store.Session, transcripts map[string]transcript.Info,
	starts map[string][2]time.Time, defaultKey string, now time.Time) map[string][]ModelWindow {
	acc := map[string]*[2]map[string]*modelUse{}
	for key := range starts {
		acc[key] = &[2]map[string]*modelUse{{}, {}}
	}
	oldest := now.Add(-sevenDays - time.Hour)
	for id, info := range transcripts {
		if info.ModTime.Before(oldest) {
			continue
		}
		reqs := transcript.Requests(info.Path)
		if len(reqs) == 0 {
			continue
		}
		cost := map[string]float64{}
		for _, t := range transcript.SessionModels(info.Path) {
			if t.HasCost {
				cost[t.Model] = t.CostUSD
			}
		}
		weight := map[string]float64{}
		for _, r := range reqs {
			weight[r.Model] += r.Weight()
		}
		s, managed := sessions[id]
		for _, r := range reqs {
			if r.At.After(now) {
				continue
			}
			key := defaultKey
			if managed {
				switch k := s.AccountAt(r.At); {
				case k != "":
					key = k
				case len(s.Spans) == 0 && s.AccountKey != "":
					key = s.AccountKey // recorded before spans existed
				}
			}
			a, ok := acc[key]
			if !ok {
				continue
			}
			for i, start := range starts[key] {
				if r.At.Before(start) {
					continue
				}
				name := transcript.ModelName(r.Model)
				u := a[i][name]
				if u == nil {
					u = &modelUse{}
					a[i][name] = u
				}
				u.out += r.Out
				if c, ok := cost[r.Model]; ok && weight[r.Model] > 0 {
					u.cost += c * r.Weight() / weight[r.Model]
					u.hasCost = true
				}
			}
		}
	}
	out := map[string][]ModelWindow{}
	for key, a := range acc {
		for i, label := range []string{"5h", "7d"} {
			if len(a[i]) > 0 {
				out[key] = append(out[key], ModelWindow{Label: label, Text: modelsText(a[i], true)})
			}
		}
	}
	return out
}

func money(x *store.ExtraUsage) string {
	if x.Currency == "" || strings.EqualFold(x.Currency, "USD") {
		return fmt.Sprintf("$%.2f / $%.2f", x.Used, x.Limit)
	}
	return fmt.Sprintf("%s %.2f / %.2f", strings.ToUpper(x.Currency), x.Used, x.Limit)
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
	states := map[string]store.Session{}
	for _, s := range store.LoadSessions(p) {
		seen[s.ID] = true
		states[s.ID] = s
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
			Ctx: dash, Task: taskCell(s.Tasks), Plan: pi.plan, Models: sessionModels(info.Path)}
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
		key := s.AccountKey
		if key == "" {
			key = s.Identity.Key(s.Profile)
		}
		entries = append(entries, entry{r, active, key})
	}
	for id, info := range transcripts {
		if seen[id] || (!all && now.Sub(info.ModTime) > recentFor) {
			continue
		}
		root := rootOf(info.Cwd)
		r := Row{ID: id, Project: filepath.Base(root), Session: info.Name, State: "unmanaged",
			Account: dash, FiveHour: dash, Week: dash, Ctx: dash, Models: sessionModels(info.Path),
			Task: taskCell(transcript.Tasks(info.Path)), Plan: projOf(root).plan}
		if root == "" {
			r.Project = dash
		}
		if r.Session == "" {
			r.Session = shortID(id)
		}
		entries = append(entries, entry{r, info.ModTime, ""})
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

	active := map[string]int{}
	for _, e := range entries {
		if e.row.Running {
			active[e.key]++
		}
	}
	registered := accounts.List(p)
	starts := map[string][2]time.Time{}
	defaultKey := ""
	for _, a := range registered {
		key := accounts.ResolveIdentity(a.ConfigDir).Key(a.Name)
		u := usage[key]
		starts[key] = [2]time.Time{windowStart(u.FiveHour, fiveHours, now), windowStart(u.SevenDay, sevenDays, now)}
		if a.Name == paths.DefaultAccount {
			defaultKey = key
		}
	}
	models := byModel(states, transcripts, starts, defaultKey, now)

	var accts []AccountRow
	for _, a := range registered {
		id := accounts.ResolveIdentity(a.ConfigDir)
		key := id.Key(a.Name)
		u, have := usage[key]
		ar := AccountRow{Name: a.Name, Label: id.Label(a.Name), Org: id.OrgName, Plan: u.Plan, Active: active[key]}
		var l5, l7 int
		ar.FiveHour, l5 = UsageCell(u.FiveHour, u.SeenAt, now)
		ar.Week, l7 = UsageCell(u.SevenDay, u.SeenAt, now)
		ar.Level = max(l5, l7)
		if u.FiveHour != nil && u.FiveHour.ResetsAt.After(now) {
			ar.Resets = u.FiveHour.ResetsAt.In(now.Location()).Format("15:04")
		}
		if x := u.Extra; x != nil && x.Enabled {
			ar.Bars = append(ar.Bars, Bar{Label: "$$", Pct: x.UsedPct, Level: levelFor(x.UsedPct), Detail: money(x)})
		}
		if u.FiveHour != nil {
			ar.Bars = append(ar.Bars, windowBar("5h", *u.FiveHour, now))
		}
		if u.SevenDay != nil {
			ar.Bars = append(ar.Bars, windowBar("7d", *u.SevenDay, now))
		}
		for _, sc := range u.Scoped {
			ar.Bars = append(ar.Bars, windowBar(sc.Name, sc.Window, now))
		}
		for _, bar := range ar.Bars {
			ar.Level = max(ar.Level, bar.Level)
		}
		switch {
		case len(ar.Bars) > 0:
			seen := u.SeenAt
			if u.FetchedAt.After(seen) {
				seen = u.FetchedAt
			}
			if !seen.IsZero() {
				if age := now.Sub(seen); age < time.Minute {
					ar.Seen = "just now"
				} else {
					ar.Seen = shortAge(age) + " ago"
				}
			}
		case !have:
			ar.Note = "usage not seen yet"
		case u.ProbeError != "":
			ar.Note = "usage unavailable: " + u.ProbeError
		case !u.TriedAt.IsZero():
			ar.Note = "no plan limits reported (signed out, or not a subscription login)"
		default:
			ar.Note = "usage not seen yet"
		}
		ar.ByModel = models[key]
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
	ansiGreen = "\x1b[32m"
	ansiCyan  = "\x1b[36m"
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
		head := []string{"PROJECT", "SESSION", "STATE", "ACCOUNT", "5h", "WEEK", "CTX", "MODELS", "TASK", "PLAN"}
		const (
			cProject = iota
			cSession
			cState
			cAccount
			c5h
			cWeek
			cCtx
			cModels
			cTask
			cPlan
		)
		cells := make([][]string, len(rows))
		widths := make([]int, len(head))
		for i, h := range head {
			widths[i] = runeLen(h)
		}
		for i, r := range rows {
			cells[i] = []string{r.Project, r.Session, r.State, r.Account, r.FiveHour, r.Week, r.Ctx, r.Models, r.Task, r.Plan}
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
		free := []int{cTask, cSession, cAccount, cModels}
		shrink(free, 8)
		if total() > width {
			hidden[cCtx] = true
		}
		shrink(free, 5)
		if total() > width {
			hidden[cModels] = true // still listed per account below
		}
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
	nameW := 0
	for _, a := range accts {
		nameW = max(nameW, runeLen(a.Name))
	}
	for i, a := range accts {
		if i > 0 {
			line("")
		}
		// Header: the name you pass to -a, then who it is signed in as.
		plain := fmt.Sprintf("%-*s  %s", nameW, a.Name, clean(a.Label))
		tail := ""
		if a.Org != "" {
			tail += "  [" + clean(a.Org) + "]"
		}
		if a.Plan != "" {
			if a.Org != "" {
				tail += " · " + a.Plan
			} else {
				tail += "  " + a.Plan
			}
		}
		if a.Seen != "" {
			tail += "  · " + a.Seen
		}
		mark := ""
		if a.Active > 0 {
			mark = "   ● active"
			if a.Active > 1 {
				mark += fmt.Sprintf(" (%d)", a.Active)
			}
		}
		head := fit(plain+tail+mark, width)
		if color && head == plain+tail+mark {
			head = paint(plain, ansiBold) + paint(tail, ansiDim) + paint(mark, ansiCyan)
		}
		line(head)

		modelLines := func() {
			for _, m := range a.ByModel {
				plainLine := "   models " + m.Label + "  " + clean(m.Text)
				if !color || runeLen(plainLine) > width {
					line(fit(plainLine, width))
					continue
				}
				line("   " + paint("models "+m.Label, ansiDim) + "  " + clean(m.Text))
			}
		}
		if len(a.Bars) == 0 {
			if a.Note != "" {
				line(fit("   "+paint(clean(a.Note), ansiDim), width+len(paint("", ansiDim))))
			}
			modelLines()
			continue
		}
		labelW, detailW := 2, 0
		for _, b := range a.Bars {
			labelW, detailW = max(labelW, runeLen(b.Label)), max(detailW, runeLen(b.Detail))
		}
		// indent + label + space, then the bar, then " 100%" and the detail
		barW := width - (3 + labelW + 1) - (1 + 4) - 2 - detailW
		if barW > 40 {
			barW = 40
		}
		if barW < 10 {
			barW = 10
		}
		for _, b := range a.Bars {
			pct := b.Pct
			if pct < 0 {
				pct = 0
			}
			filled := int(pct/100*float64(barW) + 0.5)
			if filled == 0 && pct > 0 {
				filled = 1 // any use at all is visible
			}
			if filled > barW {
				filled = barW
			}
			code := levelColor(b.Level)
			if code == "" {
				code = ansiGreen
			}
			pctText := fmt.Sprintf("%3.0f%%", pct)
			plainLine := fmt.Sprintf("   %-*s %s%s %s  %s", labelW, clean(b.Label),
				strings.Repeat("━", filled), strings.Repeat("─", barW-filled), pctText, b.Detail)
			if !color || runeLen(plainLine) > width {
				line(fit(plainLine, width))
				continue
			}
			line(fmt.Sprintf("   %s %s%s %s  %s", paint(fmt.Sprintf("%-*s", labelW, clean(b.Label)), ansiDim),
				paint(strings.Repeat("━", filled), code), paint(strings.Repeat("─", barW-filled), ansiDim),
				paint(pctText, code), b.Detail))
		}
		modelLines()
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

const (
	ansiBase    = "\x1b[40;97m"                 // black background, bright white text
	ansiEraseEL = "\x1b[K"                      // fill the rest of the line with the background
	ansiEnter   = "\x1b[?1049h\x1b[?25l"        // alternate screen, cursor hidden
	ansiLeave   = "\x1b[0m\x1b[?25h\x1b[?1049l" // restore colours, cursor and the user's screen
)

// OnBlack repaints rendered output on a black background with bright text,
// whatever the terminal's own colours are. Every colour reset in the text
// would fall back to the terminal's background, so each one restores the
// black one, and every line is filled to the right edge.
func OnBlack(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, ansiReset, ansiReset+ansiBase)
	s = strings.ReplaceAll(s, "\n", ansiEraseEL+"\n")
	if !strings.HasSuffix(s, "\n") {
		s += ansiEraseEL
	}
	return ansiBase + s
}

// Watch redraws the dashboard until interrupted. refresh, if set, is run in
// the background (never two at once) to bring account usage up to date; it
// decides for itself whether anything is stale. With black set (and colour
// on) it draws on a black background, on the terminal's alternate screen so
// the user's own screen comes back unchanged on exit.
func Watch(p paths.Paths, out *os.File, all bool, refresh func(), black bool) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(stop)
	color := os.Getenv("NO_COLOR") == ""
	black = black && color
	if black {
		io.WriteString(out, ansiEnter)
		defer io.WriteString(out, ansiLeave)
	}
	tick := time.NewTicker(redrawEvery)
	defer tick.Stop()
	var refreshing atomic.Bool
	for {
		if refresh != nil && refreshing.CompareAndSwap(false, true) {
			go func() {
				defer refreshing.Store(false)
				refresh()
			}()
		}
		rows, accts := Build(p, time.Now(), all)
		var b strings.Builder
		RenderTable(&b, rows, accts, Width(out), color)
		fmt.Fprintf(&b, "\n%s", time.Now().Format("15:04:05")+"  ctrl-c to quit")
		frame := b.String()
		if black {
			// Set the colours first so clearing the screen paints it black.
			frame = ansiBase + "\x1b[H\x1b[2J" + OnBlack(frame)
		} else {
			frame = "\x1b[H\x1b[2J" + frame
		}
		io.WriteString(out, frame)
		select {
		case <-stop:
			if !black {
				fmt.Fprintln(out)
			}
			return
		case <-tick.C:
		}
	}
}
