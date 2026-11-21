package codemode

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/harness"
	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

type liveIssue struct {
	Number   int      `json:"number"`
	Title    string   `json:"title"`
	State    string   `json:"state"`
	Labels   []string `json:"labels"`
	Assignee string   `json:"assignee"`
	Comments int      `json:"comments"`
	Body     string   `json:"body"`
}

// liveIssues is a fixed tracker: 240 issues, 60 to a page.
func liveIssues() []liveIssue {
	r := rand.New(rand.NewSource(7))
	people := []string{"anna", "bruno", "chiara", "dario", "elena", "fabio", "gina", "hugo"}
	nouns := []string{"login", "export", "search", "billing", "sync", "editor", "upload", "report", "cache", "webhook"}
	verbs := []string{"fails", "is slow", "crashes", "shows stale data", "ignores settings", "times out", "duplicates entries"}
	out := make([]liveIssue, 0, 240)
	for n := 1; n <= 240; n++ {
		issue := liveIssue{
			Number: n, State: "open", Labels: []string{},
			Title:    nouns[r.Intn(len(nouns))] + " " + verbs[r.Intn(len(verbs))],
			Assignee: people[r.Intn(len(people))],
			Comments: r.Intn(13),
		}
		if r.Intn(100) < 40 {
			issue.State = "closed"
		}
		for _, label := range []string{"bug", "feature", "docs", "perf", "ui"} {
			if r.Intn(100) < 30 {
				issue.Labels = append(issue.Labels, label)
			}
		}
		issue.Body = fmt.Sprintf("Steps to reproduce: open the %s page, perform the usual action and observe that it %s. Expected: the action completes. Seen on build 2026.%d by %d customers. Workaround: none known.",
			issue.Title, verbs[r.Intn(len(verbs))], 100+r.Intn(900), 1+r.Intn(40))
		out = append(out, issue)
	}
	return out
}

func liveTrackerTools(issues []liveIssue) []tool.Tool {
	issueSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"number": map[string]any{"type": "integer"}, "title": str, "state": str, "assignee": str,
			"labels": map[string]any{"type": "array", "items": str}, "comments": map[string]any{"type": "integer"}, "body": str,
		},
		"required": []any{"number", "title", "state", "labels", "assignee", "comments", "body"},
	}
	text := func(v any) tool.Result {
		raw, _ := json.Marshal(v)
		return tool.Text(string(raw))
	}
	return []tool.Tool{{
		Name: "tracker_list_issues", Description: "List issues, 60 per page, oldest first. Filters by state only.",
		Effect: tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"state": map[string]any{"type": "string", "enum": []any{"open", "closed"}},
			"page":  map[string]any{"type": "integer", "description": "Page number, starting at 1."},
		}},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"issues":    map[string]any{"type": "array", "items": issueSchema},
				"next_page": map[string]any{"type": "integer", "description": "0 on the last page."},
				"total":     map[string]any{"type": "integer"},
			},
			"required": []any{"issues", "next_page", "total"},
		},
		Execute: func(_ context.Context, args map[string]any) (tool.Result, error) {
			state, _ := args["state"].(string)
			var matched []liveIssue
			for _, issue := range issues {
				if state == "" || issue.State == state {
					matched = append(matched, issue)
				}
			}
			page, _ := tool.IntArg(args, "page")
			page = max(page, 1)
			start := min((page-1)*60, len(matched))
			end := min(start+60, len(matched))
			next := 0
			if end < len(matched) {
				next = page + 1
			}
			return text(map[string]any{"issues": matched[start:end], "next_page": next, "total": len(matched)}), nil
		},
	}, {
		Name: "tracker_get_issue", Description: "Get one issue by number.",
		Effect: tool.StaticEffect(tool.EffectReadOnly),
		Parameters: map[string]any{"type": "object", "required": []any{"number"}, "properties": map[string]any{
			"number": map[string]any{"type": "integer"},
		}},
		OutputSchema: issueSchema,
		Execute: func(_ context.Context, args map[string]any) (tool.Result, error) {
			number, _ := tool.IntArg(args, "number")
			if number < 1 || number > len(issues) {
				return tool.Error(fmt.Sprintf("issue %d not found", number)), nil
			}
			return text(issues[number-1]), nil
		},
	}}
}

