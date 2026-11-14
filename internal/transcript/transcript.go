// Package transcript reads Claude Code's session history. It is the only
// code that knows the transcript format, which is internal to Claude Code
// and may change: everything here degrades to "unknown" rather than fail.
//
// Format as of Claude Code 2.1.325: <store>/projects/<dir>/<session-id>.jsonl,
// one JSON object per line. Conversation records carry "cwd"; a
// {"type":"custom-title","customTitle":…} record carries the session name;
// tool calls are message.content[] items of type "tool_use".
package transcript

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"claude-sessions/internal/store"
)

// Info describes one session found in the history.
type Info struct {
	ID, Cwd, Name, Path string
	ModTime             time.Time
}

var (
	ErrNotFound  = errors.New("no such session")
	ErrAmbiguous = errors.New("more than one session matches")
)

const (
	headLines = 50
	tailBytes = 64 * 1024
	maxLine   = 16 * 1024 * 1024
)

type record struct {
	Type        string `json:"type"`
	Cwd         string `json:"cwd"`
	CustomTitle string `json:"customTitle"`
	Message     *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type cacheKey struct {
	path string
	mod  int64
	size int64
}

var (
	cacheMu sync.Mutex
	cache   = map[cacheKey]Info{}
)

// List returns every session in the store, newest first.
func List(claudeDir string) []Info {
	files, _ := filepath.Glob(filepath.Join(claudeDir, "projects", "*", "*.jsonl"))
	out := make([]Info, 0, len(files))
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || st.IsDir() {
			continue
		}
		k := cacheKey{f, st.ModTime().UnixNano(), st.Size()}
		cacheMu.Lock()
		info, ok := cache[k]
		cacheMu.Unlock()
		if !ok {
			info = Info{ID: strings.TrimSuffix(filepath.Base(f), ".jsonl"), Path: f, ModTime: st.ModTime()}
			info.Cwd, info.Name = headAndTail(f, st.Size())
			cacheMu.Lock()
			cache[k] = info
			cacheMu.Unlock()
		}
		out = append(out, info)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out
}

// headAndTail reads the working directory from the first lines and the most
// recent session name from the first lines or the end of the file.
func headAndTail(path string, size int64) (cwd, name string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	scan := func(r io.Reader, limit int) {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), maxLine)
		for n := 0; sc.Scan() && (limit == 0 || n < limit); n++ {
			var rec record
			if json.Unmarshal(sc.Bytes(), &rec) != nil {
				continue
			}
			if cwd == "" && rec.Cwd != "" {
				cwd = rec.Cwd
			}
			if rec.Type == "custom-title" && rec.CustomTitle != "" {
				name = rec.CustomTitle
			}
		}
	}
	scan(f, headLines)
	if size > tailBytes {
		if _, err := f.Seek(size-tailBytes, io.SeekStart); err == nil {
			scan(f, 0) // the first, partial line fails to parse and is skipped
		}
	}
	return cwd, name
}

// Find resolves a session by exact id, unique id prefix, or exact name.
func Find(claudeDir, ref string) (Info, error) {
	if ref == "" {
		return Info{}, ErrNotFound
	}
	all := List(claudeDir)
	for _, i := range all {
		if i.ID == ref {
			return i, nil
		}
	}
	var hits []Info
	for _, i := range all {
		if i.Name == ref {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		for _, i := range all {
			if strings.HasPrefix(i.ID, ref) {
				hits = append(hits, i)
			}
		}
	}
	switch len(hits) {
	case 0:
		return Info{}, ErrNotFound
	case 1:
		return hits[0], nil
	}
	return Info{}, ErrAmbiguous
}

// RecentWindow is how recently a transcript must have been written for the
// session to be treated as possibly open elsewhere.
const RecentWindow = 60 * time.Second

// RecentlyWritten reports whether the transcript changed within RecentWindow.
func RecentlyWritten(path string, now time.Time) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	age := now.Sub(st.ModTime())
	return age < RecentWindow
}

type task struct {
	subject, status string
	deleted         bool
}

// Tasks replays the task tool calls in a transcript and summarises the
// session's to-do list. Known is false when there is no list or the calls
// do not add up.
func Tasks(path string) store.TaskProgress {
	f, err := os.Open(path)
	if err != nil {
		return store.TaskProgress{}
	}
	defer f.Close()

	var list []task
	seen, broken := false, false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if !strings.Contains(string(line), `"tool_use"`) {
			continue
		}
		var rec record
		if json.Unmarshal(line, &rec) != nil || rec.Message == nil {
			continue
		}
		var items []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(rec.Message.Content, &items) != nil {
			continue
		}
		for _, it := range items {
			if it.Type != "tool_use" {
				continue
			}
			switch it.Name {
			case "TodoWrite":
				var in struct {
					Todos []struct{ Content, Status string } `json:"todos"`
				}
				if json.Unmarshal(it.Input, &in) != nil {
					continue
				}
				seen, broken = true, false
				list = list[:0]
				for _, td := range in.Todos {
					list = append(list, task{subject: td.Content, status: td.Status})
				}
			case "TaskCreate":
				var in struct {
					Subject string `json:"subject"`
				}
				if json.Unmarshal(it.Input, &in) != nil {
					continue
				}
				seen = true
				list = append(list, task{subject: in.Subject, status: "pending"})
			case "TaskUpdate":
				var in struct {
					TaskID  json.RawMessage `json:"taskId"`
					Status  string          `json:"status"`
					Subject string          `json:"subject"`
				}
				if json.Unmarshal(it.Input, &in) != nil {
					continue
				}
				seen = true
				// Task ids are 1-based, in creation order.
				n, err := strconv.Atoi(strings.Trim(string(in.TaskID), `"`))
				if err != nil || n < 1 || n > len(list) {
					broken = true
					continue
				}
				t := &list[n-1]
				if in.Subject != "" {
					t.subject = in.Subject
				}
				switch in.Status {
				case "":
				case "deleted":
					t.deleted = true
				default:
					t.status = in.Status
				}
			}
		}
	}
	if !seen || broken {
		return store.TaskProgress{}
	}
	p := store.TaskProgress{Known: true}
	for _, t := range list {
		if t.deleted {
			continue
		}
		p.Total++
		switch t.status {
		case "completed":
			p.Done++
		case "in_progress":
			if p.Current == "" {
				p.Current = t.subject
			}
		}
	}
	if p.Total == 0 {
		return store.TaskProgress{}
	}
	return p
}
