package main

import (
	"os"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/run"
)

// parseLaunch reads `[session] [-a account] [-n name] [--force] [-- claude args]`.
func parseLaunch(args []string) (o run.Opts, positional []string, ok bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.ClaudeArgs = args[i+1:]
			return o, positional, true
		case a == "-a" || a == "--account":
			if i+1 >= len(args) {
				return o, nil, false
			}
			i++
			o.Account = args[i]
		case a == "-n" || a == "--name":
			if i+1 >= len(args) {
				return o, nil, false
			}
			i++
			o.Name = args[i]
		case a == "--force":
			o.Force = true
		case len(a) > 0 && a[0] == '-':
			return o, nil, false
		default:
			positional = append(positional, a)
		}
	}
	return o, positional, true
}

func runner(p paths.Paths) (*run.Runner, int) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fail(err.Error())
	}
	self, err := os.Executable()
	if err != nil {
		return nil, fail(err.Error())
	}
	return &run.Runner{P: p, Cwd: cwd, CSBin: self, Environ: os.Environ(),
		Stdio: run.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}}, 0
}

func cmdNew(p paths.Paths, args []string) int {
	o, pos, ok := parseLaunch(args)
	if !ok || len(pos) != 0 {
		return fail("usage: cs new [-a account] [-n name] [-- claude args]")
	}
	r, code := runner(p)
	if r == nil {
		return code
	}
	return r.New(o)
}

func cmdResume(p paths.Paths, args []string) int {
	o, pos, ok := parseLaunch(args)
	if !ok || len(pos) > 1 {
		return fail("usage: cs resume [session] [-a account] [--force] [-- claude args]")
	}
	if len(pos) == 1 {
		o.SessionRef = pos[0]
	}
	r, code := runner(p)
	if r == nil {
		return code
	}
	return r.Resume(o)
}
