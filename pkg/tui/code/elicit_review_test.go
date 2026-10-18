package code

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type elicitOutcome struct {
	result tool.ElicitResult
	err    error
}

func startTestElicitation(app *App, t *testing.T, field tool.ElicitField) <-chan elicitOutcome {
	t.Helper()
	done := make(chan elicitOutcome, 1)
	go func() {
		result, err := app.Elicit(t.Context(), tool.ElicitRequest{Message: "Choose", Fields: []tool.ElicitField{field}})
		done <- elicitOutcome{result, err}
	}()
	select {
	case fn := <-app.queue:
		fn()
	case <-time.After(time.Second):
		t.Fatal("prompt did not open")
	}
	return done
}

func awaitTestElicitation(app *App, t *testing.T, done <-chan elicitOutcome) tool.ElicitResult {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case fn := <-app.queue:
			fn()
		case got := <-done:
			if got.err != nil {
				t.Fatal(got.err)
			}
			return got.result
		case <-deadline:
			t.Fatal("prompt did not finish")
		}
	}
}

func TestElicitOptionLabelsCannotBecomeUIActions(t *testing.T) {
	for _, label := range []string{"__other", "__decline"} {
		t.Run(label, func(t *testing.T) {
			app, _ := newQueueTestApp(t, 0)
			done := startTestElicitation(app, t, tool.ElicitField{Name: "choice", Type: "string", Enum: []string{label, "ordinary"}})
			app.popup.onAccept([]string{app.popup.items[0].ID})
			got := awaitTestElicitation(app, t, done)
			if got.Action != tool.ElicitAccept || got.Content["choice"] != label {
				t.Fatalf("label became control action: %+v", got)
			}
		})
	}
}

func TestElicitOtherPreservesSelectionsAndLiteralText(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "multiple"}[multiple], func(t *testing.T) {
			app, _ := newQueueTestApp(t, 0)
			done := startTestElicitation(app, t, tool.ElicitField{Name: "choice", Type: "string", Required: true, Multiple: multiple, Enum: []string{"API", "CLI"}})
			ids := []string{"__other"}
			if multiple {
				ids = append(ids, app.popup.items[0].ID)
			}
			app.popup.onAccept(ids)
			deadline := time.After(time.Second)
			for !app.askActive {
				select {
				case fn := <-app.queue:
					fn()
				case <-deadline:
					t.Fatal("Other input did not open")
				}
			}
			text := "1"
			var want any = text
			if multiple {
				text = "docs, tests"
				want = []string{"API", text}
			}
			app.editor.SetText(text)
			app.answerPrompt()
			got := awaitTestElicitation(app, t, done)
			if got.Action != tool.ElicitAccept || !reflect.DeepEqual(got.Content["choice"], want) {
				t.Fatalf("Other lost answer: %+v, want %#v", got, want)
			}
		})
	}
}

func TestQueuedElicitationCanBeCancelled(t *testing.T) {
	app, _ := newQueueTestApp(t, 0)
	if err := app.acquirePrompt(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer app.releasePrompt()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := app.Elicit(ctx, tool.ElicitRequest{Message: "Queued"}); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled elicitation blocked behind another prompt")
	}
	if _, ok := parseElicitValue(tool.ElicitField{Type: "string", Required: true}, " "); ok {
		t.Fatal("required empty answer accepted")
	}
}
