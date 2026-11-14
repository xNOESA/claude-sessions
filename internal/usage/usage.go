// Package usage asks Claude Code for an account's plan usage. It starts
// `claude` under the account's config directory and sends the get_usage
// control request, which answers from the claude.ai usage endpoint without
// a model call. cs itself never touches the account's login.
package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/store"
)

var timeout = 20 * time.Second

// MaxAge is how old a probe may be before the dashboard asks again.
const MaxAge = 5 * time.Minute

const requestID = "cs-usage"

type apiWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func (w *apiWindow) toStore() *store.Window {
	if w == nil || w.Utilization == nil {
		return nil
	}
	out := &store.Window{UsedPct: *w.Utilization}
	if w.ResetsAt != nil {
		if t, err := time.Parse(time.RFC3339, *w.ResetsAt); err == nil {
			out.ResetsAt = t
		}
	}
	return out
}

type answer struct {
	SubscriptionType    *string `json:"subscription_type"`
	RateLimitsAvailable bool    `json:"rate_limits_available"`
	RateLimits          *struct {
		FiveHour       *apiWindow `json:"five_hour"`
		SevenDay       *apiWindow `json:"seven_day"`
		SevenDayOpus   *apiWindow `json:"seven_day_opus"`
		SevenDaySonnet *apiWindow `json:"seven_day_sonnet"`
		ModelScoped    []struct {
			DisplayName string `json:"display_name"`
			apiWindow
		} `json:"model_scoped"`
		ExtraUsage *struct {
			IsEnabled    bool     `json:"is_enabled"`
			MonthlyLimit *float64 `json:"monthly_limit"`
			UsedCredits  *float64 `json:"used_credits"`
			Utilization  *float64 `json:"utilization"`
			Currency     *string  `json:"currency"`
		} `json:"extra_usage"`
	} `json:"rate_limits"`
}

// Fetch asks Claude Code, running as the account, for its current usage.
func Fetch(p paths.Paths, a accounts.Account) (store.Usage, error) {
	u := store.Usage{Identity: accounts.ResolveIdentity(a.ConfigDir), Profile: a.Name}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.ClaudeBin,
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--no-session-persistence", "--settings", `{"disableAllHooks":true}`)
	cmd.Env = accounts.Environ(p, a, os.Environ())
	// A neutral directory: no project settings, hooks or trust prompt apply.
	if err := os.MkdirAll(p.Home, 0o700); err == nil {
		cmd.Dir = p.Home
	}
	cmd.Stdin = strings.NewReader(`{"type":"control_request","request_id":"` + requestID + `","request":{"subtype":"get_usage","skip_behaviors":true}}` + "\n")
	cmd.WaitDelay = 500 * time.Millisecond
	// Its own process group, so a timeout also stops anything claude started
	// (which would otherwise hold the output pipe open).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	killGroup := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.Cancel = killGroup
	pr, pw, err := os.Pipe()
	if err != nil {
		return u, err
	}
	cmd.Stdout = pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return u, err
	}
	pw.Close()
	stop := context.AfterFunc(ctx, func() { pr.Close() }) // unblock the reader on timeout
	ans, perr := readAnswer(pr)
	stop()
	pr.Close()
	// We have what we came for (or gave up); do not wait for claude to wind down.
	_ = killGroup()
	werr := cmd.Wait()
	if perr != nil {
		if ctx.Err() != nil {
			return u, errors.New("claude did not answer the usage request in time")
		}
		if werr != nil {
			return u, fmt.Errorf("%v (%v)", perr, werr)
		}
		return u, perr
	}

	if ans.SubscriptionType != nil {
		u.Plan = *ans.SubscriptionType
	}
	u.Available = ans.RateLimitsAvailable && ans.RateLimits != nil
	if rl := ans.RateLimits; rl != nil {
		u.FiveHour, u.SevenDay = rl.FiveHour.toStore(), rl.SevenDay.toStore()
		for _, m := range rl.ModelScoped {
			if w := m.apiWindow.toStore(); w != nil && m.DisplayName != "" {
				u.Scoped = append(u.Scoped, store.ScopedWindow{Name: m.DisplayName, Window: *w})
			}
		}
		for _, m := range []struct {
			name string
			w    *apiWindow
		}{{"Opus", rl.SevenDayOpus}, {"Sonnet", rl.SevenDaySonnet}} {
			if w := m.w.toStore(); w != nil && !hasScoped(u.Scoped, m.name) {
				u.Scoped = append(u.Scoped, store.ScopedWindow{Name: m.name, Window: *w})
			}
		}
		if e := rl.ExtraUsage; e != nil && e.IsEnabled {
			x := &store.ExtraUsage{Enabled: true}
			// Amounts arrive in minor units (cents).
			if e.UsedCredits != nil {
				x.Used = *e.UsedCredits / 100
			}
			if e.MonthlyLimit != nil {
				x.Limit = *e.MonthlyLimit / 100
			}
			if e.Utilization != nil {
				x.UsedPct = *e.Utilization
			} else if x.Limit > 0 {
				x.UsedPct = x.Used / x.Limit * 100
			}
			if e.Currency != nil {
				x.Currency = *e.Currency
			}
			u.Extra = x
		}
	}
	return u, nil
}

