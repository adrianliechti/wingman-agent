package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/google/uuid"
)

type asyncQuestion struct {
	Title   string   `json:"title"`
	Options []string `json:"options"`
}

type asyncQuestionReply struct {
	Answer         string `json:"answer"`
	Question       string `json:"question"`
	QuestionItemID string `json:"questionItemId"`
}

const questionReplyStart = "<send_user_message_question_reply>"
const questionReplyEnd = "</send_user_message_question_reply>"

// Async questions belong to the backend turn that asked them. Its context
// closes the form; ExpectedTurnID prevents a late answer reaching another turn.
func (a *approver) askAsyncQuestions(cc *codexClient, turnID, itemID string, questions []asyncQuestion) {
	if a.client.Elicitation == nil || a.client.Elicitation.Form == nil || len(questions) == 0 {
		return
	}
	form := make([]userInputQuestion, len(questions))
	for i, q := range questions {
		form[i] = userInputQuestion{ID: fmt.Sprintf("question_%d", i), Question: q.Title, IsOther: len(q.Options) > 0}
		for _, option := range q.Options {
			form[i].Options = append(form[i].Options, userInputOption{Label: option})
		}
	}
	schema := userInputSchema(form)
	schema.Required = nil // Each question can be answered independently.
	meta := a.elicitationMeta(nil)
	meta["codex"] = map[string]any{"delivery": "async"}
	response, err := callClient(a.ctx, a.conn, func() (acp.UnstableCreateElicitationResponse, error) {
		return a.conn.UnstableCreateElicitation(a.ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{
			Mode: "form", Meta: meta, Message: "Codex keeps working while you answer. Answers are delivered only while this turn is running.", RequestedSchema: schema,
		}})
	})
	if err != nil || a.ctx.Err() != nil || response.Accept == nil {
		return
	}
	answers := userInputAnswers(form, response.Accept.Content)
	var replies []asyncQuestionReply
	for i, q := range form {
		var parts []string
		for _, answer := range answers.Answers[q.ID].Answers {
			if answer == userInputOtherOption {
				continue
			}
			parts = append(parts, strings.TrimPrefix(answer, userInputNotePrefix))
		}
		if len(parts) == 0 {
			continue
		}
		questionID, _ := json.Marshal([]any{"request_user_input_async", itemID, i})
		title := []rune(q.Question)
		title = title[:min(len(title), 512)]
		replies = append(replies, asyncQuestionReply{Answer: strings.Join(parts, "\n"), Question: strings.NewReplacer("\n", " ", "\r", " ").Replace(string(title)), QuestionItemID: string(questionID)})
	}
	if len(replies) == 0 {
		return
	}
	body, _ := json.Marshal(replies)
	text := questionReplyStart + "\n" + string(body) + "\n" + questionReplyEnd
	for _, reply := range replies {
		if len(reply.QuestionItemID) > 512 {
			// Codex rejects oversized identifiers in the structured envelope.
			text = renderQuestionReply(text)
			break
		}
	}
	_ = cc.turnSteer(a.ctx, turnSteerParams{ThreadID: string(a.sessionID), ExpectedTurnID: turnID, ClientUserMessageID: uuid.NewString(), Input: promptToInput([]acp.ContentBlock{acp.TextBlock(text)})})
}

func renderQuestionReply(text string) string {
	body, ok := strings.CutPrefix(strings.TrimSpace(text), questionReplyStart)
	if !ok {
		return text
	}
	body, ok = strings.CutSuffix(body, questionReplyEnd)
	if !ok {
		return text
	}
	var replies []asyncQuestionReply
	if json.Unmarshal([]byte(body), &replies) != nil {
		var reply asyncQuestionReply
		if json.Unmarshal([]byte(body), &reply) != nil {
			return text
		}
		replies = []asyncQuestionReply{reply}
	}
	if len(replies) == 0 {
		return text
	}
	var display []string
	for _, r := range replies {
		if r.QuestionItemID == "" || r.Question == "" || r.Answer == "" {
			return text
		}
		display = append(display, "> "+r.Question+"\n\n"+r.Answer)
	}
	return strings.Join(display, "\n\n")
}

// Keep the asynchronous callback outside the event loop so the app-server can
// finish its turn while the client still has a form open.
func asyncQuestionHandler(ctx context.Context, approver *approver, cc *codexClient, stream *turnStream) func(string, []asyncQuestion) {
	return func(itemID string, questions []asyncQuestion) {
		if ctx.Err() == nil {
			go approver.askAsyncQuestions(cc, stream.turnID, itemID, questions)
		}
	}
}
