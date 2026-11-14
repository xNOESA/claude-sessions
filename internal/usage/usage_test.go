package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/store"
)

const fullAnswer = `{"session":{"total_cost_usd":0},"subscription_type":"max","rate_limits_available":true,"rate_limits":{
 "five_hour":{"utilization":16,"resets_at":"2026-11-14T00:10:00Z"},
 "seven_day":{"utilization":52.4,"resets_at":"2026-11-15T03:59:00Z"},
 "seven_day_opus":null,
 "seven_day_sonnet":{"utilization":7,"resets_at":"2026-11-15T03:59:00Z"},
 "model_scoped":[{"display_name":"Fable","utilization":85,"resets_at":"2026-11-15T04:00:00Z"},{"display_name":"Ghost","utilization":null,"resets_at":null}],
 "extra_usage":{"is_enabled":true,"monthly_limit":4000,"used_credits":2199,"utilization":54.975,"currency":"USD"}},"behaviors":null}`

type world struct {
	t   *testing.T
	p   paths.Paths
	log string
}

func newWorld(t *testing.T) *world {
	d := t.TempDir()
	fake, _ := filepath.Abs(filepath.Join("..", "..", "testdata", "fakeclaude"))
	p := paths.Paths{Home: filepath.Join(d, "cs home"), ClaudeDir: filepath.Join(d, "home", ".claude"), ClaudeBin: fake}
	os.MkdirAll(p.ClaudeDir, 0o700)
	w := &world{t, p, filepath.Join(d, "fake.log")}
	t.Setenv("FAKE_LOG", w.log)
	w.account("default", "d@acme.com", fullAnswer)
	return w
}

func (w *world) account(name, email, answer string) accounts.Account {
	w.t.Helper()
	a, ok := accounts.Get(w.p, name)
	if !ok {
		var err error
		if a, err = accounts.Create(w.p, name); err != nil {
			w.t.Fatal(err)
		}
	}
	b, _ := json.Marshal(map[string]any{"oauthAccount": map[string]string{"emailAddress": email, "accountUuid": "acc-" + name, "organizationUuid": "org-" + name, "organizationName": "Org " + name}})
	os.WriteFile(filepath.Join(a.ConfigDir, ".claude.json"), b, 0o600)
	if answer != "" {
		os.WriteFile(filepath.Join(a.ConfigDir, "usage.json"), []byte(strings.ReplaceAll(answer, "\n", "")), 0o600)
	}
	return a
}

func (w *world) probes() []string {
	b, _ := os.ReadFile(w.log)
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "USAGE|") {
			out = append(out, l)
		}
	}
	return out
}

