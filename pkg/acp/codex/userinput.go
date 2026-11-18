package codex

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/adrianliechti/wingman-agent/internal/acpjson"
)

const (
	userInputOtherOption = "None of the above"
	userInputNotePrefix  = "user_note: "
)

// handleUserInput renders Codex's request_user_input tool as one form
// elicitation. Without form support, or when the user does not accept, Codex
// receives no answers and continues on its own.
func (a *approver) handleUserInput(p userInputParams) userInputResponse {
	if a.client.Elicitation == nil || a.client.Elicitation.Form == nil || len(p.Questions) == 0 {
		return emptyUserInputResponse()
	}
	ctx := a.ctx
	if p.AutoResolutionMs != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(max(0, *p.AutoResolutionMs))*time.Millisecond)
		defer cancel()
	}
	meta := map[string]any{}
	meta["codex"] = map[string]any{"autoResolutionMs": p.AutoResolutionMs}
	resp, err := callClient(ctx, a.conn, func() (acp.CreateElicitationResponse, error) {
		return a.conn.CreateElicitation(ctx, acp.CreateElicitationRequest{
			Form: &acp.CreateElicitationForm{
				Session: &acp.ElicitationSessionScope{SessionId: a.sessionID, ToolCallId: new(acp.ToolCallId(p.ItemID))}, Meta: acpjson.Metadata(meta),
				Message:         "Codex needs your input to continue.",
				Mode:            "form",
				RequestedSchema: userInputSchema(p.Questions),
			},
		})
	})
	if err != nil || ctx.Err() != nil || resp.Accept == nil {
		return emptyUserInputResponse()
	}
	return userInputAnswers(p.Questions, acpjson.Object(resp.Accept.Content))
}

func userInputSchema(questions []userInputQuestion) acp.ElicitationSchema {
	ids := questionIDs(questions)
	properties := map[string]acp.ElicitationPropertySchema{}
	required := make([]string, 0, len(questions))
	for _, q := range questions {
		title := q.Question
		if title == "" {
			title = q.Header
		}
		if title == "" {
			title = q.ID
		}
		field := &acp.ElicitationPropertySchemaString{Title: &title, Meta: acpjson.Metadata(map[string]any{"codex": map[string]any{"isOther": q.IsOther, "isSecret": q.IsSecret}})}
		if q.Header != "" {
			field.Description = &q.Header
		}
		for _, o := range q.Options {
			opt := acp.EnumOption{Const: o.Label, Title: o.Label}
			if o.Description != "" {
				opt.Description = &o.Description
			}
			field.OneOf = append(field.OneOf, opt)
		}
		properties[q.ID] = acp.ElicitationPropertySchema{String: field}
		required = append(required, q.ID)
		if q.IsOther && len(q.Options) > 0 {
			if !slices.ContainsFunc(q.Options, func(o userInputOption) bool { return o.Label == userInputOtherOption }) {
				field.OneOf = append(field.OneOf, acp.EnumOption{Const: userInputOtherOption, Title: userInputOtherOption, Description: new("Provide a different answer in the note field.")})
			}
			properties[userInputNoteFieldID(q.ID, ids)] = acp.ElicitationPropertySchema{String: &acp.ElicitationPropertySchemaString{Title: new("Additional answer or note"), Meta: acpjson.Metadata(map[string]any{"codex": map[string]any{"questionId": q.ID, "role": "user_note", "isSecret": q.IsSecret}})}}
		}
	}

	return acp.ElicitationSchema{Type: acp.ElicitationSchemaTypeObject, Properties: properties, Required: required}
}

func userInputAnswers(questions []userInputQuestion, content map[string]any) userInputResponse {
	out := emptyUserInputResponse()
	ids := questionIDs(questions)
	for _, q := range questions {
		answers := userInputValues(content[q.ID])
		if q.IsOther && len(q.Options) > 0 {
			for _, note := range userInputValues(content[userInputNoteFieldID(q.ID, ids)]) {
				answers = append(answers, userInputNotePrefix+strings.TrimSpace(note))
			}
		}
		if len(answers) > 0 {
			out.Answers[q.ID] = userInputAnswer{Answers: answers}
		}
	}
	return out
}

func userInputValues(value any) []string {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, fmt.Sprint(item))
		}
		return out
	default:
		return []string{fmt.Sprint(v)}
	}
}

// Note fields share the form namespace with question IDs, so avoid collisions.
func userInputNoteFieldID(questionID string, ids map[string]bool) string {
	base := questionID + "_note"
	id := base
	for i := 1; ids[id]; i++ {
		id = fmt.Sprintf("%s%d", base, i)
	}
	return id
}

func questionIDs(questions []userInputQuestion) map[string]bool {
	ids := make(map[string]bool, len(questions))
	for _, q := range questions {
		ids[q.ID] = true
	}
	return ids
}
