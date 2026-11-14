// Package report records what a running session tells us through Claude
// Code's hooks and status line. It runs inside the session (and inside any
// sandbox around it), so it must be fast and must never fail the session.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/store"
	"claude-sessions/internal/transcript"
)

// hookEvents maps Claude Code hook names to `cs report` events.
var hookEvents = [][2]string{
	{"SessionStart", "session-start"},
	{"UserPromptSubmit", "prompt"},
	{"Stop", "stop"},
	{"Notification", "notification"},
	{"SessionEnd", "session-end"},
	{"TaskCreated", "tasks"},
	{"TaskCompleted", "tasks"},
}

// ShellQuote quotes s as one POSIX shell word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// InjectedSettings is the settings JSON passed to claude with --settings so
// the session reports to cs. The user's own settings are not edited.
func InjectedSettings(csBin string) []byte {
	type hook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
	}
	type matcher struct {
		Hooks []hook `json:"hooks"`
	}
	bin := ShellQuote(csBin)
	hooks := map[string][]matcher{}
	for _, e := range hookEvents {
		hooks[e[0]] = []matcher{{Hooks: []hook{{Type: "command", Command: bin + " report " + e[1]}}}}
	}
	b, _ := json.MarshalIndent(map[string]any{
		"hooks":      hooks,
		"statusLine": hook{Type: "command", Command: bin + " report statusline"},
	}, "", "  ")
	return b
}

// UserStatusLine returns the status line command from the user's shared
// settings, so cs can keep showing it.
func UserStatusLine(claudeDir string) string {
	var d struct {
		StatusLine struct {
			Command string `json:"command"`
		} `json:"statusLine"`
	}
	_ = store.ReadJSON(claudeDir+"/settings.json", &d)
	return d.StatusLine.Command
}