func TestFetchParsesEverything(t *testing.T) {
	w := newWorld(t)
	a, _ := accounts.Get(w.p, "default")
	u, err := Fetch(w.p, a)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Available || u.Plan != "max" || u.Identity.Email != "d@acme.com" || u.Profile != "default" {
		t.Errorf("header = %+v", u)
	}
	if u.FiveHour == nil || u.FiveHour.UsedPct != 16 || !u.FiveHour.ResetsAt.Equal(time.Date(2026, 11, 14, 0, 10, 0, 0, time.UTC)) {
		t.Errorf("five hour = %+v", u.FiveHour)
	}
	if u.SevenDay == nil || u.SevenDay.UsedPct != 52.4 {
		t.Errorf("seven day = %+v", u.SevenDay)
	}
	// Per-model windows: the server's labelled ones, plus Sonnet; a null
	// Opus window and a model with no reading are left out.
	var names []string
	for _, s := range u.Scoped {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "Fable,Sonnet" || u.Scoped[0].UsedPct != 85 || !u.Scoped[0].ResetsAt.Equal(time.Date(2026, 11, 15, 4, 0, 0, 0, time.UTC)) {
		t.Errorf("scoped = %+v", u.Scoped)
	}
	if u.Extra == nil || !u.Extra.Enabled || u.Extra.Used != 21.99 || u.Extra.Limit != 40 || u.Extra.UsedPct != 54.975 || u.Extra.Currency != "USD" {
		t.Errorf("extra = %+v", u.Extra)
	}
}

func TestFetchRunsUnderTheAccountWithoutCredentialEnvOrHooks(t *testing.T) {
	w := newWorld(t)
	work := w.account("work", "w@other.org", fullAnswer)
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if _, err := Fetch(w.p, work); err != nil {
		t.Fatal(err)
	}
	p := w.probes()
	if len(p) != 1 || !strings.Contains(p[0], "CONFIG="+work.ConfigDir+"|") || !strings.Contains(p[0], "APIKEY=|") {
		t.Fatalf("probe = %v", p)
	}
	for _, want := range []string{"ARGV=-p ", "--output-format stream-json", "--no-session-persistence", "disableAllHooks"} {
		if !strings.Contains(p[0]+" ", want) {
			t.Errorf("probe args missing %q: %s", want, p[0])
		}
	}
}

func TestFetchNoPlanLimits(t *testing.T) {
	w := newWorld(t)
	a := w.account("work", "w@other.org", "") // fake answers "rate limits unavailable"
	u, err := Fetch(w.p, a)
	if err != nil {
		t.Fatal(err)
	}
	if u.Available || u.FiveHour != nil || u.Extra != nil || len(u.Scoped) != 0 {
		t.Errorf("got %+v", u)
	}
}

func TestFetchTimesOut(t *testing.T) {
	w := newWorld(t)
	t.Setenv("FAKE_USAGE_HANG", "1")
	old := timeout
	timeout = 400 * time.Millisecond
	defer func() { timeout = old }()
	a, _ := accounts.Get(w.p, "default")
	start := time.Now()
	if _, err := Fetch(w.p, a); err == nil {
		t.Error("a hung probe returned no error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}

func TestRefreshStaleOnlyFetchesOldAccounts(t *testing.T) {
	w := newWorld(t)
	w.account("work", "w@other.org", fullAnswer)
	now := time.Now()

	RefreshStale(w.p, now, 5*time.Minute)
	if n := len(w.probes()); n != 2 {
		t.Fatalf("first refresh probed %d accounts, want 2", n)
	}
	us := store.LoadUsage(w.p)
	if len(us) != 2 || us[0].Plan != "max" || us[0].FetchedAt.IsZero() {
		t.Fatalf("stored = %+v", us)
	}
	RefreshStale(w.p, now.Add(4*time.Minute), 5*time.Minute)
	if n := len(w.probes()); n != 2 {
		t.Errorf("fresh accounts were probed again (%d probes)", n)
	}
	RefreshStale(w.p, now.Add(6*time.Minute), 5*time.Minute)
	if n := len(w.probes()); n != 4 {
		t.Errorf("stale accounts not refreshed (%d probes)", n)
	}
}

func TestRefreshKeepsLastGoodDataWhenProbeFails(t *testing.T) {
	w := newWorld(t)
	now := time.Now()
	RefreshStale(w.p, now, 5*time.Minute)
	t.Setenv("FAKE_USAGE_HANG", "1")
	old := timeout
	timeout = 300 * time.Millisecond
	defer func() { timeout = old }()
	RefreshStale(w.p, now.Add(10*time.Minute), 5*time.Minute)
	u := store.LoadUsage(w.p)[0]
	if u.FiveHour == nil || u.FiveHour.UsedPct != 16 || u.Extra == nil {
		t.Errorf("a failed probe wiped the last reading: %+v", u)
	}
	if u.ProbeError == "" {
		t.Error("the failure was not recorded")
	}
	// And it is not retried on every redraw.
	n := len(w.probes())
	RefreshStale(w.p, now.Add(11*time.Minute), 5*time.Minute)
	if len(w.probes()) != n {
		t.Error("a failing account was probed again within the interval")
	}
}

func TestRefreshKeepsStatusLineReadingWhenProbeHasNoLimits(t *testing.T) {
	w := newWorld(t)
	a := w.account("work", "w@other.org", "")
	id := accounts.ResolveIdentity(a.ConfigDir)
	store.SaveUsage(w.p, store.Usage{Identity: id, Profile: "work", SeenAt: time.Now(), FiveHour: &store.Window{UsedPct: 33}})
	RefreshStale(w.p, time.Now(), 5*time.Minute)
	for _, u := range store.LoadUsage(w.p) {
		if u.Profile == "work" && (u.FiveHour == nil || u.FiveHour.UsedPct != 33) {
			t.Errorf("status line reading lost: %+v", u)
		}
	}
}
