// Command cs manages Claude Code sessions across several accounts.
package main

import (
	"fmt"
	"os"
)

const version = "0.1.0"

const usage = `usage: cs <command>

  cs                         live dashboard (same as cs status)
  cs status [--once] [--json] [--all]
  cs account add|list|remove [name]
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
	fmt.Fprint(os.Stderr, usage)
	return 2
}
