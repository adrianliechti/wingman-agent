package elicit

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/harness/tool"
)

const (
	maxQuestions = 4
	minOptions   = 2
	maxOptions   = 4
)

func Tools(elicit *tool.Elicitation) []tool.Tool {
	if elicit == nil || elicit.Elicit == nil {
		return nil
	}

	description := strings.Join([]string{
		"Ask the user one or more questions and wait for their answers. Blocks until they reply.",
		"- Use only when blocked on a decision that is genuinely the user's — product intent, ambiguous requirements, a preference the workspace cannot answer. If being wrong is cheap to correct, assume and proceed instead.",
		"- 1-4 questions per call, usually with 2-4 mutually exclusive `options` with one-line tradeoff descriptions. Put a recommended option first, labeled \"(Recommended)\". When asking several questions, give each a short `header` chip (max ~12 chars) so answers stay attributable; omit `header` for a single question.",
		"- The UI always adds a free-text \"Other\" reply — never add such an option; options are shortcuts, not constraints. Omit `options` entirely for open-ended questions.",
		"- `multi_select` allows several picks (the answer becomes a list); `preview` (single-select only) shows code/mockups in clients that support previews.",
		"- Not for tool-execution confirmations (handled by the harness).",
	}, "\n")

	return []tool.Tool{{
		Name:        "elicit",
		Description: description,
		Effect:      tool.StaticEffect(tool.EffectReadOnly),

		Parameters: map[string]any{
			"type": "object",

			"properties": map[string]any{
				"questions": map[string]any{
					"type":        "array",
					"minItems":    1,
					"maxItems":    maxQuestions,
					"description": "The questions to ask the user (1-4).",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"question": map[string]any{"type": "string", "description": "The full question text."},
							"header":   map[string]any{"type": "string", "description": "Chip label, e.g. \"Auth method\"; used to attribute answers when asking several questions. Omit for a single question."},
							"options": map[string]any{
								"type":        "array",
								"minItems":    minOptions,
								"maxItems":    maxOptions,
								"description": "The choices (2-4). Omit for a free-form question.",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"label":       map[string]any{"type": "string", "description": "Concise display text (1-5 words); returned verbatim when chosen."},
										"description": map[string]any{"type": "string", "description": "What choosing this means; its tradeoff."},
										"preview":     map[string]any{"type": "string", "description": "Optional code/mockup shown while focused."},
									},
									"required":             []string{"label", "description"},
									"additionalProperties": false,
								},
							},
							"multi_select": map[string]any{"type": "boolean", "description": "Allow selecting multiple options.", "default": false},
						},
						"required":             []string{"question"},
						"additionalProperties": false,
					},
				},
			},

			"required":             []string{"questions"},
			"additionalProperties": false,
		},

		Execute: func(ctx context.Context, args map[string]any) (tool.Result, error) {
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}

			questions, ok := args["questions"].([]any)

			if !ok || len(questions) == 0 {
				return tool.Result{}, fmt.Errorf("questions is required")
			}
			if len(questions) > maxQuestions {
				return tool.Result{}, fmt.Errorf("at most %d questions per call; split the rest into a follow-up", maxQuestions)
			}

			fields, err := parseQuestions(questions)
			if err != nil {
				return tool.Result{}, err
			}

			req := tool.ElicitRequest{Fields: slices.Clone(fields)}
			if len(req.Fields) == 1 {
				// A single question reads best as the prompt message itself.
				req.Message = req.Fields[0].Description
				req.Fields[0].Description = ""
			}

			result, err := elicit.Elicit(ctx, req)

			if err != nil {
				return tool.Result{}, err
			}
			if err := ctx.Err(); err != nil {
				return tool.Result{}, err
			}

			switch result.Action {
			case tool.ElicitAccept:
				return tool.Text(renderAnswers(fields, result.Content)), nil
			case tool.ElicitDecline:
				return tool.Text("The user declined to answer. Do not infer an answer or repeat the same questions; continue only work that does not depend on this input."), nil
			case tool.ElicitCancel:
				return tool.Text("The user dismissed the questions without answering."), nil
			default:
				return tool.Result{}, fmt.Errorf("invalid elicitation response action %q", result.Action)
			}
		},

		Hidden: true,
	}}
}

