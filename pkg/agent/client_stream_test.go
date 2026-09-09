package agent

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestSendStreamsAndRetainsRefusal(t *testing.T) {
	const refusal = "I can’t help with that."
	const item = `{"type":"message","id":"msg_refusal","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"refusal","refusal":"I can’t help with that."}]}`
	for _, deltas := range [][]string{nil, {"I can’t"}, {"I can’t", " help with that."}} {
		t.Run(fmt.Sprintf("deltas=%d", len(deltas)), func(t *testing.T) {
			requests := 0
			client := streamingTestClient(func(*http.Request) string {
				requests++
				var body strings.Builder
				for i, delta := range deltas {
					fmt.Fprintf(&body, "data: {\"type\":\"response.refusal.delta\",\"sequence_number\":%d,\"item_id\":\"msg_refusal\",\"output_index\":0,\"content_index\":0,\"delta\":%q}\n\n", i, delta)
				}
				body.WriteString(phaseTestResponse(false, item))
				return body.String()
			})
			a := &Agent{Config: &Config{client: &client, MaxTurns: 2}}
			stream, err := a.Send(t.Context(), []Content{{Text: "start"}})
			if err != nil {
				t.Fatal(err)
			}
			var visible strings.Builder
			for message, err := range stream {
				if err != nil {
					t.Fatal(err)
				}
				for _, content := range message.Content {
					visible.WriteString(content.AsText())
					if content.TextID != "msg_refusal" {
						t.Errorf("streamed identity = %q", content.TextID)
					}
				}
			}
			if visible.String() != refusal || requests != 1 {
				t.Fatalf("visible=%q requests=%d; refusal must appear once and end the turn", visible.String(), requests)
			}
			retained := a.Messages[len(a.Messages)-1]
			if len(retained.Content) != 1 || retained.Content[0].Refusal != refusal || retained.Content[0].TextID != "msg_refusal" || retained.Phase != PhaseCommentary {
				t.Fatalf("retained refusal = %+v", retained)
			}
			replay := toInput([]Message{retained})
			if len(replay) != 1 || replay[0].OfOutputMessage == nil {
				t.Fatalf("replayed refusal = %+v", replay)
			}
			part := replay[0].OfOutputMessage.Content[0]
			if part.OfOutputText != nil || part.OfRefusal == nil || part.OfRefusal.Refusal != refusal {
				t.Fatalf("refusal lost its provider type on replay: %+v", part)
			}
		})
	}
}

type closeTrackedBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackedBody) Close() error {
	b.closed = true
	return nil
}

func TestCompleteClosesStreamOnEarlyExit(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		stopYield  bool
		wantError  bool
	}{
		{name: "completed", body: phaseTestResponse(false, finalAnswerOutput)},
		{name: "incomplete", body: strings.ReplaceAll(phaseTestResponse(false, commentaryOutput), "response.completed", "response.incomplete")},
		{name: "failed", body: "data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"failed\"}}}\n\n", wantError: true},
		{name: "consumer stopped", body: "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg\",\"delta\":\"partial\"}\n\n", stopYield: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &closeTrackedBody{Reader: strings.NewReader(tc.body)}
			client := openai.NewClient(option.WithAPIKey("test"), option.WithHTTPClient(&http.Client{
				Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
				}),
			}))
			_, err := complete(t.Context(), &client, &request{}, func(Message, error) bool { return !tc.stopYield })
			if (err != nil) != tc.wantError || (tc.stopYield && !errors.Is(err, errYieldStopped)) {
				t.Fatalf("unexpected completion error: %v", err)
			}
			if !body.closed {
				t.Error("response body was not closed after leaving the stream loop")
			}
		})
	}
}
