package pi

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/adrianliechti/wingman-agent/pkg/acp/internal/acptest"
)

func TestACPContract(t *testing.T) {
	acptest.Run(t, newContractAgent)
}

func newContractAgent(t *testing.T) acptest.Agent {
	t.Helper()
	script, dir, env := acptest.CommandHelper(t, "TestPiContractHelper", "PI_CONTRACT_HELPER")
	return New(Options{Path: script, Dir: dir, Env: env})
}

func TestPiContractHelper(t *testing.T) {
	if os.Getenv("PI_CONTRACT_HELPER") != "1" {
		return
	}
	runPiContractHelper()
	os.Exit(0)
}

func runPiContractHelper() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil || req.ID == "" {
			continue
		}
		switch req.Type {
		case "get_available_models":
			writePiResponse(req.ID, map[string]any{"models": []any{map[string]any{"provider": "contract", "id": "model", "name": "Contract Model"}}})
		case "get_state":
			writePiResponse(req.ID, map[string]any{
				"sessionId":     "00000000-0000-4000-8000-000000000003",
				"thinkingLevel": "medium",
				"model":         map[string]any{"provider": "contract", "id": "model"},
			})
		case "set_model", "set_thinking_level", "abort":
			writePiResponse(req.ID, map[string]any{})
		case "prompt":
			writePiDelta("")
			if strings.Contains(req.Message, acptest.CancelPrompt) {
				writePiDelta(acptest.CancelText)
				continue
			}
			writePiDelta(acptest.NormalText)
			writePiContract(map[string]any{
				"type": "tool_execution_start", "toolCallId": "00000000-0000-4000-8000-000000000004",
				"toolName": "bash", "args": map[string]any{"command": "pwd"},
			})
			writePiContract(map[string]any{
				"type": "tool_execution_end", "toolCallId": "00000000-0000-4000-8000-000000000004",
				"result": "contract output", "isError": false,
			})
			writePiContract(map[string]any{"type": "agent_end"})
			writePiResponse(req.ID, map[string]any{})
		case "get_messages":
			writePiResponse(req.ID, map[string]any{"messages": []any{}})
		default:
			writePiContract(map[string]any{"type": "response", "id": req.ID, "success": false, "error": "unsupported"})
		}
	}
}

func writePiDelta(text string) {
	writePiContract(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "text_delta", "delta": text},
	})
}

func writePiResponse(id string, data any) {
	writePiContract(map[string]any{"type": "response", "id": id, "success": true, "data": data})
}

func writePiContract(value any) {
	b, _ := json.Marshal(value)
	_, _ = os.Stdout.Write(append(b, '\n'))
}
