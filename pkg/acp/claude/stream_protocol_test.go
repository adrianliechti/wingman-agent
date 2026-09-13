package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/coder/acp-go-sdk"
)

const rootUsageFrame = `{"type":"assistant","parent_tool_use_id":null,"message":{"id":"root","model":"claude-sonnet-5","usage":{"input_tokens":100,"cache_read_input_tokens":900,"output_tokens":50},"content":[{"type":"text","text":"done"}]}}`
const cumulativeResultFrame = `{"type":"result","subtype":"success","total_cost_usd":0.25,"usage":{"input_tokens":100000,"cache_read_input_tokens":200000,"output_tokens":20000},"modelUsage":{"claude-sonnet-5":{"contextWindow":200000},"claude-opus-5":{"contextWindow":1000000}}}`

func readTranscriptUpdates(t *testing.T, wire []byte) []acp.SessionUpdate {
	t.Helper()
	var updates []acp.SessionUpdate
	scanner := bufio.NewScanner(bytes.NewReader(wire))
	for scanner.Scan() {
		var frame struct {
			Method string                  `json:"method"`
			Params acp.SessionNotification `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatal(err)
		}
		if frame.Method == "session/update" {
			updates = append(updates, frame.Params.Update)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return updates
}

func newTranscriptProcess(t *testing.T) (*claudeProc, *acp.AgentSideConnection, *bytes.Buffer) {
	t.Helper()
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	wire := &bytes.Buffer{}
	a := New(Options{Stderr: io.Discard})
	conn := acp.NewAgentSideConnection(a, wire, reader)
	p := &claudeProc{
		session: a.newSession("s", t.TempDir(), "sonnet", "", nil),
		models:  []ModelEntry{{ID: "sonnet", ResolvedModel: "claude-sonnet-5"}},
		results: make(chan turnResult, 1), dead: make(chan struct{}),
		emitted: newToolCallTracker(), tools: toolUseCache{},
		streamedContent: &streamedBlockTracker{},
	}
	return p, conn, wire
}

func captureCLITranscript(t *testing.T, ctx context.Context, lines ...string) ([]acp.SessionUpdate, turnResult) {
	t.Helper()
	p, conn, wire := newTranscriptProcess(t)
	p.beginTurn(ctx)
	p.read(context.Background(), conn, "s", strings.NewReader(strings.Join(lines, "\n")+"\n"))
	var result turnResult
	select {
	case result = <-p.results:
	default:
	}
	return readTranscriptUpdates(t, wire.Bytes()), result
}

func transcriptText(updates []acp.SessionUpdate) string {
	var text strings.Builder
	for _, update := range updates {
		if chunk := update.AgentMessageChunk; chunk != nil && chunk.Content.Text != nil {
			text.WriteString(chunk.Content.Text.Text)
		}
	}
	return text.String()
}

func TestContextUsageUsesLatestRootResponse(t *testing.T) {
	for _, tt := range []struct {
		name   string
		frames []string
		used   int
	}{
		{"consolidated", []string{rootUsageFrame}, 1050},
		{"nested tool", []string{rootUsageFrame, `{"type":"assistant","parent_tool_use_id":"child","message":{"model":"claude-opus-5","usage":{"input_tokens":500000},"content":[]}}`}, 1050},
		{"nested agent", []string{rootUsageFrame, `{"type":"assistant","parent_agent_id":"child","message":{"model":"claude-opus-5","usage":{"input_tokens":500000},"content":[]}}`}, 1050},
		{"synthetic", []string{rootUsageFrame, `{"type":"assistant","message":{"model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0},"content":[]}}`}, 1050},
		{"streamed", []string{
			`{"type":"stream_event","event":{"type":"message_start","message":{"model":"claude-sonnet-5","usage":{"input_tokens":100,"cache_read_input_tokens":900,"cache_creation_input_tokens":20}}}}`,
			`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":25}}}`,
			`{"type":"stream_event","event":{"type":"message_delta","usage":{"output_tokens":50}}}`,
		}, 1070},
	} {
		t.Run(tt.name, func(t *testing.T) {
			updates, result := captureCLITranscript(t, context.Background(), append(tt.frames, cumulativeResultFrame)...)
			var usage *acp.SessionUsageUpdate
			for _, update := range updates {
				if update.UsageUpdate != nil {
					usage = update.UsageUpdate
				}
			}
			if usage == nil || usage.Used != tt.used || usage.Size != 200000 {
				t.Fatalf("context usage=%+v, want %d/200000", usage, tt.used)
			}
			if usage.Cost == nil || usage.Cost.Amount != 0.25 {
				t.Fatalf("cost=%+v", usage.Cost)
			}
			if result.usage == nil || result.usage.TotalTokens != 320000 {
				t.Fatalf("prompt usage=%+v", result.usage)
			}
		})
	}
}

func TestUnknownContextUsageDoesNotUseCumulativeTokens(t *testing.T) {
	for _, frames := range [][]string{
		{cumulativeResultFrame},
		{`{"type":"assistant","message":{"model":"custom-model","usage":{"input_tokens":10},"content":[]}}`, cumulativeResultFrame},
		{`{"type":"assistant","message":{"model":"claude-sonnet-5","content":[]}}`, cumulativeResultFrame},
	} {
		updates, _ := captureCLITranscript(t, context.Background(), frames...)
		for _, update := range updates {
			if update.UsageUpdate != nil {
				t.Fatalf("fabricated context usage: %+v", update.UsageUpdate)
			}
		}
	}
}

func TestCompactionRefreshesContextUsageImmediately(t *testing.T) {
	for _, used := range []int{12000, 0} {
		boundary, _ := json.Marshal(map[string]any{"type": "system", "subtype": "compact_boundary", "compact_metadata": map[string]any{"post_tokens": used}})
		updates, _ := captureCLITranscript(t, context.Background(), rootUsageFrame, cumulativeResultFrame, string(boundary))
		last := updates[len(updates)-1].UsageUpdate
		if last == nil || last.Used != used || last.Size != 200000 {
			t.Fatalf("compacted usage=%+v, want %d/200000", last, used)
		}
	}
}

func TestResultOnlyAnswerFallback(t *testing.T) {
	const cached = `{"type":"result","subtype":"success","result":"cached answer","usage":{"output_tokens":0}}`
	for _, tt := range []struct {
		name   string
		frames []string
		want   string
	}{
		{"cached", []string{cached}, "cached answer"},
		{"missing usage", []string{`{"type":"result","subtype":"success","result":"cached answer"}`}, "cached answer"},
		{"duplicate result", []string{cached, cached}, "cached answer"},
		{"already streamed", []string{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"streamed answer"}}}`, cached}, "streamed answer"},
		{"already consolidated", []string{rootUsageFrame, cached}, "done"},
		{"local output", []string{`{"type":"system","subtype":"local_command_output","content":"command output"}`, cached}, "command output"},
		{"thought only", []string{`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"thinking"}}}`, cached}, "cached answer"},
		{"generated result", []string{`{"type":"result","subtype":"success","result":"background prose","usage":{"output_tokens":10}}`}, ""},
		{"stale UUID", []string{`{"type":"result","subtype":"success","user_message_uuid":"previous-turn","result":"stale","usage":{"output_tokens":0}}`}, ""},
		{"error", []string{`{"type":"result","subtype":"success","is_error":true,"result":"failed","usage":{"output_tokens":0}}`}, ""},
		{"compaction result", []string{`{"type":"system","subtype":"status","status":"compacting"}`, cached}, "Compacting context...\n\n"},
		{"compaction block", []string{`{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"compaction"}}}`, cached}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			updates, _ := captureCLITranscript(t, context.Background(), tt.frames...)
			if got := transcriptText(updates); got != tt.want {
				t.Fatalf("text=%q, want %q", got, tt.want)
			}
		})
	}
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		updates, _ := captureCLITranscript(t, ctx, cached)
		if got := transcriptText(updates); got != "" {
			t.Fatalf("cancelled turn emitted %q", got)
		}
	})
}

func TestResultFallbackResetsForNextPrompt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, conn, wire := newTranscriptProcess(t)
		reader, writer := io.Pipe()
		defer reader.Close()
		defer writer.Close()
		go p.read(context.Background(), conn, "s", reader)
		p.beginTurn(context.Background())
		oldID := p.turnID
		_, _ = io.WriteString(writer, rootUsageFrame+"\n"+cumulativeResultFrame+"\n")
		<-p.results
		p.beginTurn(context.Background())
		stale, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "user_message_uuid": oldID, "result": "stale answer"})
		_, _ = writer.Write(append(stale, '\n'))
		synctest.Wait()
		select {
		case <-p.results:
			t.Fatal("stale result finished the next turn")
		default:
		}
		current, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "user_message_uuid": p.turnID, "result": "next answer"})
		_, _ = writer.Write(append(current, '\n'))
		<-p.results
		synctest.Wait()
		if got := transcriptText(readTranscriptUpdates(t, wire.Bytes())); got != "donenext answer" {
			t.Fatalf("transcript=%q", got)
		}
	})
}