type input struct {
	SessionID      string `json:"session_id"`
	SessionName    string `json:"session_name"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Model          struct {
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Cost struct {
		TotalCostUSD *float64 `json:"total_cost_usd"`
	} `json:"cost"`
	ContextWindow struct {
		UsedPercentage *float64 `json:"used_percentage"`
	} `json:"context_window"`
	RateLimits *struct {
		FiveHour *window `json:"five_hour"`
		SevenDay *window `json:"seven_day"`
	} `json:"rate_limits"`
}

type window struct {
	UsedPercentage float64 `json:"used_percentage"`
	ResetsAt       float64 `json:"resets_at"`
}

func (w *window) toStore() *store.Window {
	if w == nil {
		return nil
	}
	out := &store.Window{UsedPct: w.UsedPercentage}
	if w.ResetsAt > 0 {
		out.ResetsAt = time.Unix(int64(w.ResetsAt), 0)
	}
	return out
}

// Handle processes one report. It does nothing outside a cs-run session,
// swallows every error (logging it), and writes to stdout only for the
// status line.
func Handle(p paths.Paths, event string, stdin io.Reader, stdout io.Writer, getenv func(string) string) {
	if getenv("CS_SESSION") != "1" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			store.Logf(p, "report %s: panic: %v", event, r)
		}
	}()
	raw, _ := io.ReadAll(io.LimitReader(stdin, 8<<20))
	var in input
	if err := json.Unmarshal(raw, &in); err != nil || in.SessionID == "" {
		store.Logf(p, "report %s: unusable input (%v)", event, err)
		if event == "statusline" {
			chainOrPrint(p, raw, stdout, getenv, "")
		}
		return
	}

	if event == "statusline" {
		line := statusline(p, in, getenv)
		chainOrPrint(p, raw, stdout, getenv, line)
		return
	}

	var signalPID int
	err := store.UpdateSession(p, in.SessionID, func(s *store.Session) {
		common(s, in, getenv)
		switch event {
		case "session-start":
			s.State = store.StateIdle
		case "prompt":
			s.State = store.StateWorking
		case "stop":
			s.State = store.StateIdle
		case "notification":
			// A notification while working is usually a permission prompt.
			s.State = store.StateNeedsYou
		case "session-end":
			s.State = store.StateEnded
		}
		if (event == "stop" || event == "tasks") && s.TranscriptPath != "" {
			s.Tasks = transcript.Tasks(s.TranscriptPath)
		}
		if event == "stop" && s.SwitchTo != "" && s.WrapperPID > 0 {
			signalPID = s.WrapperPID
		}
	})
	if err != nil {
		store.Logf(p, "report %s: %v", event, err)
		return
	}
	if signalPID > 0 {
		// Tell the wrapper the turn is over so a pending switch can go ahead.
		_ = syscall.Kill(signalPID, syscall.SIGUSR1)
	}
}

func common(s *store.Session, in input, getenv func(string) string) {
	if in.Cwd != "" && s.Cwd == "" {
		s.Cwd = in.Cwd
	}
	if in.TranscriptPath != "" {
		s.TranscriptPath = in.TranscriptPath
	}
	if s.Profile == "" {
		s.Profile = getenv("CS_PROFILE")
	}
}

func statusline(p paths.Paths, in input, getenv func(string) string) string {
	cfg := getenv("CLAUDE_CONFIG_DIR")
	if cfg == "" {
		cfg = p.ClaudeDir
	}
	id := accounts.ResolveIdentity(cfg)
	var profile string
	err := store.UpdateSession(p, in.SessionID, func(s *store.Session) {
		common(s, in, getenv)
		if in.SessionName != "" {
			s.Name = in.SessionName
		}
		if in.Model.DisplayName != "" {
			s.Model = in.Model.DisplayName
		}
		if in.ContextWindow.UsedPercentage != nil {
			s.ContextPct = in.ContextWindow.UsedPercentage
		}
		if in.Cost.TotalCostUSD != nil {
			s.CostUSD = in.Cost.TotalCostUSD
		}
		s.Identity = id
		s.AccountKey = id.Key(s.Profile)
		profile = s.Profile
	})
	if err != nil {
		store.Logf(p, "report statusline: %v", err)
		profile = getenv("CS_PROFILE")
	}

	parts := []string{id.Label(profile)}
	if in.RateLimits != nil && (in.RateLimits.FiveHour != nil || in.RateLimits.SevenDay != nil) {
		u := store.Usage{Identity: id, Profile: profile}
		_ = store.ReadJSON(p.UsageFile(id.Key(profile)), &u) // keep a window this report omits
		u.Identity, u.Profile, u.SeenAt = id, profile, time.Now()
		if w := in.RateLimits.FiveHour.toStore(); w != nil {
			u.FiveHour = w
			parts = append(parts, fmt.Sprintf("5h %.0f%%", w.UsedPct))
		}
		if w := in.RateLimits.SevenDay.toStore(); w != nil {
			u.SevenDay = w
			parts = append(parts, fmt.Sprintf("wk %.0f%%", w.UsedPct))
		}
		if err := store.SaveUsage(p, u); err != nil {
			store.Logf(p, "report statusline: usage: %v", err)
		}
	}
	return strings.Join(parts, " · ")
}

// chainOrPrint shows the user's own status line if they had one, otherwise
// cs's line.
func chainOrPrint(p paths.Paths, raw []byte, stdout io.Writer, getenv func(string) string, line string) {
	if user := getenv("CS_USER_STATUSLINE"); user != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", user)
		cmd.Stdin = bytes.NewReader(raw)
		cmd.Stderr = nil
		cmd.Env = os.Environ()
		cmd.WaitDelay = 200 * time.Millisecond
		out, err := cmd.Output()
		if err == nil && len(bytes.TrimSpace(out)) > 0 {
			_, _ = stdout.Write(out)
			return
		}
		if err != nil {
			store.Logf(p, "report statusline: user command: %v", err)
		}
	}
	if line != "" {
		fmt.Fprintln(stdout, line)
	}
}
