package external

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var placeholderPattern = regexp.MustCompile(`\$\{([^{}]+)\}`)

// callMCP expands the handler's input template against the event and treats
// the tool's text like a command hook's stdout.
func (h Handler) callMCP(ctx context.Context, input []byte, caller MCPCaller) runResult {
	if caller == nil {
		return runResult{exitCode: -1, err: errors.New("MCP tools are not available to hooks")}
	}
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	var event any
	if err := decoder.Decode(&event); err != nil {
		return runResult{exitCode: -1, err: fmt.Errorf("parse hook event input: %w", err)}
	}
	arguments, err := expandTemplate(h.Input, event)
	if err != nil {
		return runResult{exitCode: -1, err: err}
	}
	text, err := caller(ctx, h.Server, h.Tool, arguments)
	if err != nil {
		return runResult{exitCode: -1, err: err}
	}
	return runResult{exitCode: 0, stdout: limit(text), stdoutTruncated: len(text) > maxHookOutput}
}

// expandTemplate substitutes ${field.nested} placeholders from the hook event.
// A placeholder that is the whole string keeps its JSON type; one embedded in
// text is rendered as a string. Missing fields fail the hook.
func expandTemplate(template map[string]any, event any) (map[string]any, error) {
	expanded := make(map[string]any, len(template))
	for key, value := range template {
		resolved, err := expandValue(value, event)
		if err != nil {
			return nil, err
		}
		expanded[key] = resolved
	}
	return expanded, nil
}

func expandValue(value any, event any) (any, error) {
	switch v := value.(type) {
	case map[string]any:
		return expandTemplate(v, event)
	case []any:
		items := make([]any, len(v))
		for i, item := range v {
			resolved, err := expandValue(item, event)
			if err != nil {
				return nil, err
			}
			items[i] = resolved
		}
		return items, nil
	case string:
		return expandString(v, event)
	default:
		return value, nil
	}
}

func expandString(text string, event any) (any, error) {
	matches := placeholderPattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}
	if len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(text) {
		return lookupField(event, text[matches[0][2]:matches[0][3]])
	}
	var out strings.Builder
	previous := 0
	for _, match := range matches {
		out.WriteString(text[previous:match[0]])
		value, err := lookupField(event, text[match[2]:match[3]])
		if err != nil {
			return nil, err
		}
		if s, ok := value.(string); ok {
			out.WriteString(s)
		} else {
			encoded, err := json.Marshal(value)
			if err != nil {
				return nil, err
			}
			out.Write(encoded)
		}
		previous = match[1]
	}
	out.WriteString(text[previous:])
	return out.String(), nil
}

func lookupField(event any, path string) (any, error) {
	value := event
	for field := range strings.SplitSeq(path, ".") {
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("hook input placeholder ${%s} was not found", path)
		}
		if value, ok = object[field]; !ok {
			return nil, fmt.Errorf("hook input placeholder ${%s} was not found", path)
		}
	}
	return value, nil
}
