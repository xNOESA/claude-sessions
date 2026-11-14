// Command cs manages Claude Code sessions across several accounts.
package main

import (
	"fmt"
	"os"

	"claude-sessions/internal/paths"
)

const version = "0.1.0"

const usage = `usage: cs <command>

  cs                         live dashboard (same as cs status)
  cs status [--once] [--json] [--all]
  cs account add|login|list|remove [name]
  cs new [-a account] [-n name] [-- claude args]
  cs resume [session] [-a account] [--force] [-- claude args]
  cs switch <session> <account> [--force]
  cs project set <key> <value> | show
  cs version
`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) > 0 && args[0] == "version" {
		fmt.Println("cs " + version)
		return 0
	}
	p := paths.Resolve()
	if len(args) > 0 {
		switch args[0] {
		case "account":
			return cmdAccount(p, args[1:])
		case "project":
			return cmdProject(p, args[1:])
		}
	}
	fmt.Fprint(os.Stderr, usage)
	return 2
}
