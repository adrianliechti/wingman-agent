package acptest

import (
	"context"
	"encoding/json"

	"github.com/coder/acp-go-sdk"
)

// NoElicitation implements the client methods for fixtures without form or URL support.
type NoElicitation struct{}

func (NoElicitation) CreateElicitation(context.Context, acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error) {
	return acp.CreateElicitationResponse{}, acp.NewMethodNotFound("elicitation/create")
}
func (NoElicitation) CompleteElicitation(context.Context, acp.CompleteElicitationNotification) error {
	return nil
}

func Answers(fields map[string]any) map[string]acp.ElicitationContentValue {
	var answers map[string]acp.ElicitationContentValue
	data, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(data, &answers); err != nil {
		panic(err)
	}
	return answers
}

func Value[T any](p acp.Patch[T]) T { value, _ := p.Value(); return value }
