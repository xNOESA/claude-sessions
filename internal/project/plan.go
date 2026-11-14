package project

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"claude-sessions/internal/paths"
	"claude-sessions/internal/store"
)

// Plan is a project's overall progress.
type Plan struct {
	Done, Total int
	Label       string
	Known       bool
}

const (
	planCommandTTL     = 60 * time.Second
	planCommandTimeout = 5 * time.Second
)

var checkboxRE = regexp.MustCompile(`^\s*[-*]\s+\[([ xX])\]`)

// CountCheckboxes counts markdown task items. Items inside fenced code
// blocks are counted too; plans rarely have them and it keeps this simple.
func CountCheckboxes(r io.Reader) (done, total int) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		m := checkboxRE.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		total++
		if m[1] != " " {
			done++
		}
	}
	return done, total
}

func fromFile(path string) (Plan, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Plan{}, false
	}
	defer f.Close()
	done, total := CountCheckboxes(f)
	if total == 0 {
		return Plan{}, false
	}
	return Plan{Done: done, Total: total, Label: fmt.Sprintf("%d/%d", done, total), Known: true}, true
}

// Progress finds the project's plan: a configured command, a configured
// file, then conventional locations. Known is false when there is none.
func Progress(p paths.Paths, root string, s Settings, now time.Time) Plan {
	if s.PlanCommand != "" {
		if pl := fromCommand(p, root, s.PlanCommand, now); pl.Known {
			return pl
		}
		return Plan{}
	}
	if s.PlanFile != "" {
		f := s.PlanFile
		if !filepath.IsAbs(f) {
			f = filepath.Join(root, f)
		}
		pl, _ := fromFile(f)
		return pl
	}
	if m, _ := filepath.Glob(filepath.Join(root, "docs", "superpowers", "plans", "*.md")); len(m) > 0 {
		sort.Slice(m, func(i, j int) bool { return modTime(m[i]).After(modTime(m[j])) })
		if pl, ok := fromFile(m[0]); ok {
			return pl
		}
	}
	for _, n := range []string{"PLAN.md", "TODO.md", filepath.Join("docs", "plan.md")} {
		if pl, ok := fromFile(filepath.Join(root, n)); ok {
			return pl
		}
	}
	return Plan{}
}

func modTime(path string) time.Time {
	st, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

type cachedPlan struct {
	Command string
	At      time.Time
	Plan    Plan
}

func fromCommand(p paths.Paths, root, command string, now time.Time) Plan {
	sum := sha1.Sum([]byte(root))
	cache := filepath.Join(p.PlansDir(), hex.EncodeToString(sum[:])+".json")
	var c cachedPlan
	if store.ReadJSON(cache, &c) == nil && c.Command == command {
		if age := now.Sub(c.At); age >= 0 && age < planCommandTTL {
			return c.Plan
		}
	}
	pl := runPlanCommand(root, command)
	_ = store.WriteJSON(cache, cachedPlan{Command: command, At: now, Plan: pl})
	return pl
}

func runPlanCommand(root, command string) Plan {
	ctx, cancel := context.WithTimeout(context.Background(), planCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout = &out
	// Kill the whole group on timeout so a child holding the pipe cannot
	// keep us waiting.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return Plan{}
	}
	line, _, _ := strings.Cut(out.String(), "\n")
	f := strings.Fields(line)
	if len(f) < 2 {
		return Plan{}
	}
	done, err1 := strconv.Atoi(f[0])
	total, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || done < 0 || total <= 0 || done > total {
		return Plan{}
	}
	label := fmt.Sprintf("%d/%d", done, total)
	if len(f) > 2 {
		label = strings.Join(f[2:], " ") + " " + label
	}
	return Plan{Done: done, Total: total, Label: label, Known: true}
}
