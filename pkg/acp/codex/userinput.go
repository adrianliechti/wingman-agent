package codex

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
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
	meta := a.elicitationMeta(nil)
	meta["toolCallId"] = p.ItemID
	meta["codex"] = map[string]any{"autoResolutionMs": p.AutoResolutionMs}
	resp, err := callClient(ctx, a.conn, func() (acp.UnstableCreateElicitationResponse, error) {
		return a.conn.UnstableCreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
			Form: &acp.UnstableCreateElicitationForm{
				Meta:            meta,
				Message:         "Codex needs your input to continue.",
				Mode:            "form",
				RequestedSchema: userInputSchema(p.Questions),
			},
		})
	})
	if err != nil || ctx.Err() != nil || resp.Accept == nil {
		return emptyUserInputResponse()
	}
	return userInputAnswers(p.Questions, resp.Accept.Content)
}

func userInputSchema(questions []userInputQuestion) acp.UnstableElicitationSchema {
	ids := questionIDs(questions)
	properties := map[string]any{}
	required := make([]string, 0, len(questions))
	for _, q := range questions {
		title := q.Question
		if title == "" {
			title = q.Header
		}
		if title == "" {
			title = q.ID
		}
		field := map[string]any{
			"type":  "string",
			"title": title,
			"_meta": map[string]any{"codex": map[string]any{"isOther": q.IsOther, "isSecret": q.IsSecret}},
		}
		if q.Header != "" {
			field["description"] = q.Header
		}
		if len(q.Options) > 0 {
			oneOf := make([]any, 0, len(q.Options)+1)
			for _, option := range q.Options {
				choice := map[string]any{"const": option.Label, "title": option.Label}
				if option.Description != "" {
					choice["description"] = option.Description
				}
				oneOf = append(oneOf, choice)
			}
			hasOther := slices.ContainsFunc(q.Options, func(o userInputOption) bool { return o.Label == userInputOtherOption })
			if q.IsOther && !hasOther {
				oneOf = append(oneOf, map[string]any{
					"const": userInputOtherOption, "title": userInputOtherOption,
					"description": "Provide a different answer in the note field.",
				})
			}
			field["oneOf"] = oneOf
		}
		properties[q.ID] = field
		required = append(required, q.ID)
		if q.IsOther && len(q.Options) > 0 {
			properties[userInputNoteFieldID(q.ID, ids)] = map[string]any{
				"type":  "string",
				"title": "Additional answer or note",
				"_meta": map[string]any{"codex": map[string]any{"questionId": q.ID, "role": "user_note", "isSecret": q.IsSecret}},
			}
		}
	}
	return acp.UnstableElicitationSchema{Type: acp.UnstableElicitationSchemaTypeObject, Properties: properties, Required: required}
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
