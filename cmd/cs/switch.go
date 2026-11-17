package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"claude-sessions/internal/dash"
	"claude-sessions/internal/paths"
	"claude-sessions/internal/run"
)

func cmdSwitch(p paths.Paths, args []string) int {
	var o run.SwitchOpts
	var pos []string
	for _, a := range args {
		switch a {
		case "--force":
			o.Force = true
		case "--compact":
			o.Compact = true
		case "--yes", "-y":
			o.AllowCrossOrg = true
		default:
			if strings.HasPrefix(a, "-") {
				return fail("usage: cs switch <session> <account> [--compact] [--yes] [--force]")
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return fail("usage: cs switch <session> <account> [--compact] [--yes] [--force]")
	}
	msg, err := run.Switch(p, pos[0], pos[1], o)
	var cross *run.CrossOrgError
	if errors.As(err, &cross) && dash.IsTerminal(os.Stdin) {
		// Nothing has been changed yet; let the user decide here.
		fmt.Fprintln(os.Stderr, "cs: "+err.Error())
		fmt.Fprint(os.Stderr, "Compact first, switch as it is, or cancel? [c/s/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "c", "compact":
			o.Compact = true
		case "s", "switch", "y", "yes":
			o.AllowCrossOrg = true
		default:
			fmt.Fprintln(os.Stderr, "Not switched.")
			return 1
		}
		msg, err = run.Switch(p, pos[0], pos[1], o)
	}
	if err != nil {
		return fail(err.Error())
	}
	if o.Compact {
		msg += " (compacting first if the organization changes)"
	}
	fmt.Println(msg)
	return 0
}
