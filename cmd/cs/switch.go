package main

import (
	"fmt"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/run"
)

func cmdSwitch(p paths.Paths, args []string) int {
	force := false
	var pos []string
	for _, a := range args {
		if a == "--force" {
			force = true
		} else {
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return fail("usage: cs switch <session> <account> [--force]")
	}
	msg, err := run.Switch(p, pos[0], pos[1], force)
	if err != nil {
		return fail(err.Error())
	}
	fmt.Println(msg)
	return 0
}
