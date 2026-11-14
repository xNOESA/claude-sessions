package main

import (
	"fmt"
	"os"
	"os/exec"

	"claude-sessions/internal/accounts"
	"claude-sessions/internal/paths"
)

func cmdAccount(p paths.Paths, args []string) int {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		for _, a := range accounts.List(p) {
			in := "signed out"
			if accounts.LoggedIn(p, a) {
				in = "signed in"
			}
			fmt.Printf("%-16s %-40s %s\n", a.Name, accounts.ResolveIdentity(a.ConfigDir).Label(a.Name), in)
		}
		return 0
	case "add":
		if len(args) != 2 {
			return fail("usage: cs account add <name>")
		}
		a, err := accounts.Create(p, args[1])
		if err != nil {
			return fail(err.Error())
		}
		fmt.Printf("Created account %q. Signing in:\n", a.Name)
		if err := claudeAuth(p, a, "login"); err != nil {
			return fail(fmt.Sprintf("sign-in did not complete (%v). Run `cs account login %s` to try again.", err, a.Name))
		}
		return 0
	case "login":
		if len(args) != 2 {
			return fail("usage: cs account login <name>")
		}
		a, ok := accounts.Get(p, args[1])
		if !ok {
			return fail(fmt.Sprintf("no account named %q", args[1]))
		}
		if err := accounts.EnsureLinks(p, a); err != nil {
			return fail(err.Error())
		}
		if err := claudeAuth(p, a, "login"); err != nil {
			return fail(err.Error())
		}
		return 0
	case "remove":
		if len(args) != 2 {
			return fail("usage: cs account remove <name>")
		}
		a, ok := accounts.Get(p, args[1])
		if !ok {
			return fail(fmt.Sprintf("no account named %q", args[1]))
		}
		if a.Name == paths.DefaultAccount {
			return fail("the default account is ~/.claude itself and cannot be removed")
		}
		// Sign out first so no login is left behind in the Keychain.
		if err := claudeAuth(p, a, "logout"); err != nil {
			fmt.Fprintf(os.Stderr, "cs: sign-out reported: %v (continuing)\n", err)
		}
		if err := accounts.Delete(p, a.Name); err != nil {
			return fail(err.Error())
		}
		fmt.Printf("Removed account %q.\n", a.Name)
		return 0
	}
	return fail("usage: cs account add|login|list|remove [name]")
}

func claudeAuth(p paths.Paths, a accounts.Account, sub string) error {
	cmd := exec.Command(p.ClaudeBin, "auth", sub)
	cmd.Env = accounts.Environ(p, a, os.Environ())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func fail(msg string) int {
	fmt.Fprintln(os.Stderr, "cs: "+msg)
	return 1
}
