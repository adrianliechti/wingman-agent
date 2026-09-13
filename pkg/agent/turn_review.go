package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/adrianliechti/wingman-agent/pkg/agent/tool"
)

const ChildReviewsMetadata = "child_turn_reviews"

type ValidationCheck struct {
	Command  string `json:"command"`
	WorkDir  string `json:"workdir,omitempty"`
	Outcome  string `json:"outcome"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Sequence uint64 `json:"-"`
}
type TurnReview struct {
	ID         string            `json:"id"`
	InputID    string            `json:"inputId"`
	Started    time.Time         `json:"started"`
	Outcome    string            `json:"outcome"`
	Files      []tool.FileChange `json:"files"`
	Checks     []ValidationCheck `json:"checks"`
	Validation string            `json:"validation"`
	Untracked  bool              `json:"untracked"`
	Uncertain  bool              `json:"uncertain"`
	Undone     bool              `json:"undone"`
	lastEdit   uint64
}

// Reviews use the canonical ledger, so compaction cannot erase attribution.
// Validation requires an explicitly designated check and a recorded exit
// status. Model claims, tool success text and background starts are not proof.
func BuildTurnReviews(events []RuntimeEvent) []TurnReview {
	reviews := []TurnReview{}
	index := map[string]int{}
	current := -1
	executions := map[int]uint64{}
	// A ledger message can contain several results, including child reviews.
	// Preserve their order even when they share an event sequence number.
	var position uint64
	for _, event := range events {
		switch event.Type {
		case EventTurnStarted:
			clear(executions)
			current = len(reviews)
			index[event.TurnID] = current
			reviews = append(reviews, TurnReview{ID: event.TurnID, Started: event.At, Outcome: "running", Files: []tool.FileChange{}, Checks: []ValidationCheck{}, Validation: "not_run"})
		case EventTurnUndo:
			if i, ok := index[event.TurnID]; ok {
				reviews[i].Undone = true
			}
		case EventTurnTerminal:
			if i, ok := index[event.TurnID]; ok && event.Terminal != nil {
				reviews[i].Outcome = string(event.Terminal.Status)
				reviews[i].Uncertain = reviews[i].Uncertain || event.Terminal.OutcomeUncertain
			}
		case EventToolTerminal:
			if i, ok := index[event.TurnID]; ok && event.Terminal != nil {
				reviews[i].Uncertain = reviews[i].Uncertain || event.Terminal.OutcomeUncertain
			}
		case EventMessage:
			if current < 0 || event.Message == nil {
				continue
			}
			review := &reviews[current]
			if review.InputID == "" && event.Message.Role == RoleUser && !event.Message.Hidden {
				review.InputID = event.Message.InputID
			}
			for _, content := range event.Message.Content {
				result := content.ToolResult
				if result == nil {
					continue
				}
				position++
				if result.Name == "agent" && result.Metadata[ChildReviewsMetadata] != nil {
					data, err := json.Marshal(result.Metadata[ChildReviewsMetadata])
					var children []TurnReview
					if err != nil || json.Unmarshal(data, &children) != nil {
						review.Uncertain = true
						continue
					}
					for _, child := range children {
						position++
						review.Files = append(review.Files, child.Files...)
						if len(child.Files) > 0 {
							review.lastEdit = position
						}
						review.Uncertain = review.Uncertain || child.Uncertain
						review.Untracked = review.Untracked || child.Untracked
						for _, check := range child.Checks {
							check.Sequence = position
							if child.Validation == "outdated" && check.Outcome == "passed" {
								check.Outcome = "outdated"
							}
							review.addCheck(check)
						}
					}
					continue
				}
				if result.Name == "edit" {
					if changes, err := tool.ResultFileChanges(result.Metadata); err != nil {
						review.Uncertain = true
					} else if len(changes) > 0 {
						review.Files = append(review.Files, changes...)
						review.lastEdit = position
					}
				}
				var args map[string]any
				_ = json.Unmarshal([]byte(result.Args), &args)
				if result.Name == "exec_command" || result.Name == "exec_session" {
					review.Untracked = true
					sequence := position
					id, known := tool.IntArg(result.Metadata, "session_id")
					if result.Name == "exec_command" {
						if known {
							executions[id] = position
						}
					} else {
						if !known {
							id, _ = tool.IntArg(args, "session_id")
						}
						// Polling cannot make a pre-edit check current. Unknown
						// launches conservatively predate this turn's edits.
						sequence = executions[id]
					}
					validation, _ := args["validation"].(bool)
					if marked, _ := result.Metadata["validation"].(bool); marked {
						validation = true
					}
					if !validation {
						continue
					}
					command, _ := args["command"].(string)
					if value, _ := result.Metadata["command"].(string); value != "" {
						command = value
					}
					workdir, _ := args["workdir"].(string)
					if value, _ := result.Metadata["workdir"].(string); value != "" {
						workdir = value
					}
					check := ValidationCheck{Command: command, WorkDir: workdir, Outcome: "not_confirmed", Sequence: sequence}
					if result.IsError {
						check.Outcome = "failed"
					}
					if value, ok := result.Metadata["exit_code"]; ok {
						data, _ := json.Marshal(value)
						var code *int
						if json.Unmarshal(data, &code) == nil && code != nil {
							check.ExitCode = code
							check.Outcome = "passed"
							if *code != 0 {
								check.Outcome = "failed"
							}
						}
					}
					if check.Outcome == "passed" && sequence == 0 {
						check.Outcome = "not_confirmed"
					}
					review.addCheck(check)
				} else if result.Name != "edit" && result.Name != "read" && result.Name != "ls" && result.Name != "glob" && result.Name != "grep" {
					review.Untracked = true
				}
			}
		}
	}
	for i := range reviews {
		r := &reviews[i]
		if len(r.Checks) == 0 {
			continue
		}
		var failed, unconfirmed, outdated bool
		for j := range r.Checks {
			check := &r.Checks[j]
			if check.Outcome == "passed" && check.Sequence < r.lastEdit {
				check.Outcome = "outdated"
			}
			switch check.Outcome {
			case "failed":
				failed = true
			case "outdated":
				outdated = true
			case "passed":
			default:
				unconfirmed = true
			}
		}
		switch {
		case failed:
			r.Validation = "failed"
		case unconfirmed:
			r.Validation = "not_confirmed"
		case outdated:
			r.Validation = "outdated"
		default:
			r.Validation = "passed"
		}
	}
	return reviews
}

// The latest attempt wins whether the command ran here or in an inline child.
func (r *TurnReview) addCheck(check ValidationCheck) {
	for i := range r.Checks {
		if r.Checks[i].Command == check.Command && r.Checks[i].WorkDir == check.WorkDir {
			if r.Checks[i].Sequence > check.Sequence {
				return // An older background process finished after a newer attempt.
			}
			r.Checks[i] = check
			return
		}
	}
	r.Checks = append(r.Checks, check)
}

func (a *Agent) TurnReviews() []TurnReview {
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	a.ensureStateLocked()
	return BuildTurnReviews(a.Events)
}
func (a *Agent) RecordTurnUndo(id string) error {
	return a.recordEvents(RuntimeEvent{Type: EventTurnUndo, TurnID: id}, RuntimeEvent{Type: EventMessage, Message: &Message{Role: RoleUser, Hidden: true, Content: []Content{{Text: fmt.Sprintf("The user undid the tracked file edits from turn %s. Re-read affected files before continuing.", id)}}}})
}
