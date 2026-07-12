package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/pkg/code"
)

func TestSessionSteerCorrelatesWithActiveCodexTurn(t *testing.T) {
	agentIO, serverIO := net.Pipe()
	t.Cleanup(func() {
		_ = agentIO.Close()
		_ = serverIO.Close()
	})

	rpc := newRPCClient(agentIO, agentIO)
	client := newCodexClient(rpc)
	rpc.start()
	s := newSession("thread-1", "", "", nil)
	s.currentTurnID = "turn-7"

	params := make(chan turnSteerParams, 1)
	go func() {
		scanner := bufio.NewScanner(serverIO)
		if !scanner.Scan() {
			return
		}
		var request rpcMessage
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return
		}
		var p turnSteerParams
		if request.Method != "turn/steer" || json.Unmarshal(request.Params, &p) != nil {
			return
		}
		params <- p
		result, _ := json.Marshal(turnSteerResponse{TurnID: "turn-7"})
		response, _ := json.Marshal(rpcMessage{Jsonrpc: "2.0", ID: request.ID, Result: result})
		_, _ = serverIO.Write(append(response, '\n'))
	}()

	err := s.steer(context.Background(), client, []acp.ContentBlock{acp.TextBlock("guide")}, "message-9")
	if err != nil {
		t.Fatal(err)
	}
	got := <-params
	if got.ThreadID != "thread-1" || got.ExpectedTurnID != "turn-7" || got.ClientUserMessageID != "message-9" {
		t.Fatalf("steer params = %#v", got)
	}
	if len(got.Input) != 1 {
		t.Fatalf("steer input = %#v", got.Input)
	}
	b, _ := json.Marshal(got.Input[0])
	var input struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b, &input) != nil || input.Type != "text" || input.Text != "guide" {
		t.Fatalf("steer input = %s", b)
	}
}

func TestSessionSteerRequiresActiveTurn(t *testing.T) {
	s := newSession("thread-1", "", "", nil)
	if err := s.steer(context.Background(), nil, []acp.ContentBlock{acp.TextBlock("guide")}, "message-9"); !errors.Is(err, code.ErrNoActiveTurn) {
		t.Fatalf("steer error = %v", err)
	}
}

func TestClassifySteerError(t *testing.T) {
	if err := classifySteerError(&rpcError{Code: -32600, Message: "no active turn to steer"}); !errors.Is(err, code.ErrNoActiveTurn) {
		t.Fatalf("no-active error = %v", err)
	}
	if err := classifySteerError(&rpcError{Code: -32600, Message: "cannot steer a review turn"}); !errors.Is(err, code.ErrTurnNotSteerable) {
		t.Fatalf("non-steerable error = %v", err)
	}
	original := &rpcError{Code: -32600, Message: "expected active turn id `old` but found `new`"}
	if err := classifySteerError(original); !errors.Is(err, code.ErrNoActiveTurn) {
		t.Fatalf("turn mismatch error = %v", err)
	}
}
