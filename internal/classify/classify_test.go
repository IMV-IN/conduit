package classify

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/yourorg/conduit/internal/canonical"
)

type labeled struct {
	Task   string `json:"task"`
	Prompt string `json:"prompt"`
	Tools  bool   `json:"tools"`
}

// TestFixtureAccuracy runs the classifier over test/datasets/tasks.jsonl.
// The k6 accuracy suite asserts the same threshold live (>0.85); this test
// guards it offline in CI without any running service.
func TestFixtureAccuracy(t *testing.T) {
	raw, err := os.ReadFile("../../test/datasets/tasks.jsonl")
	if err != nil {
		t.Skip("tasks.jsonl not found")
	}
	var items []labeled
	for _, line := range splitLines(string(raw)) {
		var l labeled
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			continue
		}
		items = append(items, l)
	}
	if len(items) == 0 {
		t.Fatal("no labeled items")
	}
	c := New()
	match := 0
	for _, it := range items {
		req := &canonical.Request{
			Tools:    it.Tools,
			Messages: []canonical.Message{{Role: "user", Content: []canonical.Part{{Text: it.Prompt}}}},
			Metadata: map[string]string{},
		}
		got, _ := c.Classify(req)
		if got == it.Task {
			match++
		} else {
			t.Logf("mismatch label=%s got=%s prompt=%.60q", it.Task, got, it.Prompt)
		}
	}
	rate := float64(match) / float64(len(items))
	t.Logf("fixture accuracy: %d/%d = %.3f", match, len(items), rate)
	if rate < 0.85 {
		t.Fatalf("classifier fixture accuracy %.3f below 0.85 gate", rate)
	}
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