// Opt-in comparison of plain tool calls with the code tool on a task whose
// intermediate results are large. All tools are local test fixtures.
// WINGMAN_URL=http://localhost:4242 WINGMAN_LIVE_CODEMODE_MODELS=gpt-6.2-sol,claude-sonnet-5-5
// go test ./pkg/harness/tool/codemode -run '^TestLiveCodeMode$' -v -count=1
func TestLiveCodeMode(t *testing.T) {
	models := strings.FieldsFunc(os.Getenv("WINGMAN_LIVE_CODEMODE_MODELS"), func(r rune) bool { return r == ',' })
	if len(models) == 0 {
		t.Skip("set WINGMAN_LIVE_CODEMODE_MODELS to run real inference")
	}
	if os.Getenv("WINGMAN_URL") == "" {
		t.Fatal("set WINGMAN_URL explicitly for live code mode tests")
	}
	issues := liveIssues()
	bugs, comments := map[string]int{}, map[string]int{}
	top := ""
	for _, issue := range issues {
		if issue.State != "open" || !strings.Contains(strings.Join(issue.Labels, ","), "bug") {
			continue
		}
		bugs[issue.Assignee]++
		comments[issue.Assignee] += issue.Comments
		if bugs[issue.Assignee] > bugs[top] {
			top = issue.Assignee
		}
	}

	for _, model := range models {
		model = strings.TrimSpace(model)
		for _, mode := range []string{"direct", "code"} {
			t.Run(model+"/"+mode, func(t *testing.T) {
				t.Parallel()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()
				cfg, err := harness.ConfigFromEnvironment()
				if err != nil {
					t.Fatal(err)
				}
				cfg.Model = func() string { return model }
				cfg.Effort = func() string { return "low" }
				cfg.MaxTurns = 16
				cfg.ContextWindow = -1
				cfg.Instructions = func() string { return "Answer from actual tool results. Be brief." }
				catalog := liveTrackerTools(issues)
				if mode == "code" {
					catalog = append(catalog, codeTool)
				}
				cfg.Tools = func() []tool.Tool { return Annotate(catalog) }

				a := &harness.Agent{Config: cfg}
				started := time.Now()
				stream, err := a.Send(ctx, []harness.Content{{Text: "Which assignee has the most open issues labeled 'bug'? Give the name, how many such issues they have, and the total number of comments on those issues."}})
				if err != nil {
					t.Fatal(err)
				}
				for _, err := range stream {
					if err != nil && mode == "direct" {
						t.Logf("RESULT model=%s mode=direct wrong: %v", model, err)
						return
					}
					if err != nil {
						t.Fatal(err)
					}
				}

				calls, failed, requests, reply := map[string]int{}, 0, 0, ""
				for _, message := range a.MessagesSnapshot() {
					for _, content := range message.Content {
						switch {
						case content.ToolCall != nil:
							calls[content.ToolCall.Name]++
						case content.ToolResult != nil && content.ToolResult.IsError:
							failed++
							t.Logf("failed %s %.1500s\n-> %.600s", content.ToolResult.Name, content.ToolResult.Args, content.ToolResult.Content.Text())
						case content.Text != "" && message.Role == harness.RoleAssistant:
							reply = content.Text
						}
					}
				}
				for _, event := range a.Events {
					if event.Type == harness.EventRunStarted {
						requests++
					}
				}
				usage := a.UsageSnapshot()
				t.Logf("RESULT model=%s mode=%s requests=%d input=%d output=%d calls=%v failed=%d seconds=%.0f",
					model, mode, requests, usage.InputTokens, usage.OutputTokens, calls, failed, time.Since(started).Seconds())
				for _, want := range []string{top, fmt.Sprint(bugs[top]), fmt.Sprint(comments[top])} {
					if strings.Contains(strings.ToLower(reply), want) {
						continue
					}
					// Counting 164 issues by reading them is where plain calls go
					// wrong; that is the baseline's result, not a failure here.
					if mode == "direct" {
						t.Logf("RESULT model=%s mode=direct wrong: reply lacks %q: %s", model, want, reply)
						continue
					}
					t.Errorf("reply lacks %q: %s", want, reply)
				}
			})
		}
	}
}