func hasScoped(list []store.ScopedWindow, name string) bool {
	for _, s := range list {
		if strings.EqualFold(s.Name, name) {
			return true
		}
	}
	return false
}

func readAnswer(r interface{ Read([]byte) (int, error) }) (answer, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8<<20)
	for sc.Scan() {
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string          `json:"subtype"`
				RequestID string          `json:"request_id"`
				Error     string          `json:"error"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != requestID {
			continue
		}
		if msg.Response.Subtype != "success" {
			return answer{}, fmt.Errorf("claude refused the usage request: %s", msg.Response.Error)
		}
		var a answer
		if err := json.Unmarshal(msg.Response.Response, &a); err != nil {
			return answer{}, fmt.Errorf("unreadable usage answer: %v", err)
		}
		return a, nil
	}
	return answer{}, errors.New("claude exited without answering the usage request (is this Claude Code version too old?)")
}

// RefreshStale probes every account whose last attempt is older than maxAge,
// in parallel, and stores the results. A failed probe keeps the last good
// reading and records why it failed.
func RefreshStale(p paths.Paths, now time.Time, maxAge time.Duration) {
	stored := map[string]store.Usage{}
	for _, u := range store.LoadUsage(p) {
		stored[u.Identity.Key(u.Profile)] = u
	}
	var wg sync.WaitGroup
	for _, a := range accounts.List(p) {
		key := accounts.ResolveIdentity(a.ConfigDir).Key(a.Name)
		prev, had := stored[key]
		if had && !prev.TriedAt.IsZero() && now.Sub(prev.TriedAt) < maxAge {
			continue
		}
		wg.Add(1)
		go func(a accounts.Account, prev store.Usage, had bool) {
			defer wg.Done()
			fresh, err := Fetch(p, a)
			merged := fresh
			if had {
				merged = prev
				merged.Identity, merged.Profile = fresh.Identity, fresh.Profile
			}
			merged.TriedAt = now
			if err != nil {
				merged.ProbeError = err.Error()
				store.Logf(p, "usage %s: %v", a.Name, err)
			} else {
				merged.ProbeError = ""
				merged.FetchedAt = now
				merged.Plan, merged.Available = fresh.Plan, fresh.Available
				merged.Scoped, merged.Extra = fresh.Scoped, fresh.Extra
				// Keep a status-line reading when the probe has none.
				if fresh.FiveHour != nil {
					merged.FiveHour = fresh.FiveHour
				}
				if fresh.SevenDay != nil {
					merged.SevenDay = fresh.SevenDay
				}
				if fresh.FiveHour != nil || fresh.SevenDay != nil || !had {
					merged.SeenAt = now
				}
			}
			if err := store.SaveUsage(p, merged); err != nil {
				store.Logf(p, "usage %s: save: %v", a.Name, err)
			}
		}(a, prev, had)
	}
	wg.Wait()
}
