package transcript

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

// reqLine is one assistant record as Claude Code writes it: several records
// can share a message id (one per content block) and repeat the same usage.
func reqLine(id, model string, at time.Time, in, out, cacheRead, cacheWrite int) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "sessionId": "s", "timestamp": at.Format("2006-01-02T15:04:05.000Z"),
		"message": map[string]any{"id": id, "model": model, "role": "assistant", "content": []any{},
			"usage": map[string]int{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": cacheRead, "cache_creation_input_tokens": cacheWrite}},
	})
	return string(b) + "\n"
}

func costLine(models map[string]float64) string {
	mu := map[string]any{}
	total := 0.0
	for m, c := range models {
		mu[m] = map[string]any{"costUSD": c, "outputTokens": 1}
		total += c
	}
	b, _ := json.Marshal(map[string]any{"type": "cost-state", "sessionId": "s", "totalCostUSD": total, "modelUsage": mu})
	return string(b) + "\n"
}

func writeSession(t *testing.T, body string, subagents ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "projects", "-p")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "sess.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, sub := range subagents {
		sd := filepath.Join(dir, "sess", "subagents")
		os.MkdirAll(sd, 0o700)
		os.WriteFile(filepath.Join(sd, fmt.Sprintf("agent-%d.jsonl", i)), []byte(sub), 0o600)
	}
	return path
}

func TestRequestsDedupeAndSubagents(t *testing.T) {
	main := reqLine("m1", "claude-fable-5-2", t0, 2, 100, 1000, 50) +
		reqLine("m1", "claude-fable-5-2", t0.Add(time.Second), 2, 100, 1000, 50) + // same response, second block
		`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n" +
		reqLine("m2", "claude-fable-5-2", t0.Add(time.Minute), 3, 200, 2000, 0) +
		reqLine("", "<synthetic>", t0.Add(2*time.Minute), 0, 0, 0, 0) // no real request
	sub := reqLine("a1", "claude-haiku-5-5", t0.Add(30*time.Second), 500, 40, 0, 0)
	got := Requests(writeSession(t, main, sub))
	want := []Request{
		{At: t0, Model: "claude-fable-5-2", In: 2, Out: 100, CacheRead: 1000, CacheWrite: 50},
		{At: t0.Add(30 * time.Second), Model: "claude-haiku-5-5", In: 500, Out: 40},
		{At: t0.Add(time.Minute), Model: "claude-fable-5-2", In: 3, Out: 200, CacheRead: 2000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestRequestsIncremental(t *testing.T) {
	path := writeSession(t, reqLine("m1", "claude-fable-5-2", t0, 1, 10, 0, 0))
	if n := len(Requests(path)); n != 1 {
		t.Fatalf("first read: %d", n)
	}
	// The session keeps writing: one whole line and the start of another.
	next := reqLine("m2", "claude-fable-5-2", t0.Add(time.Minute), 1, 20, 0, 0)
	partial := reqLine("m3", "claude-fable-5-2", t0.Add(2*time.Minute), 1, 30, 0, 0)
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(next + partial[:40])
	f.Close()
	if got := Requests(path); len(got) != 2 || got[1].Out != 20 {
		t.Fatalf("after append: %+v", got)
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(partial[40:])
	f.Close()
	got := Requests(path)
	if len(got) != 3 || got[2].Out != 30 {
		t.Fatalf("after the partial line completed: %+v", got)
	}
	// A rewritten (shorter) file is read afresh, not appended to stale data.
	os.WriteFile(path, []byte(reqLine("z", "claude-haiku-5-5", t0, 1, 5, 0, 0)), 0o600)
	if got := Requests(path); len(got) != 1 || got[0].Model != "claude-haiku-5-5" {
		t.Errorf("after rewrite: %+v", got)
	}
}

func TestSessionModelsFromCostState(t *testing.T) {
	body := reqLine("m1", "claude-fable-5-2", t0, 1, 100, 0, 0) +
		costLine(map[string]float64{"claude-fable-5-2": 1.5}) + // superseded by the later record
		reqLine("m2", "claude-fable-5-2", t0.Add(time.Minute), 1, 200, 0, 0) +
		costLine(map[string]float64{"claude-fable-5-2": 33.8345, "claude-haiku-5-5": 0.0637})
	got := SessionModels(writeSession(t, body))
	want := []ModelTotal{
		{Model: "claude-fable-5-2", CostUSD: 33.8345, HasCost: true, Out: 300},
		{Model: "claude-haiku-5-5", CostUSD: 0.0637, HasCost: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestSessionModelsWithoutCostState(t *testing.T) {
	body := reqLine("m1", "claude-haiku-5-5", t0, 1, 40, 0, 0) + reqLine("m2", "claude-fable-5-2", t0, 1, 300, 0, 0)
	got := SessionModels(writeSession(t, body))
	want := []ModelTotal{{Model: "claude-fable-5-2", Out: 300}, {Model: "claude-haiku-5-5", Out: 40}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v", got)
	}
	if got := SessionModels(writeSession(t, `{"type":"user"}`+"\n")); len(got) != 0 {
		t.Errorf("no requests: %+v", got)
	}
	if got := SessionModels(filepath.Join(t.TempDir(), "missing.jsonl")); len(got) != 0 {
		t.Errorf("missing file: %+v", got)
	}
}

func TestModelName(t *testing.T) {
	for id, want := range map[string]string{
		"claude-fable-5-2": "Fable", "claude-haiku-5-5": "Haiku", "claude-opus-4-1-20250805": "Opus",
		"claude-3-5-sonnet-20241022": "Sonnet", "claude-sonnet-5-5[1m]": "Sonnet", "gpt-x": "gpt-x", "": "?",
	} {
		if got := ModelName(id); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", id, got, want)
		}
	}
}
