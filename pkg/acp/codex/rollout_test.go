package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRolloutOutputs(t *testing.T) {

	lines := []string{
		`{"type":"session_meta","payload":{"id":"019e7aeb","cwd":"/x"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{}","call_id":"call_A"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_A","output":"hello\nworld"}}`,
		`{"type":"event_msg","payload":{"type":"token_count"}}`,
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call_B","output":"second"}}`,
		`not json`,
	}
	got := parseRolloutOutputs(strings.NewReader(strings.Join(lines, "\n")))
	if got["call_A"] != "hello\nworld" {
		t.Errorf("call_A = %q", got["call_A"])
	}
	if got["call_B"] != "second" {
		t.Errorf("call_B = %q", got["call_B"])
	}
	if len(got) != 2 {
		t.Errorf("expected 2 outputs, got %d: %v", len(got), got)
	}
}

func TestDecodeRolloutOutput(t *testing.T) {
	if got := decodeRolloutOutput([]byte(`"plain string"`)); got != "plain string" {
		t.Errorf("string = %q", got)
	}

	if got := decodeRolloutOutput([]byte(`{"x":1}`)); got != `{"x":1}` {
		t.Errorf("object = %q", got)
	}
	if got := decodeRolloutOutput(nil); got != "" {
		t.Errorf("empty = %q", got)
	}

	shell := mustJSONString(t, "Chunk ID: bd92ed\nWall time: 0.05 seconds\nProcess exited with code 0\nOutput:\nfile_a.go\nfile_b.go")
	if got := decodeRolloutOutput(shell); got != "file_a.go\nfile_b.go" {
		t.Errorf("shell = %q", got)
	}

	mcp := mustJSONString(t, "Wall time: 0.26 seconds\nOutput:\n[{\"type\":\"text\",\"text\":\"Found 5 results\"},{\"type\":\"text\",\"text\":\"line two\"}]")
	if got := decodeRolloutOutput(mcp); got != "Found 5 results\nline two" {
		t.Errorf("mcp = %q", got)
	}
}

func mustJSONString(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFindRolloutFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "2026", "05", "31")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "019e7aeb-870c-78d2-91b1-40f715ef0e79"
	want := filepath.Join(dir, "rollout-2026-05-31T00-05-16-"+id+".jsonl")
	if err := os.WriteFile(want, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	_ = os.WriteFile(filepath.Join(dir, "rollout-other.jsonl"), []byte("{}"), 0o644)

	if got := findRolloutFile(root, id); got != want {
		t.Errorf("findRolloutFile = %q, want %q", got, want)
	}
	if got := findRolloutFile(root, "missing-id"); got != "" {
		t.Errorf("expected empty for missing id, got %q", got)
	}
}
