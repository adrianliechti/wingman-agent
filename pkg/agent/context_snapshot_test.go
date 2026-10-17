package agent_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/agent"
	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

var updateContextSnapshots = flag.Bool("update-context-snapshots", false, "rewrite model-request snapshot fixtures")

// Show the reusable prefix once per window, with explicit reasons whenever
// settings or history invalidate it. Fingerprints keep omitted changes visible.
func contextSnapshot(t *testing.T, requests []contextRequest) string {
	t.Helper()
	var out strings.Builder
	var previousItems []json.RawMessage
	var previousSettings map[string]json.RawMessage
	window := 0
	keys := []string{"model", "stream", "instructions", "reasoning", "tools", "cache_key"}
	for i, req := range requests {
		settings := map[string]json.RawMessage{"instructions": req.Instructions, "reasoning": req.Reasoning, "tools": req.Tools}
		settings["model"], _ = json.Marshal(req.Model)
		settings["stream"], _ = json.Marshal(req.Stream)
		settings["cache_key"], _ = json.Marshal(req.CacheKey)
		var items []json.RawMessage
		if err := json.Unmarshal(req.Input, &items); err != nil {
			items = []json.RawMessage{req.Input}
		}
		var changed []string
		for _, key := range keys {
			if !bytes.Equal(settings[key], previousSettings[key]) {
				changed = append(changed, key)
			}
		}
		common := 0
		for common < len(items) && common < len(previousItems) && bytes.Equal(items[common], previousItems[common]) {
			common++
		}
		var reasons []string
		if i == 0 {
			reasons = append(reasons, "initial request")
		} else {
			if len(changed) > 0 {
				reasons = append(reasons, "settings changed: "+strings.Join(changed, ", "))
			}
			if common < len(previousItems) {
				reasons = append(reasons, fmt.Sprintf("history replaced at item %d", common))
			}
		}
		start := common
		if len(reasons) > 0 {
			window++
			fmt.Fprintf(&out, "\nWindow %d (%s)\n", window, strings.Join(reasons, "; "))
			for _, key := range keys {
				if i == 0 || !bytes.Equal(settings[key], previousSettings[key]) {
					fmt.Fprintf(&out, "%s: %s\n", key, snapshotJSON(t, settings[key]))
				}
			}
			start = 0
		}
		fmt.Fprintf(&out, "Request %d: %d retained, %d shown\n", i+1, start, len(items)-start)
		for j := start; j < len(items); j++ {
			fmt.Fprintf(&out, "[%d] %s\n", j, snapshotJSON(t, items[j]))
		}
		previousItems, previousSettings = items, settings
	}
	return strings.TrimSpace(out.String()) + "\n"
}

func snapshotJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return "null"
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	var normalize func(string, any) any
	normalize = func(key string, value any) any {
		switch value := value.(type) {
		case map[string]any:
			for k, v := range value {
				value[k] = normalize(k, v)
			}
		case []any:
			for i, v := range value {
				value[i] = normalize(key, v)
			}
		case string:
			opaque := key == "encrypted_content" || (key == "image_url" && strings.HasPrefix(value, "data:"))
			if opaque || len(value) > 400 {
				hash := sha256.Sum256([]byte(value))
				label := fmt.Sprintf("<%d bytes; sha256:%x>", len(value), hash[:6])
				if !opaque {
					label += " " + value[:min(80, len(value))] + "…"
				}
				return label
			}
		}
		return value
	}
	encoded, err := json.MarshalIndent(normalize("", value), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestContextRequestSnapshotE2E(t *testing.T) {
	var rounds atomic.Int64
	p := newContextProvider(t, func(w http.ResponseWriter, req contextRequest) {
		if isCheckpointRequest(req) {
			fmt.Fprint(w, completedText("Inspected the repository. Continue verification using updated guidance."))
			return
		}
		if rounds.Add(1) == 1 {
			fmt.Fprint(w, completedToolCall("read-1", "read"))
			return
		}
		fmt.Fprint(w, withUsage(completedText("inspection complete"), 6000, 1))
	})
	guidance := "Initial guidance"
	cfg := e2eConfig(t, tool.Tool{Name: "read", Description: "Read the repository", Execute: func(context.Context, map[string]any) (tool.Result, error) {
		guidance = "Updated guidance"
		return tool.Text("observed source"), nil
	}})
	cfg.CacheKey = "snapshot-session"
	cfg.ContextWindow = 400_000
	cfg.Effort = func() string { return "high" }
	cfg.ContextInstructions = func() string { return guidance }
	a := &agent.Agent{Config: cfg, Messages: []agent.Message{
		{Role: agent.RoleUser, Content: []agent.Content{{Text: "Original task: inspect and verify"}}},
		{Role: agent.RoleAssistant, Content: []agent.Content{{Text: strings.Repeat("Earlier observed progress.\n", 600)}}},
	}}
	if err := drain(t, a, "inspect"); err != nil {
		t.Fatal(err)
	}
	cfg.Effort = func() string { return "low" }
	cfg.Tools = func() []tool.Tool { return []tool.Tool{{Name: "verify", Description: "Verify the result"}} }
	if err := drain(t, a, "prepare verification"); err != nil {
		t.Fatal(err)
	}
	cfg.ContextWindow, cfg.ReserveTokens = 2000, 300
	if err := drain(t, a, "continue after compaction"); err != nil {
		t.Fatal(err)
	}
	requests := p.snapshot()
	if len(requests) != 5 || !isCheckpointRequest(requests[3]) || a.StateSnapshot().ContextRevision == 0 {
		t.Fatalf("expected tool continuation, settings update and compaction; requests=%d revision=%d", len(requests), a.StateSnapshot().ContextRevision)
	}
	got := contextSnapshot(t, requests)
	path := filepath.Join("testdata", "context_requests.golden")
	if *updateContextSnapshots {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("model request snapshot changed; review and run with -update-context-snapshots\n%s", got)
	}
}

func TestContextSnapshotReportsCacheChangesAndOpaquePayloads(t *testing.T) {
	requests := []contextRequest{
		{Model: "model", CacheKey: "first", Input: json.RawMessage(`[{"type":"reasoning","encrypted_content":"secret-a"}]`)},
		{Model: "model", CacheKey: "second", Input: json.RawMessage(`[{"type":"reasoning","encrypted_content":"secret-b"}]`)},
	}
	got := contextSnapshot(t, requests)
	if !strings.Contains(got, "settings changed: cache_key; history replaced at item 0") || strings.Contains(got, "secret-") || strings.Count(got, "sha256:") != 2 {
		t.Fatalf("snapshot hides boundaries or exposes opaque content:\n%s", got)
	}
	if snapshotJSON(t, requests[0].Input) == snapshotJSON(t, requests[1].Input) {
		t.Fatal("omitted payload changes are invisible")
	}
}
