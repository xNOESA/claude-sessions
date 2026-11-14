package main

import (
	"fmt"
	"os"
	"strings"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/project"
)

func cmdProject(p paths.Paths, args []string) int {
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err.Error())
	}
	root := project.Root(cwd)
	switch {
	case len(args) == 1 && args[0] == "show":
		s := project.Load(p, root)
		fmt.Printf("project           %s\n", root)
		fmt.Printf("plan.command      %s\n", s.PlanCommand)
		fmt.Printf("plan.file         %s\n", s.PlanFile)
		fmt.Printf("default_account   %s\n", s.DefaultAccount)
		fmt.Printf("allowed_accounts  %s\n", strings.Join(s.AllowedAccounts, ", "))
		fmt.Printf("launch            %s\n", s.Launch)
		return 0
	case len(args) >= 2 && args[0] == "set":
		value := strings.Join(args[2:], " ")
		if err := project.Set(p, root, args[1], value); err != nil {
			return fail(err.Error())
		}
		if value == "" {
			fmt.Printf("Cleared %s for %s\n", args[1], root)
		} else {
			fmt.Printf("Set %s for %s\n", args[1], root)
		}
		return 0
	}
	return fail("usage: cs project show | cs project set <key> <value>   (keys: " + strings.Join(project.Keys, ", ") + ")")
}
