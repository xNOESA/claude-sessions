package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Request is one model response recorded in a session's history.
type Request struct {
	At                             time.Time
	Model                          string
	In, Out, CacheRead, CacheWrite int64
}

// Weight is the request's size in input-token price units, using the usual
// price ratios (output 5x, cache write 1.25x, cache read 0.1x). It is only
// used to share a session's known cost between time windows.
func (r Request) Weight() float64 {
	return float64(r.In) + 5*float64(r.Out) + 1.25*float64(r.CacheWrite) + 0.1*float64(r.CacheRead)
}

// ModelTotal is one model's share of a whole session.
type ModelTotal struct {
	Model   string
	CostUSD float64 // as Claude Code itself computed it
	HasCost bool
	Out     int64 // output tokens seen in the history
}

// fileState is what has been read of one history file so far. Sessions
// append to their files, so a later read continues from offset.
type fileState struct {
	offset int64
	order  []string
	byID   map[string]Request
	cost   map[string]float64 // from the latest cost-state record
	anon   int
}

var (
	modelMu    sync.Mutex
	modelCache = map[string]*fileState{}
)

// read brings the state for path up to date and returns it. The caller must
// hold modelMu.
func read(path string) *fileState {
	st, err := os.Stat(path)
	if err != nil {
		delete(modelCache, path)
		return nil
	}
	fs := modelCache[path]
	if fs == nil || st.Size() < fs.offset {
		fs = &fileState{byID: map[string]Request{}}
		modelCache[path] = fs
	}
	if st.Size() == fs.offset {
		return fs
	}
	f, err := os.Open(path)
	if err != nil {
		return fs
	}
	defer f.Close()
	if _, err := f.Seek(fs.offset, io.SeekStart); err != nil {
		return fs
	}
	r := bufio.NewReaderSize(f, 256*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a line still being written is left for the next read
		}
		fs.offset += int64(len(line))
		fs.take(line)
	}
	return fs
}

func (fs *fileState) take(line []byte) {
	isReq := bytes.Contains(line, []byte(`"usage"`))
	isCost := bytes.Contains(line, []byte(`"cost-state"`))
	if !isReq && !isCost {
		return
	}
	var rec struct {
		Type       string `json:"type"`
		Timestamp  string `json:"timestamp"`
		ModelUsage map[string]struct {
			CostUSD float64 `json:"costUSD"`
		} `json:"modelUsage"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				In         int64 `json:"input_tokens"`
				Out        int64 `json:"output_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil {
		return
	}
	if rec.Type == "cost-state" && rec.ModelUsage != nil {
		fs.cost = map[string]float64{}
		for m, u := range rec.ModelUsage {
			fs.cost[m] = u.CostUSD
		}
		return
	}
	m := rec.Message
	if rec.Type != "assistant" || m == nil || m.Usage == nil || m.Model == "" || strings.HasPrefix(m.Model, "<") {
		return
	}
	at, _ := time.Parse(time.RFC3339, rec.Timestamp)
	id := m.ID
	if id == "" {
		fs.anon++
		id = "anon-" + strings.Repeat("x", fs.anon%7) + rec.Timestamp
	}
	req := Request{At: at, Model: m.Model, In: m.Usage.In, Out: m.Usage.Out, CacheRead: m.Usage.CacheRead, CacheWrite: m.Usage.CacheWrite}
	if prev, seen := fs.byID[id]; seen {
		// One response is written once per content block; keep its first
		// time and the latest usage figures.
		req.At = prev.At
	} else {
		fs.order = append(fs.order, id)
	}
	fs.byID[id] = req
}

// sessionFiles are the session's own history plus its subagents'.
func sessionFiles(path string) []string {
	files := []string{path}
	subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
	return append(files, subs...)
}

// Requests returns every model response in a session, subagents included,
// oldest first. Repeated calls only read what was appended since.
func Requests(path string) []Request {
	modelMu.Lock()
	defer modelMu.Unlock()
	var out []Request
	for _, f := range sessionFiles(path) {
		fs := read(f)
		if fs == nil {
			continue
		}
		for _, id := range fs.order {
			out = append(out, fs.byID[id])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// SessionModels returns each model a session used, most expensive first,
// with the cost Claude Code recorded for it when there is one.
func SessionModels(path string) []ModelTotal {
	reqs := Requests(path)
	modelMu.Lock()
	var cost map[string]float64
	if fs := modelCache[path]; fs != nil {
		cost = fs.cost
	}
	modelMu.Unlock()

	by := map[string]*ModelTotal{}
	get := func(m string) *ModelTotal {
		if by[m] == nil {
			by[m] = &ModelTotal{Model: m}
		}
		return by[m]
	}
	for _, r := range reqs {
		get(r.Model).Out += r.Out
	}
	for m, c := range cost {
		t := get(m)
		t.CostUSD, t.HasCost = c, true
	}
	out := make([]ModelTotal, 0, len(by))
	for _, t := range by {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		if a.Out != b.Out {
			return a.Out > b.Out
		}
		return a.Model < b.Model
	})
	return out
}

// ModelName shortens a model id to its family name: claude-fable-5-2 is
// "Fable". Ids it does not recognise are returned unchanged.
func ModelName(id string) string {
	if id == "" {
		return "?"
	}
	base, _, _ := strings.Cut(id, "[")
	if !strings.HasPrefix(base, "claude-") {
		return id
	}
	for _, part := range strings.Split(base, "-")[1:] {
		if part != "" && (part[0] < '0' || part[0] > '9') {
			return strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return id
}
