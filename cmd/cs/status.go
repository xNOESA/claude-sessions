package main

import (
	"os"
	"time"

	"claude-sessions/internal/dash"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/usage"
)

func cmdStatus(p paths.Paths, args []string) int {
	var once, asJSON, all, noRefresh, noBlack bool
	for _, a := range args {
		switch a {
		case "--once":
			once = true
		case "--json":
			asJSON = true
		case "--all":
			all = true
		case "--no-refresh":
			noRefresh = true
		case "--no-black":
			noBlack = true
		default:
			return fail("usage: cs status [--once] [--json] [--all] [--no-refresh] [--no-black]")
		}
	}
	// Ask Claude Code for any account usage older than a few minutes.
	refresh := func() { usage.RefreshStale(p, time.Now(), usage.MaxAge) }
	if noRefresh {
		refresh = nil
	}
	live := !asJSON && !once && dash.IsTerminal(os.Stdout)
	if !live && refresh != nil {
		refresh()
	}
	switch {
	case asJSON:
		rows, accts := dash.Build(p, time.Now(), all)
		if err := dash.RenderJSON(os.Stdout, rows, accts); err != nil {
			return fail(err.Error())
		}
	case once || !dash.IsTerminal(os.Stdout):
		dash.Once(p, os.Stdout, all)
	default:
		dash.Watch(p, os.Stdout, all, refresh, !noBlack)
	}
	return 0
}
