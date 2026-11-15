// Command cs manages Claude Code sessions across several accounts.
package main

import (
	"fmt"
	"os"
	"strings"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/report"
)

const version = "0.1.0"

const helpText = `usage: cs <command>

  cs                         live dashboard (same as cs status)
  cs status [--once] [--json] [--all] [--no-refresh] [--no-black]
  cs account add|login|list|remove [name]
  cs new [-a account] [-n name] [--prompt text] [-- claude flags]
  cs resume [session] [-a account] [--force] [-- claude flags]
  cs switch <session> <account> [--force]
  cs project set <key> <value> | show
  cs version
`

func main() { os.Exit(dispatch(os.Args[1:])) }

func dispatch(args []string) int {
	if len(args) > 0 && args[0] == "version" {
		fmt.Println("cs " + version)
		return 0
	}
	p := paths.Resolve()
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(helpText)
		return 0
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return cmdStatus(p, args) // `cs` and `cs --no-black` are the dashboard
	}
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return cmdStatus(p, args[1:])
		case "help", "-h", "--help":
			fmt.Print(helpText)
			return 0
		case "account":
			return cmdAccount(p, args[1:])
		case "report":
			// Runs inside sessions: never fail, never print errors.
			if len(args) > 1 {
				report.Handle(p, args[1], os.Stdin, os.Stdout, os.Getenv)
			}
			return 0
		case "new":
			return cmdNew(p, args[1:])
		case "resume":
			return cmdResume(p, args[1:])
		case "switch":
			return cmdSwitch(p, args[1:])
		case "project":
			return cmdProject(p, args[1:])
		}
	}
	fmt.Fprint(os.Stderr, helpText)
	return 2
}
