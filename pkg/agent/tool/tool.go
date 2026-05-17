package tool

import (
	"context"
	"fmt"
	"math"
)

type Effect string

const (
	EffectReadOnly  Effect = "read_only"
	EffectMutates   Effect = "mutates"
	EffectDangerous Effect = "dangerous"
	EffectDynamic   Effect = "dynamic"
)

func StaticEffect(effect Effect) func(map[string]any) Effect {
	return func(map[string]any) Effect {
		return effect
	}
}

type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Execute     func(ctx context.Context, args map[string]any) (string, error)
	Hidden      bool
	Effect      func(args map[string]any) Effect
}

type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type Elicitation struct {
	Ask     func(ctx context.Context, message string) (string, error)
	Confirm func(ctx context.Context, message string) (bool, error)
}

func IntValue(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		if v > int64(math.MaxInt) || v < int64(math.MinInt) {
			return 0, false
		}
		return int(v), true
	case float64:
		if v > float64(math.MaxInt) || v < float64(math.MinInt) {
			return 0, false
		}
		if math.Trunc(v) != v {
			return 0, false
		}
		return int(v), true
	default:
		return 0, false
	}
}

func IntArg(args map[string]any, key string) (int, bool) {
	return IntValue(args[key])
}

func OptionalIntArg(args map[string]any, key string) (int, bool, error) {
	raw, present := args[key]
	if !present {
		return 0, false, nil
	}

	value, ok := IntValue(raw)
	if !ok {
		return 0, true, fmt.Errorf("%s must be an integer", key)
	}

	return value, true, nil
}