func parseQuestions(raw []any) ([]tool.ElicitField, error) {
	fields := make([]tool.ElicitField, 0, len(raw))
	seen := map[string]bool{}

	for i, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("questions[%d] must be an object", i)
		}

		question, _ := entry["question"].(string)
		if strings.TrimSpace(question) == "" {
			return nil, fmt.Errorf("questions[%d].question is required", i)
		}

		var header string
		if rawHeader, present := entry["header"]; present {
			h, ok := rawHeader.(string)
			if !ok {
				return nil, fmt.Errorf("questions[%d].header must be a string", i)
			}
			header = strings.TrimSpace(h)
		}

		title := header
		if title == "" {
			title = truncateQuestion(question, 40)
		}

		name := slugify(header)
		if name == "" {
			name = fmt.Sprintf("q_%d", i+1)
		}
		base := name
		for suffix := i + 1; seen[name]; suffix++ {
			name = fmt.Sprintf("%s_%d", base, suffix)
		}
		seen[name] = true

		var multiple bool
		if rawMulti, present := entry["multi_select"]; present {
			m, ok := rawMulti.(bool)
			if !ok {
				return nil, fmt.Errorf("questions[%d].multi_select must be a boolean", i)
			}
			multiple = m
		}

		field := tool.ElicitField{
			Name:        name,
			Type:        "string",
			Title:       title,
			Description: strings.TrimSpace(question),
			Required:    true,
			Multiple:    multiple,
		}

		if rawOptionsVal, present := entry["options"]; present {
			rawOptions, ok := rawOptionsVal.([]any)
			if !ok {
				return nil, fmt.Errorf("questions[%d].options must be an array", i)
			}
			if len(rawOptions) < minOptions || len(rawOptions) > maxOptions {
				return nil, fmt.Errorf("questions[%d].options must contain %d-%d options; omit options for free text", i, minOptions, maxOptions)
			}
			for j, rawOption := range rawOptions {
				option, ok := rawOption.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("questions[%d].options[%d] must be an object with a label", i, j)
				}
				label, _ := option["label"].(string)
				label = strings.TrimSpace(label)
				if label == "" {
					return nil, fmt.Errorf("questions[%d].options[%d].label is required", i, j)
				}
				if slices.ContainsFunc(field.Enum, func(existing string) bool { return strings.EqualFold(existing, label) }) {
					return nil, fmt.Errorf("questions[%d].options[%d] duplicates label %q", i, j, label)
				}
				optionDescription, ok := option["description"].(string)
				if !ok || strings.TrimSpace(optionDescription) == "" {
					return nil, fmt.Errorf("questions[%d].options[%d].description must be a non-empty string", i, j)
				}
				var preview string
				if rawPreview, present := option["preview"]; present {
					preview, ok = rawPreview.(string)
					if !ok {
						return nil, fmt.Errorf("questions[%d].options[%d].preview must be a string", i, j)
					}
					if multiple {
						return nil, fmt.Errorf("questions[%d].options[%d].preview requires single selection", i, j)
					}
				}

				field.Enum = append(field.Enum, label)
				field.EnumDescriptions = append(field.EnumDescriptions, strings.TrimSpace(optionDescription))
				field.EnumPreviews = append(field.EnumPreviews, preview)
			}
		}
		if multiple && len(field.Enum) == 0 {
			return nil, fmt.Errorf("questions[%d].multi_select requires options", i)
		}

		fields = append(fields, field)
	}

	return fields, nil
}

func renderAnswers(fields []tool.ElicitField, content map[string]any) string {
	if len(fields) == 1 {
		answer := answerText(content[fields[0].Name])
		if answer == "" {
			return "The user submitted an empty answer."
		}
		return answer
	}

	var b strings.Builder
	b.WriteString("User answers:")
	for _, field := range fields {
		answer := answerText(content[field.Name])
		if answer == "" {
			answer = "(no answer)"
		}
		fmt.Fprintf(&b, "\n- %s: %s", field.Title, answer)
	}
	return b.String()
}

func answerText(value any) string {
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case []string:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
		return strings.Join(parts, ", ")
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if item == nil {
				continue
			}
			if trimmed := strings.TrimSpace(fmt.Sprintf("%v", item)); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
		return strings.Join(parts, ", ")
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func truncateQuestion(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return strings.TrimSpace(string(runes[:max])) + "…"
}

func slugify(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case r == ' ', r == '-', r == '_':
			if b.Len() > 0 && !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}
