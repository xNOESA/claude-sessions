package main

import (
	"os"
	"time"

	"claude-sessions/internal/dash"
	"claude-sessions/internal/paths"
)

func cmdStatus(p paths.Paths, args []string) int {
	var once, asJSON, all bool
	for _, a := range args {
		switch a {
		case "--once":
			once = true
		case "--json":
			asJSON = true
		case "--all":
			all = true
		default:
			return fail("usage: cs status [--once] [--json] [--all]")
		}
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
		dash.Watch(p, os.Stdout, all)
	}
	return 0
}
